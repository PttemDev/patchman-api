package handlers

import "github.com/google/uuid"

// pickID returns the client-supplied ID when it parses as a UUID. If clientID is
// empty, a fresh UUIDv4 is generated. If clientID is non-empty but not a valid
// UUID, ok is false — callers should reject the request with 400.
//
// Phase 2 sync lets offline clients push local UUIDs so foreign keys survive a
// register-from-local flow without re-keying.
func pickID(clientID string) (string, bool) {
	if clientID == "" {
		return uuid.New().String(), true
	}
	if _, err := uuid.Parse(clientID); err != nil {
		return "", false
	}
	return clientID, true
}
