package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
)

type pairingState struct {
	ctx    context.Context
	cancel context.CancelFunc
	prev   State
	client *libgm.Client
	sess   *libgm.PairingSession
}

type pairError struct{ Code, Msg string }

func (e pairError) dbus() *dbus.Error {
	return dbus.NewError(Interface+".Error."+e.Code, []any{e.Msg})
}

func dbusErr(code, msg string) *dbus.Error { return pairError{code, msg}.dbus() }

func classifyPairErr(err error) pairError {
	var reqErr events.RequestError
	switch {
	case errors.Is(err, libgm.ErrNoDevicesFound):
		return pairError{"NoDevices", "No devices found. Make sure account pairing is enabled in the Google Messages app on your phone."}
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		return pairError{"PhoneNotResponding", "Phone not responding. Make sure it's online and account pairing is enabled in Google Messages. You may need to keep the app open and disable battery optimization."}
	case errors.Is(err, events.ErrCallerNoPermission):
		return pairError{"NoPermission", "That Google account doesn't seem to have permission to use Google Messages for Web."}
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		return pairError{"IncorrectEmoji", "Incorrect emoji chosen on the phone, please try again."}
	case errors.Is(err, libgm.ErrPairingCancelled):
		return pairError{"Cancelled", "Pairing was cancelled on the phone."}
	case errors.Is(err, libgm.ErrPairingTimeout), errors.Is(err, context.DeadlineExceeded):
		return pairError{"Timeout", "Pairing timed out, please try again."}
	case errors.Is(err, context.Canceled):
		return pairError{"Aborted", "Pairing was cancelled."}
	case errors.As(err, &reqErr) && reqErr.Data != nil:
		return pairError{"GoogleError", fmt.Sprintf("Unexpected Google error %d: %v", reqErr.Data.Type, err)}
	default:
		return pairError{"Unknown", err.Error()}
	}
}

var cookieNames = []string{"SID", "HSID", "SSID", "OSID", "APISID", "SAPISID", "__Secure-1PSIDTS"}

func pickCookies(all map[string]string) map[string]string {
	out := make(map[string]string, len(cookieNames))
	for _, n := range cookieNames {
		if v := all[n]; v != "" {
			out[n] = v
		}
	}
	return out
}

func (s *Service) startGoogleLogin(cookies string) (string, *dbus.Error) {
	all, err := ParseCookies(cookies)
	if err != nil {
		return "", dbusErr("BadCookies", err.Error())
	}
	ad := libgm.NewAuthData()
	ad.Cookies = pickCookies(all)
	return s.startPairing(ad, false)
}

func (s *Service) repair() (string, *dbus.Error) {
	rec, err := s.store.Load()
	if err != nil {
		return "", dbusErr("NeedsLogin", "could not read stored credentials: "+err.Error())
	}
	if rec == nil || rec.Auth == nil || !rec.Auth.HasCookies() {
		return "", dbusErr("NeedsLogin", "no stored cookies")
	}
	return s.startPairing(rec.Auth, true)
}

func (s *Service) cancelPairing() {
	s.mu.Lock()
	p := s.pairing
	var cli *libgm.Client
	if p != nil {
		cli = p.client
	}
	s.mu.Unlock()

	if p == nil {
		return
	}
	p.cancel()
	if cli != nil {
		cli.Disconnect()
	}
}

func (s *Service) startPairing(ad *libgm.AuthData, repair bool) (string, *dbus.Error) {
	pctx, pcancel := context.WithCancel(s.rootCtx)
	p := &pairingState{ctx: pctx, cancel: pcancel}

	s.mu.Lock()
	if s.pairing != nil {
		s.mu.Unlock()
		pcancel()
		return "", dbusErr("Busy", "a login is already in progress")
	}
	p.prev = s.state
	s.pairing = p
	s.mu.Unlock()

	fail := func(e *dbus.Error) (string, *dbus.Error) {
		s.endPairing(p)
		s.setState(p.prev)
		return "", e
	}

	cli := s.newClient(ad)
	cli.SetEventHandler(func(evt any) {
		s.log.Warn().Type("event_type", evt).Msg("unexpected pre-pairing event")
	})
	s.mu.Lock()
	p.client = cli
	s.mu.Unlock()

	reqCtx, cancelReq := context.WithTimeout(pctx, 30*time.Second)
	defer cancelReq()

	if err := cli.FetchConfig(reqCtx); err != nil {
		if repair {
			s.log.Err(err).Msg("config fetch failed while re-pairing with stored cookies")
			return fail(dbusErr("NeedsLogin", "stored cookies no longer work"))
		}
		if errors.Is(err, context.Canceled) {
			return fail(classifyPairErr(err).dbus())
		}
		return fail(dbusErr("FetchConfigFailed", err.Error()))
	}
	if repair && cli.Config.GetDeviceInfo().GetEmail() == "" {
		return fail(dbusErr("NeedsLogin", "stored cookies are no longer logged in"))
	}

	bgCtx := cli.Logger.WithContext(pctx)
	emoji, sess, err := cli.StartGaiaPairing(reqCtx, bgCtx)
	if err != nil {
		return fail(classifyPairErr(err).dbus())
	}

	s.mu.Lock()
	p.sess = sess
	s.mu.Unlock()
	s.setState(StatePairing)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.finishPairing(p)
	}()
	return emoji, nil
}

func (s *Service) finishPairing(p *pairingState) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Minute)
	defer cancel()

	phoneID, err := p.client.FinishGaiaPairing(ctx, p.sess)
	ad := p.client.AuthData
	s.endPairing(p)

	if err != nil {
		s.setState(p.prev)
		s.emitPairingFinished(false, classifyPairErr(err))
		return
	}

	if err := s.adopt(phoneID, ad); err != nil {
		s.log.Err(err).Msg("pairing succeeded but starting the session failed")
		s.emitPairingFinished(false, pairError{"ConnectFailed", err.Error()})
		return
	}
	s.emitPairingFinished(true, pairError{})
}

func (s *Service) endPairing(p *pairingState) {
	p.cancel()
	s.mu.Lock()
	if s.pairing == p {
		s.pairing = nil
	}
	cli := p.client
	s.mu.Unlock()
	if cli != nil {
		cli.Disconnect()
	}
}

func (s *Service) emitPairingFinished(ok bool, e pairError) {
	if err := s.conn.Emit(ObjPath, Interface+".PairingFinished", ok, e.Code, e.Msg); err != nil {
		s.log.Err(err).Msg("failed to emit PairingFinished")
	}
}
