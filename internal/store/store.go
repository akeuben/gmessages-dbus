package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/zalando/go-keyring"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

const (
	keyringService = "org.example.GMessages"
	keyringUser    = "auth-data"
	recordVersion  = 1
)

// Record is what gets persisted. PhoneID is the login ID returned by FinishGaiaPairing.
type Record struct {
	Version int             `json:"version"`
	PhoneID string          `json:"phone_id"`
	SavedAt time.Time       `json:"saved_at"`
	Auth    *libgm.AuthData `json:"auth"`
}

type Store struct {
	mu            sync.Mutex
	path          string // file fallback location
	preferKeyring bool
	log           zerolog.Logger
}

// DefaultPath returns $XDG_DATA_HOME/gmessages-dbus/auth.json (or ~/.local/share/...).
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "gmessages-dbus", "auth.json"), nil
}

func New(path string, preferKeyring bool, log zerolog.Logger) *Store {
	return &Store{path: path, preferKeyring: preferKeyring, log: log}
}

// Load returns (nil, nil) when nothing has been saved yet.
func (s *Store) Load() (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.preferKeyring {
		v, err := keyring.Get(keyringService, keyringUser)
		switch {
		case err == nil:
			return decode([]byte(v))
		case errors.Is(err, keyring.ErrNotFound):
			// fall through to the file
		default:
			s.log.Warn().Err(err).Msg("keyring unavailable on load, trying file")
		}
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return decode(data)
}

func (s *Store) Save(phoneID string, ad *libgm.AuthData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(Record{
		Version: recordVersion,
		PhoneID: phoneID,
		SavedAt: time.Now().UTC(),
		Auth:    ad,
	})
	if err != nil {
		return fmt.Errorf("marshal auth data: %w", err)
	}

	if s.preferKeyring {
		err := keyring.Set(keyringService, keyringUser, string(data))
		if err == nil {
			_ = os.Remove(s.path) // don't leave a plaintext copy behind
			return nil
		}
		s.log.Warn().Err(err).Msg("keyring unavailable on save, falling back to file")
	}
	return writeFileAtomic(s.path, data)
}

// Delete removes the credentials from both backends (use on logout/unpair).
func (s *Store) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	if err := keyring.Delete(keyringService, keyringUser); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, err)
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func decode(data []byte) (*Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode auth record: %w", err)
	}
	if rec.Auth == nil {
		return nil, errors.New("auth record has no auth data")
	}
	return &rec, nil
}

// writeFileAtomic writes via temp file + rename so a crash never leaves a half-written
// credentials file. os.CreateTemp already creates the file with mode 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
