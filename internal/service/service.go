package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"akeuben.ca/gmessages-dbus/internal/store"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
)

const (
	BusName   = "org.example.GMessages" // rename to a reverse-DNS name you own
	ObjPath   = "/org/example/GMessages"
	Interface = "org.example.GMessages1"
)

type State string

const (
	StateUnpaired   State = "Unpaired"   // no stored credentials
	StatePairing    State = "Pairing"    // waiting for emoji confirmation
	StateConnecting State = "Connecting"
	StateConnected  State = "Connected"
	StateNeedsLogin State = "NeedsLogin" // credentials exist but no longer work
	StateError      State = "Error"      // transient failure, retrying
)

type Service struct {
	log   zerolog.Logger
	store *store.Store

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	conn  *dbus.Conn
	props *prop.Properties

	mu      sync.Mutex // guards everything below
	client  *libgm.Client
	phoneID string
	pairing *pairingState
	state   State
}

func New(log zerolog.Logger, st *store.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		log:     log,
		store:   st,
		rootCtx: ctx,
		cancel:  cancel,
		state:   StateUnpaired,
	}
}

func (s *Service) Start() error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect to session bus: %w", err)
	}
	s.conn = conn

	if err := s.exportObject(); err != nil {
		conn.Close()
		return fmt.Errorf("export object: %w", err)
	}

	reply, err := conn.RequestName(BusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return fmt.Errorf("request name: %w", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return errors.New("another instance already owns " + BusName)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.restoreSession()
	}()
	return nil
}

func (s *Service) exportObject() error {
	a := &api{s: s}
	if err := s.conn.Export(a, ObjPath, Interface); err != nil {
		return err
	}

	props, err := prop.Export(s.conn, ObjPath, map[string]map[string]*prop.Prop{
		Interface: {
			"State":   {Value: string(StateUnpaired), Writable: false, Emit: prop.EmitTrue},
			"Account": {Value: "", Writable: false, Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		return err
	}
	s.props = props

	node := &introspect.Node{
		Name: ObjPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name:       Interface,
				Methods:    introspect.Methods(a),
				Properties: props.Introspection(Interface),
				Signals: []introspect.Signal{
					{Name: "PairingFinished", Args: []introspect.Arg{
						{Name: "ok", Type: "b"},
						{Name: "error", Type: "s"},
					}},
				},
			},
		},
	}
	return s.conn.Export(introspect.NewIntrospectable(node), ObjPath,
		"org.freedesktop.DBus.Introspectable")
}

func (s *Service) restoreSession() {
	rec, err := s.store.Load()
	if err != nil {
		s.log.Err(err).Msg("failed to load stored credentials")
		s.setState(StateNeedsLogin)
		return
	}
	if rec == nil {
		s.setState(StateUnpaired)
		return
	}

	s.mu.Lock()
	s.phoneID = rec.PhoneID
	s.mu.Unlock()
	s.setAccount(rec.Auth.Mobile.GetSourceID())

	backoff := 2 * time.Second
	for {
		s.setState(StateConnecting)
		cli := s.newClient(rec.Auth)
		cli.SetEventHandler(s.handleEvent)

		err := cli.Connect(s.rootCtx)
		if err == nil {
			s.setClient(cli)
			return
		}
		cli.Disconnect()
		if s.rootCtx.Err() != nil {
			return
		}
		if isAuthError(err) {
			s.log.Warn().Err(err).Msg("stored credentials rejected")
			s.setState(StateNeedsLogin)
			return
		}

		s.log.Warn().Err(err).Dur("retry_in", backoff).Msg("connect failed")
		s.setState(StateError)
		select {
		case <-s.rootCtx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (s *Service) adopt(phoneID string, ad *libgm.AuthData) error {
	if err := s.store.Save(phoneID, ad); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	s.mu.Lock()
	s.phoneID = phoneID
	s.mu.Unlock()
	s.setAccount(ad.Mobile.GetSourceID())

	live := s.newClient(ad)
	live.SetEventHandler(s.handleEvent)
	s.setState(StateConnecting)
	if err := live.Connect(s.rootCtx); err != nil {
		s.setState(StateError)
		return err
	}
	s.setClient(live)
	return nil
}

func (s *Service) setClient(cli *libgm.Client) {
	s.mu.Lock()
	old := s.client
	s.client = cli
	s.mu.Unlock()
	if old != nil && old != cli {
		old.Disconnect()
	}
}

func (s *Service) newClient(ad *libgm.AuthData) *libgm.Client {
	return libgm.NewClient(ad, nil,
		s.log.With().Str("component", "libgm").Logger(), exhttp.SensibleClientSettings)
}

func (s *Service) handleEvent(evt any) {
	s.log.Info().Str("event", fmt.Sprintf("%T", evt)).Msg("libgm event") // temporary
	switch e := evt.(type) {
	case *events.ClientReady:
		s.setState(StateConnected)
	case *events.AuthTokenRefreshed:
		s.persist()
	default:
		s.log.Trace().Type("event_type", e).Msg("unhandled libgm event")
	}
}

func (s *Service) persist() {
	s.mu.Lock()
	cli, id := s.client, s.phoneID
	s.mu.Unlock()
	if cli == nil {
		return
	}
	if err := s.store.Save(id, cli.AuthData); err != nil {
		s.log.Err(err).Msg("failed to persist auth data")
	}
}

func (s *Service) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	if s.props != nil {
		s.props.SetMust(Interface, "State", string(st))
	}
}

func (s *Service) setAccount(email string) {
	if s.props != nil {
		s.props.SetMust(Interface, "Account", email)
	}
}

func (s *Service) logout() *dbus.Error {
	s.mu.Lock()
	cli := s.client
	s.client, s.phoneID = nil, ""
	s.mu.Unlock()

	if cli != nil {
		if err := cli.UnpairGaia(s.rootCtx); err != nil {
			s.log.Warn().Err(err).Msg("unpair failed, clearing local credentials anyway")
		}
		cli.Disconnect()
	}
	if err := s.store.Delete(); err != nil {
		return dbus.MakeFailedError(err)
	}
	s.setAccount("")
	s.setState(StateUnpaired)
	return nil
}

func (s *Service) listConversations(limit int64) ([]*gmproto.Conversation, error) {
	request := gmproto.ListConversationsRequest{}
	request.Folder = gmproto.ListConversationsRequest_INBOX
	request.Count = limit 
	s.mu.Lock()
	result, err := s.client.ListConversations(s.rootCtx, &request)
	s.mu.Unlock()
	return result.Conversations, err
}

func (s *Service) Shutdown() {
	s.persist()
	s.cancel()

	s.mu.Lock()
	cli := s.client
	s.client = nil
	s.mu.Unlock()
	s.cancelPairing()

	if cli != nil {
		cli.Disconnect()
	}
	s.wg.Wait()

	if s.conn != nil {
		s.conn.ReleaseName(BusName)
		s.conn.Close()
	}
}

func isAuthError(err error) bool { return false }
