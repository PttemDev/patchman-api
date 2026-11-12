package realtime

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client wraps a single authenticated WebSocket connection. Lifetime:
//
//  1. WS Upgrade succeeds + JWT validates → handler creates Client + Register
//  2. Two goroutines start: readPump (consumes pong replies), writePump
//     (consumes the send channel + heartbeat ticker)
//  3. Any error or shutdown signal closes the conn; the deferred
//     Unregister cleans the hub entry
//
// The write side is single-writer (only writePump touches Conn.WriteX), so
// no per-conn mutex is needed. Hub fans out events by pushing them into
// the buffered `send` channel; writePump drains.
type Client struct {
	conn     *websocket.Conn
	userID   string
	send     chan Event
	shutdown chan struct{}
	// closeOnce ensures the shutdown channel is closed exactly once
	// even when Hub's slow-consumer path AND a normal disconnect race.
	closeOnce sync.Once
}

// Heartbeat & buffer constants. Picked to survive nginx-ingress's default
// 60s idle timeout — server pings every 30s; if a pong doesn't arrive
// within readDeadline, the read returns an error and the client tears
// down.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 30 * time.Second
	sendBufferSize = 64
)

// newClient creates a Client and starts both pumps. Returns the *Client
// so the handler can defer hub.unregister(c). The caller must already
// have a hub instance — handler-side guard ensures we never reach here
// before Init().
func newClient(conn *websocket.Conn, userID string) *Client {
	c := &Client{
		conn:     conn,
		userID:   userID,
		send:     make(chan Event, sendBufferSize),
		shutdown: make(chan struct{}),
	}
	go c.writePump()
	go c.readPump()
	return c
}

// readPump exists almost entirely to consume pong frames so the read
// deadline gets reset. The FE doesn't send any meaningful payloads on
// this socket today; any unexpected client→server frame is logged and
// ignored. When ReadMessage returns an error (client closed, ping
// timeout, network blip) the pump exits, triggering teardown.
func (c *Client) readPump() {
	defer func() {
		c.closeOnce.Do(func() { close(c.shutdown) })
		hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(1024) // FE shouldn't be sending bulk data
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			// websocket.CloseNormalClosure is the boring case; anything
			// else (timeout, abnormal closure, refused frame) gets a
			// log line so we can spot connectivity issues in prod.
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway) {
				log.Printf("[realtime] read error user=%s: %v", c.userID, err)
			}
			return
		}
	}
}

// writePump drains the send channel and emits ping frames on a ticker.
// Exits on any write error or on shutdown — readPump also closes shutdown
// when it tears down, so both pumps converge.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case ev, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteJSON(ev); err != nil {
				log.Printf("[realtime] write error user=%s: %v", c.userID, err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.shutdown:
			return
		}
	}
}
