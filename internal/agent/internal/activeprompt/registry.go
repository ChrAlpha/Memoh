// Package activeprompt keeps the in-process output hub of each external
// runtime prompt that is executing, so a continuation that arrives while the
// prompt runs can follow its events.
package activeprompt

import (
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
)

// Registry holds the Hub of every session whose runtime driver call is in
// flight. The zero value is ready to use.
type Registry struct {
	hubs sync.Map
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{}
}

// Register opens the Hub of a session's prompt, closing the one it replaces.
func (r *Registry) Register(botID, sessionID string) *Hub {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	key := runreq.SessionTurnKey(botID, sessionID)
	hub := newHub()
	previous, replaced := r.hubs.Swap(key, hub)
	if replaced {
		previous.(*Hub).close()
	}
	return hub
}

// Has reports whether a runtime driver call is currently executing for the
// session; the registration brackets the driver Prompt exactly, so this is the
// "driver actually stopped" signal deletion waits on.
func (r *Registry) Has(botID, sessionID string) bool {
	key := runreq.SessionTurnKey(strings.TrimSpace(botID), strings.TrimSpace(sessionID))
	_, ok := r.hubs.Load(key)
	return ok
}

// Unregister closes hub and drops it, unless a newer Hub already replaced it.
func (r *Registry) Unregister(botID, sessionID string, hub *Hub) {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	key := runreq.SessionTurnKey(botID, sessionID)
	r.hubs.CompareAndDelete(key, hub)
	hub.close()
}

// Subscribe follows the Hub of a session's running prompt. The bool is false
// when no prompt is running or its Hub has closed.
func (r *Registry) Subscribe(botID, sessionID string) (*Subscription, bool) {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	key := runreq.SessionTurnKey(botID, sessionID)
	value, ok := r.hubs.Load(key)
	if !ok {
		return nil, false
	}
	return value.(*Hub).subscribe()
}
