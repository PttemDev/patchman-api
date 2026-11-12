// Package cloudsession holds the signed-in user's CLOUD session for an embedded
// (desktop) backend, so embedded-only surfaces that must act on the hosted BE
// as the user — currently the whiteboard MCP tools — can do so.
//
// The session is delivered at runtime by the desktop FE over loopback (it
// already holds the token) and lives ONLY in memory: it is never written to the
// embedded SQLite file or any disk. It is cleared on logout. A stale/expired
// token simply yields a 401 from the cloud and callers fall back to local
// behavior — never a silent write to the wrong account.
package cloudsession

import (
	"strings"
	"sync"
)

// Session is a snapshot of the current cloud session. BaseURL is the hosted BE
// ROOT (scheme+host, no "/api" suffix); Token is the user's cloud JWT.
type Session struct {
	BaseURL string
	Token   string
}

var (
	mu      sync.RWMutex
	current *Session
)

// Set installs the cloud session. A trailing "/api" (the FE's axios baseURL
// shape) is trimmed so callers can append tool paths that already start with
// "/api". Empty baseURL or token clears the session.
func Set(baseURL, token string) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	baseURL = strings.TrimSuffix(baseURL, "/api")
	token = strings.TrimSpace(token)
	mu.Lock()
	defer mu.Unlock()
	if baseURL == "" || token == "" {
		current = nil
		return
	}
	current = &Session{BaseURL: baseURL, Token: token}
}

// Clear drops the cloud session (logout / going local).
func Clear() {
	mu.Lock()
	defer mu.Unlock()
	current = nil
}

// Get returns the current session and whether one is set.
func Get() (Session, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if current == nil {
		return Session{}, false
	}
	return *current, true
}
