package service

import (
	"strings"

	"github.com/godbus/dbus/v5"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func clean(s string) string { return strings.ToValidUTF8(s, "") }

func v(x any) dbus.Variant { return dbus.MakeVariant(x) }

func conversationsToDBus(convs []*gmproto.Conversation) []map[string]dbus.Variant {
	out := make([]map[string]dbus.Variant, 0, len(convs))
	for _, c := range convs {
		if c == nil {
			continue
		}
		out = append(out, conversationToDBus(c))
	}
	return out
}

func conversationToDBus(c *gmproto.Conversation) map[string]dbus.Variant {
	parts := make([]map[string]dbus.Variant, 0, len(c.GetParticipants()))
	for _, p := range c.GetParticipants() {
		if p == nil {
			continue
		}
		number := p.GetFormattedNumber()
		if number == "" {
			number = p.GetID().GetNumber()
		}
		parts = append(parts, map[string]dbus.Variant{
			"id":     v(clean(p.GetID().GetParticipantID())),
			"name":   v(clean(p.GetFullName())),
			"number": v(clean(number)),
			"is_me":  v(p.GetIsMe()),
		})
	}

	return map[string]dbus.Variant{
		"id":             v(clean(c.GetConversationID())),
		"name":           v(clean(displayName(c))),
		"is_group":       v(c.GetIsGroupChat()),
		"unread":         v(c.GetUnread()),
		"last_timestamp": v(c.GetLastMessageTimestamp()),
		"snippet":        v(clean(c.GetLatestMessage().GetDisplayContent())),
		"participants":   v(parts),
	}
}

func displayName(c *gmproto.Conversation) string {
	if n := c.GetName(); n != "" {
		return n
	}
	var names []string
	for _, p := range c.GetParticipants() {
		if p == nil || p.GetIsMe() {
			continue
		}
		n := p.GetFullName()
		if n == "" {
			n = p.GetFormattedNumber()
		}
		if n != "" {
			names = append(names, n)
		}
	}
	return strings.Join(names, ", ")
}
