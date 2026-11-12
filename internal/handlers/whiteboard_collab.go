package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"patchman-be/internal/middleware"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

// WhiteboardCollabHandler is the real-time multiplayer surface for whiteboards.
// It is DELIBERATELY separate from the user-keyed notification hub (realtime/):
// that hub is one-directional (discards inbound) and fans out by userID, whereas
// live boards need bidirectional, resource-scoped ROOMS. This handler keeps an
// in-process room per whiteboard and relays element/pointer/presence frames
// between the editors in that room. It holds NO scene state — persistence stays
// on the autosave PATCH path; this only relays live deltas.
//
// Auth is per-connection (the route is mounted WITHOUT the JWT middleware so
// anonymous editor-link holders can join):
//   - ?token=<jwt>  → a signed-in user; role via ResolveWhiteboardRole
//   - ?link=<token> → an anonymous link holder; role from the link row
//   - embedded mode with neither → the local desktop user (owns everything)
// A viewer connects but the server drops their outbound frames (read-only).
//
// NB: rooms are in-process, so collaboration does NOT span BE replicas. v1 is
// single-pod; a multi-pod deployment needs a cross-pod relay (Redis pub/sub or
// the existing pg_notify bus) before live boards work across replicas.
type WhiteboardCollabHandler struct {
	db       *gorm.DB
	shareH   *WhiteboardSharingHandler
	secret   string
	embedded bool
	upgrader websocket.Upgrader
	mu       sync.Mutex
	rooms    map[string]*collabRoom
}

func NewWhiteboardCollabHandler(db *gorm.DB, secret string, embedded bool, allowedOrigins string) *WhiteboardCollabHandler {
	allow := parseOrigins(allowedOrigins)
	return &WhiteboardCollabHandler{
		db:       db,
		shareH:   NewWhiteboardSharingHandler(db),
		secret:   secret,
		embedded: embedded,
		rooms:    make(map[string]*collabRoom),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				if len(allow) == 0 {
					return true
				}
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				_, ok := allow[origin]
				return ok
			},
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
}

type collabRoom struct {
	id      string
	mu      sync.Mutex
	clients map[*collabClient]struct{}
}

type collabClient struct {
	id      string // ephemeral connection id (senderId echoed to peers)
	name    string
	conn    *websocket.Conn
	send    chan []byte
	canEdit bool
	mu      sync.Mutex
	closed  bool
}

const (
	collabWriteWait  = 10 * time.Second
	collabPongWait   = 60 * time.Second
	collabPingPeriod = 25 * time.Second
	collabReadLimit  = 8 << 20 // 8MB — a scene frame can carry embedded images
)

// Connect upgrades the request and joins the board's room after resolving the
// caller's role. See the type doc for the auth matrix.
func (h *WhiteboardCollabHandler) Connect(c *gin.Context) {
	id := c.Param("id")
	role, userID, display := h.resolveRole(c, id)
	if !role.AtLeast(ViewerRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "No access to this board"})
		return
	}
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	_ = userID
	client := &collabClient{
		id:      uuid.New().String(),
		name:    display,
		conn:    conn,
		send:    make(chan []byte, 32),
		canEdit: role.AtLeast(EditorRole),
	}
	room := h.roomFor(id)
	room.add(client)
	go h.writePump(client)
	h.readPump(room, client)
}

// resolveRole derives the caller's role + a display name from the query auth.
func (h *WhiteboardCollabHandler) resolveRole(c *gin.Context, id string) (Role, string, string) {
	if link := c.Query("link"); link != "" {
		l, wb := h.shareH.resolveLink(link)
		if l == nil || wb == nil {
			return NoRole, "", ""
		}
		// The linked board itself, or any board under a linked folder.
		if _, ok := h.shareH.linkCovers(wb, id); !ok {
			return NoRole, "", ""
		}
		if l.Role == "editor" {
			return EditorRole, "", "Guest"
		}
		return ViewerRole, "", "Guest"
	}
	// Embedded desktop BE uses LocalUserAuth (no JWT) — and can't validate a
	// remote-issued token anyway (different secret). Resolve as the local user
	// regardless of any token the FE sent.
	if h.embedded {
		return ResolveWhiteboardRole(h.db, middleware.LocalUserID, id), middleware.LocalUserID, "You"
	}
	token := c.Query("token")
	if token == "" {
		return NoRole, "", ""
	}
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.secret), nil
	})
	if err != nil || !parsed.Valid {
		return NoRole, "", ""
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return NoRole, "", ""
	}
	userID, _ := claims["user_id"].(string)
	if userID == "" {
		return NoRole, "", ""
	}
	name := userID
	var u struct{ Name, Email string }
	if h.db.Table("users").Select("name, email").Where("id = ?", userID).Scan(&u).Error == nil {
		if u.Name != "" {
			name = u.Name
		} else if u.Email != "" {
			name = u.Email
		}
	}
	return ResolveWhiteboardRole(h.db, userID, id), userID, name
}

