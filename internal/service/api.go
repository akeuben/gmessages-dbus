package service

import (
	"github.com/godbus/dbus/v5"
)

type api struct{ s *Service }

func (a *api) StartGoogleLogin(cookies string) (string, *dbus.Error) {
	return a.s.startGoogleLogin(cookies)
}

func (a *api) Repair() (string, *dbus.Error) { return a.s.repair() }

func (a *api) CancelPairing() *dbus.Error {
	a.s.cancelPairing()
	return nil
}

func (a *api) Logout() *dbus.Error { return a.s.logout() }

func (a *api) GetConversations(limit int64) ([]map[string] dbus.Variant, *dbus.Error) {
	convs, err := a.s.listConversations(limit)
	if err != nil {
		return nil, dbus.MakeFailedError(err)
	}
	return conversationsToDBus(convs), nil
} 
