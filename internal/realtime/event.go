// Package realtime delivers mutation notifications from the BE to every
// connected client in near-real-time. The transport is WebSocket; cross-pod
// fan-out goes through Postgres LISTEN/NOTIFY so any of the (currently 3)
// replicas can originate a write and every other replica's WS clients still
// hear about it.
//
// On the FE the contract is intentionally dumb: receive an Event, refresh
// the matching store. Surgical diffing is a future optimization — the user
// asked for "tam refresh" so they could verify the end-to-end loop works
// before we put effort into payload-based reducers.
package realtime

// Event is the payload that crosses the wire between BE and FE. Keep it
// flat + JSON-friendly; the FE switches on `type` and re-fetches the
// matching store. `recipients` only travels over the cross-pod channel
// (NOTIFY payload) — it's stripped before the per-client send because a
// recipient list leaks who else has access.
type Event struct {
	// Type identifies what changed. Use dotted lowercase strings so a FE
	// switch can do prefix matches (`collection.*`, `request.*`) when
	// reducers go beyond "always refresh".
	Type string `json:"type"`

	// CollectionID / EnvironmentID identify the affected entity. Only one
	// of these is populated per event; nested events (a request edit)
	// always carry the *root* collection id so the FE can scope its
	// invalidation to one tree.
	CollectionID  string `json:"collectionId,omitempty"`
	EnvironmentID string `json:"environmentId,omitempty"`
	// NoteID is the affected note (not its root): the FE refreshes the notes
	// tree and reloads that note if it's open.
	NoteID string `json:"noteId,omitempty"`

	// Recipients carries the user ids that should receive this event.
	// Used only on the cross-pod channel and inside the hub fan-out; the
	// per-client message struct (sentEvent) does NOT include it.
	Recipients []string `json:"recipients,omitempty"`

	// Everyone signals "broadcast to all connected clients". Used when a
	// public collection has an `everyone` viewer grant — listing every
	// user would be expensive and stale. Recipients takes precedence
	// when both are set (defensive; production paths should set one or
	// the other).
	Everyone bool `json:"everyone,omitempty"`

	// OriginClientID echoes the X-Client-Id header from the request
	// that produced this mutation. The FE checks it against its own
	// per-tab id and suppresses the refresh on a self-echo (the
	// mutation's promise chain already refreshed locally). Empty when
	// the mutation came from a path with no client id available.
	OriginClientID string `json:"originClientId,omitempty"`
}

// Canonical event types. Listed here so the FE constants and BE callsites
// stay in lockstep; grep for the string to find callers.
const (
	EventCollectionCreated  = "collection.created"
	EventCollectionUpdated  = "collection.updated"
	EventCollectionDeleted  = "collection.deleted"
	EventCollectionShared   = "collection.shared"
	EventCollectionUnshared = "collection.unshared"

	EventRequestCreated = "request.created"
	EventRequestUpdated = "request.updated"
	EventRequestDeleted = "request.deleted"

	EventEnvironmentCreated  = "environment.created"
	EventEnvironmentUpdated  = "environment.updated"
	EventEnvironmentDeleted  = "environment.deleted"
	EventEnvironmentShared   = "environment.shared"
	EventEnvironmentUnshared = "environment.unshared"

	EventNoteCreated = "note.created"
	EventNoteUpdated = "note.updated"
	EventNoteDeleted = "note.deleted"
	EventNoteMoved   = "note.moved"
	EventNoteShared  = "note.shared"
	// An agent-memory suggestion was filed or decided on the note.
	EventNoteSuggestion = "note.suggestion"
)

// notifyChannel is the Postgres LISTEN channel name. Single channel for
// every event type keeps the listener simple; the listener decodes the
// JSON payload and dispatches by `Type`.
const notifyChannel = "patchman_events"