func (h *WhiteboardCollabHandler) roomFor(id string) *collabRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		r = &collabRoom{id: id, clients: make(map[*collabClient]struct{})}
		h.rooms[id] = r
	}
	return r
}

func (h *WhiteboardCollabHandler) dropRoomIfEmpty(r *collabRoom) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(r.clients) == 0 {
		delete(h.rooms, r.id)
	}
}

func (r *collabRoom) add(cl *collabClient) {
	r.mu.Lock()
	r.clients[cl] = struct{}{}
	r.mu.Unlock()
	r.broadcastPresence()
}

func (r *collabRoom) remove(cl *collabClient) {
	r.mu.Lock()
	delete(r.clients, cl)
	r.mu.Unlock()
	cl.close()
	r.broadcastPresence()
}

// relay sends payload to every client in the room EXCEPT the sender. A slow
// client that can't keep up is dropped (its send buffer is full) rather than
// blocking the room.
func (r *collabRoom) relay(sender *collabClient, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for cl := range r.clients {
		if cl == sender {
			continue
		}
		if !cl.trySend(payload) {
			// Full buffer or already-closed → evict now (deleting during range
			// is safe in Go) so we never send on its closed channel again.
			delete(r.clients, cl)
			cl.close()
		}
	}
}

// broadcastPresence pushes the current collaborator roster to everyone so each
// client can render who's here (cursors are keyed on these ids).
func (r *collabRoom) broadcastPresence() {
	r.mu.Lock()
	type peer struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Edit bool   `json:"canEdit"`
	}
	peers := make([]peer, 0, len(r.clients))
	for cl := range r.clients {
		peers = append(peers, peer{ID: cl.id, Name: cl.name, Edit: cl.canEdit})
	}
	buf, _ := json.Marshal(gin.H{"type": "presence", "peers": peers})
	// Send under the lock with the same evict-on-fail as relay, so a closed or
	// stalled client can never take a send-on-closed-channel panic.
	for cl := range r.clients {
		if !cl.trySend(buf) {
			delete(r.clients, cl)
			cl.close()
		}
	}
	r.mu.Unlock()
}

func (h *WhiteboardCollabHandler) readPump(room *collabRoom, cl *collabClient) {
	defer func() {
		room.remove(cl)
		h.dropRoomIfEmpty(room)
	}()
	cl.conn.SetReadLimit(collabReadLimit)
	_ = cl.conn.SetReadDeadline(time.Now().Add(collabPongWait))
	cl.conn.SetPongHandler(func(string) error {
		_ = cl.conn.SetReadDeadline(time.Now().Add(collabPongWait))
		return nil
	})
	for {
		_, msg, err := cl.conn.ReadMessage()
		if err != nil {
			return
		}
		// Viewers are read-only: their inbound frames are ignored (never relayed).
		if !cl.canEdit {
			continue
		}
		// Stamp the sender id so peers can ignore their own echo, then relay the
		// raw frame (scene / pointer). We wrap rather than trust client-sent ids.
		room.relay(cl, stampSender(msg, cl.id))
	}
}

func (h *WhiteboardCollabHandler) writePump(cl *collabClient) {
	ticker := time.NewTicker(collabPingPeriod)
	defer func() {
		ticker.Stop()
		_ = cl.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-cl.send:
			_ = cl.conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			if !ok {
				_ = cl.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := cl.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = cl.conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			if err := cl.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// close is idempotent; trySend is the ONLY safe way to send (a raw
// `cl.send <- x` panics if another path already closed the channel).
func (cl *collabClient) close() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if !cl.closed {
		cl.closed = true
		close(cl.send)
	}
}

// trySend enqueues b if the client is open and its buffer has room. Returns
// false when the client is closed or its buffer is full (caller evicts).
func (cl *collabClient) trySend(b []byte) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return false
	}
	select {
	case cl.send <- b:
		return true
	default:
		return false
	}
}

// stampSender injects a server-authoritative "senderId" as the first key of a
// JSON object frame so peers can drop their own echo without us trusting a
// client-supplied id — and without fully parsing a (potentially large) scene
// frame: we just splice after the opening brace. Non-object frames pass through
// unchanged (they'll simply carry no senderId).
func stampSender(msg []byte, id string) []byte {
	trimmed := bytes.TrimLeft(msg, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return msg
	}
	prefix := []byte(`{"senderId":"` + id + `"`)
	rest := trimmed[1:]
	// Empty object {} → no trailing comma before the closing brace.
	restTrimmed := bytes.TrimLeft(rest, " \t\r\n")
	out := make([]byte, 0, len(prefix)+len(rest)+1)
	out = append(out, prefix...)
	if len(restTrimmed) > 0 && restTrimmed[0] != '}' {
		out = append(out, ',')
	}
	out = append(out, rest...)
	return out
}
