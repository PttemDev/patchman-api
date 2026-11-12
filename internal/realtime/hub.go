package realtime

import (
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// Hub keeps track of every WebSocket client connected to *this pod*. It's
// not a cross-pod synchronization primitive — cross-pod fan-out is the
// listener's job. Hub just answers "which local clients should I push
// this event to?" given a recipient list or an everyone-flag.
//
// Concurrency: clients map is RWMutex-guarded. Per-client writes go
// through the client's own buffered channel (see client.go), so Hub
// methods don't block waiting on slow consumers — a full channel drops
// the client (logged + closed) rather than back-pressuring the listener.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	// byUser is a secondary index so per-recipient broadcasts don't have
	// to iterate every connection. One user may have multiple sockets
	// open (multiple tabs/devices), hence the set value.
	byUser map[string]map[*Client]struct{}
}

// hub is the process-wide singleton. Initialized in main.go via Init().
// Kept as a package var (rather than dependency-injected through every
// handler) because the Notify entry point is called from deep inside
// existing handlers and threading a *Hub through every signature would
// touch ~20 files for no benefit — the hub is genuinely process-global.
var hub *Hub

// Init creates the singleton hub. Safe to call once at boot. Subsequent
// calls panic so a misconfigured wiring fails loudly instead of silently
// silently dropping events on the floor.
func Init() {
	if hub != nil {
		panic("realtime.Init called twice")
	}
	hub = &Hub{
		clients: make(map[*Client]struct{}),
		byUser:  make(map[string]map[*Client]struct{}),
	}
}

// GetHub returns the singleton. Returns nil if Init wasn't called — the
// WS handler refuses upgrades in that case so the FE gets a clear 503
// instead of hanging on a never-completing handshake.
func GetHub() *Hub { return hub }

// AttachClient is the handler-facing entry point. Takes an upgraded
// gorilla/websocket Conn + the authenticated userID, wires up the
// pumps, and registers with the hub. The handler returns immediately
// after calling this — pumps own the conn for its lifetime.
func AttachClient(conn *websocket.Conn, userID string) {
	if hub == nil {
		// Defensive: should be caught by the handler's GetHub() guard,
		// but a direct caller (test code) might miss that. Close the
		// conn so the FE notices and reconnects.
		_ = conn.Close()
		return
	}
	c := newClient(conn, userID)
	hub.register(c)
}

// register adds a client to the hub. Called from the WS handler after a
// successful Upgrade + auth. The byUser index is keyed on the client's
// userID so subsequent recipient-based broadcasts can find it in O(1).
func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	set, ok := h.byUser[c.userID]
	if !ok {
		set = make(map[*Client]struct{})
		h.byUser[c.userID] = set
	}
	set[c] = struct{}{}
	log.Printf("[realtime] register user=%s total_clients=%d", c.userID, len(h.clients))
}

// unregister removes a client. Called from the client's own teardown
// path (read pump exits → defer unregister). Idempotent.
func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if set, ok := h.byUser[c.userID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byUser, c.userID)
		}
	}
	log.Printf("[realtime] unregister user=%s total_clients=%d", c.userID, len(h.clients))
}

// Broadcast delivers an event to every connected client that matches the
// event's recipient set. Called from the listener (cross-pod path) AND
// directly from notify.go on single-pod paths (SQLite, embedded).
//
// The event copy sent to each client has Recipients cleared — clients
// don't need to know who else got the message, and exposing the list
// would leak access patterns (especially harmful when Everyone=false
// and the recipient is a small set).
func (h *Hub) Broadcast(e Event) {
	// Decide the target client set up-front so we hold the read lock for
	// as little time as possible. Writes go to per-client channels so
	// they're non-blocking from the hub's perspective.
	var targets []*Client

	h.mu.RLock()
	if e.Everyone {
		targets = make([]*Client, 0, len(h.clients))
		for c := range h.clients {
			targets = append(targets, c)
		}
	} else {
		for _, uid := range e.Recipients {
			if set, ok := h.byUser[uid]; ok {
				for c := range set {
					targets = append(targets, c)
				}
			}
		}
	}
	h.mu.RUnlock()

	// Strip recipient metadata before sending — see godoc above.
	out := e
	out.Recipients = nil
	out.Everyone = false

	for _, c := range targets {
		select {
		case c.send <- out:
		default:
			// Channel full — consumer is too slow. Drop the client
			// rather than risk back-pressuring the listener. The FE
			// reconnects with exponential backoff and a full
			// refreshLists fills any gap.
			log.Printf("[realtime] dropping slow client user=%s", c.userID)
			c.closeOnce.Do(func() { close(c.shutdown) })
		}
	}
}
