package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// RealtimeHandler upgrades an HTTP request to a WebSocket connection
// owned by the realtime hub. One endpoint, one purpose: deliver mutation
// notifications so the FE can refresh its stores without polling.
//
// Auth runs upstream — the route is mounted under the standard JWT
// middleware which also accepts a `?token=` query param fallback for
// the browser WebSocket API (which can't set custom headers on the
// Upgrade request). By the time this handler runs, userID is on the
// gin context.
//
// allowedOrigins is the parsed REALTIME_ALLOWED_ORIGINS allowlist. Empty
// = allow any origin (defaults to current behaviour). Non-empty = strict
// match against the request's `Origin` header; same-origin browsers
// send an Origin equal to their page origin, so the prod config sets
// this to the FE deployment URL(s).
type RealtimeHandler struct {
	upgrader websocket.Upgrader
}

func NewRealtimeHandler(allowedOrigins string) *RealtimeHandler {
	allowlist := parseOrigins(allowedOrigins)
	return &RealtimeHandler{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Empty allowlist preserves the previous "allow any"
				// behaviour — convenient for dev / first rollout.
				if len(allowlist) == 0 {
					return true
				}
				origin := r.Header.Get("Origin")
				// Non-browser clients (curl, server-to-server) usually
				// don't send Origin; if the allowlist is configured
				// we still accept them — the JWT guard upstream is the
				// real authentication. Browsers will always send Origin
				// so the strict path applies where it matters.
				if origin == "" {
					return true
				}
				_, ok := allowlist[origin]
				return ok
			},
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
		},
	}
}

// parseOrigins splits the comma-separated env string and trims spaces.
// Returned as a set keyed on the origin so lookup is O(1) per Upgrade.
func parseOrigins(s string) map[string]struct{} {
	if s == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(s, ",") {
		o := strings.TrimSpace(part)
		if o != "" {
			out[o] = struct{}{}
		}
	}
	return out
}

// Subscribe handles GET /api/realtime. On success the connection lives
// until either side disconnects; the hub fans out events to it via the
// client's send channel (see realtime/client.go).
func (h *RealtimeHandler) Subscribe(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if realtime.GetHub() == nil {
		// Init() wasn't called at boot. Fail loud — the FE retries with
		// backoff and a 503 surfaces in the dev console so the operator
		// notices misconfiguration.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "realtime hub not initialized"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade failed (bad headers, network blip, origin rejected).
		// gorilla already wrote an HTTP error response, so we just return.
		return
	}
	realtime.AttachClient(conn, userID)
}
