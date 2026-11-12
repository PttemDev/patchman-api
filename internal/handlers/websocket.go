package handlers

import (
	"crypto/tls"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// --- Types ---

type wsMessage struct {
	Type      string    `json:"type"`      // "text" or "binary"
	Data      string    `json:"data"`
	Direction string    `json:"direction"` // "sent" or "received"
	Timestamp time.Time `json:"timestamp"`
}

type wsConnection struct {
	ID         string
	TargetConn *websocket.Conn
	Messages   []wsMessage
	CreatedAt  time.Time
	LastActive time.Time
	mu         sync.Mutex
	done       chan struct{}
	closeOnce  sync.Once
}

type WebSocketHandler struct {
	connections map[string]*wsConnection
	mu          sync.RWMutex
}

func NewWebSocketHandler() *WebSocketHandler {
	h := &WebSocketHandler{
		connections: make(map[string]*wsConnection),
	}
	go h.cleanupLoop()
	return h
}

// --- Connection Pool Management ---

func (h *WebSocketHandler) getConnection(id string) (*wsConnection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn, ok := h.connections[id]
	return conn, ok
}

func (h *WebSocketHandler) storeConnection(conn *wsConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connections[conn.ID] = conn
}

func (h *WebSocketHandler) removeConnection(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.connections[id]; ok {
		conn.closeOnce.Do(func() {
			close(conn.done)
			conn.TargetConn.Close()
		})
		delete(h.connections, id)
	}
}

func (h *WebSocketHandler) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		now := time.Now()
		for id, conn := range h.connections {
			conn.mu.Lock()
			inactive := now.Sub(conn.LastActive) > 30*time.Minute
			conn.mu.Unlock()
			if inactive {
				log.Printf("Cleaning up inactive WebSocket connection: %s", id)
				conn.closeOnce.Do(func() {
					close(conn.done)
					conn.TargetConn.Close()
				})
				delete(h.connections, id)
			}
		}
		h.mu.Unlock()
	}
}

// --- Handlers ---

type wsConnectRequest struct {
	URL       string            `json:"url" binding:"required"`
	Headers   map[string]string `json:"headers"`
	Protocols []string          `json:"protocols"`
}

// Connect establishes a WebSocket connection to the target server.
// POST /api/ws/connect
func (h *WebSocketHandler) Connect(c *gin.Context) {
	var req wsConnectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build request headers
	reqHeaders := http.Header{}
	for k, v := range req.Headers {
		reqHeaders.Set(k, v)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     req.Protocols,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	targetConn, resp, err := dialer.Dial(req.URL, reqHeaders)
	if err != nil {
		errMsg := err.Error()
		if resp != nil {
			errMsg = resp.Status + ": " + errMsg
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to WebSocket server: " + errMsg})
		return
	}

	connID := uuid.New().String()
	now := time.Now()

	wsConn := &wsConnection{
		ID:         connID,
		TargetConn: targetConn,
		Messages:   make([]wsMessage, 0),
		CreatedAt:  now,
		LastActive: now,
		done:       make(chan struct{}),
	}

	h.storeConnection(wsConn)

	// Collect response headers
	responseHeaders := make(map[string]string)
	if resp != nil {
		for k, v := range resp.Header {
			if len(v) > 0 {
				responseHeaders[k] = v[0]
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"connectionId": connID,
			"protocol":     targetConn.Subprotocol(),
			"headers":      responseHeaders,
		},
	})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Stream upgrades the HTTP connection to WebSocket and relays messages
// bidirectionally between the frontend and the target server.
// GET /api/ws/stream/:connectionId
func (h *WebSocketHandler) Stream(c *gin.Context) {
	connectionID := c.Param("connectionId")

	wsConn, ok := h.getConnection(connectionID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	// Upgrade the frontend connection to WebSocket
	frontendConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Failed to upgrade frontend connection: %v", err)
		return
	}
	defer frontendConn.Close()

	// Channel to signal when either side disconnects
	errCh := make(chan error, 2)

	// Goroutine: target -> frontend
	go func() {
		for {
			msgType, msg, err := wsConn.TargetConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}

			wsConn.mu.Lock()
			wsConn.LastActive = time.Now()

			typeStr := "text"
			if msgType == websocket.BinaryMessage {
				typeStr = "binary"
			}

			wsConn.Messages = append(wsConn.Messages, wsMessage{
				Type:      typeStr,
				Data:      string(msg),
				Direction: "received",
				Timestamp: time.Now(),
			})
			wsConn.mu.Unlock()

			if err := frontendConn.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Goroutine: frontend -> target
	go func() {
		for {
			msgType, msg, err := frontendConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}

			wsConn.mu.Lock()
			wsConn.LastActive = time.Now()

			typeStr := "text"
			if msgType == websocket.BinaryMessage {
				typeStr = "binary"
			}

			wsConn.Messages = append(wsConn.Messages, wsMessage{
				Type:      typeStr,
				Data:      string(msg),
				Direction: "sent",
				Timestamp: time.Now(),
			})
			wsConn.mu.Unlock()

			if err := wsConn.TargetConn.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for either side to disconnect or the connection to be closed
	select {
	case <-errCh:
		// One side disconnected; close both
	case <-wsConn.done:
		// Connection was explicitly closed via the disconnect endpoint
	}
}

type wsSendRequest struct {
	Message string `json:"message" binding:"required"`
	Type    string `json:"type"` // "text" or "binary", defaults to "text"
}

// Send sends a message to the target WebSocket server.
// POST /api/ws/send/:connectionId
func (h *WebSocketHandler) Send(c *gin.Context) {
	connectionID := c.Param("connectionId")

	wsConn, ok := h.getConnection(connectionID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	var req wsSendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msgType := websocket.TextMessage
	typeStr := "text"
	if req.Type == "binary" {
		msgType = websocket.BinaryMessage
		typeStr = "binary"
	}

	wsConn.mu.Lock()
	wsConn.LastActive = time.Now()
	wsConn.Messages = append(wsConn.Messages, wsMessage{
		Type:      typeStr,
		Data:      req.Message,
		Direction: "sent",
		Timestamp: time.Now(),
	})
	wsConn.mu.Unlock()

	if err := wsConn.TargetConn.WriteMessage(msgType, []byte(req.Message)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Message sent"}})
}

// Disconnect closes the WebSocket connection to the target server.
// POST /api/ws/disconnect/:connectionId
func (h *WebSocketHandler) Disconnect(c *gin.Context) {
	connectionID := c.Param("connectionId")

	wsConn, ok := h.getConnection(connectionID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	// Grab the message history before removing
	wsConn.mu.Lock()
	messages := make([]wsMessage, len(wsConn.Messages))
	copy(messages, wsConn.Messages)
	wsConn.mu.Unlock()

	h.removeConnection(connectionID)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"message":  "Connection closed",
			"messages": messages,
		},
	})
}

// History returns the message history for a connection.
// GET /api/ws/history/:connectionId
func (h *WebSocketHandler) History(c *gin.Context) {
	connectionID := c.Param("connectionId")

	wsConn, ok := h.getConnection(connectionID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	wsConn.mu.Lock()
	messages := make([]wsMessage, len(wsConn.Messages))
	copy(messages, wsConn.Messages)
	wsConn.mu.Unlock()

	c.JSON(http.StatusOK, gin.H{"data": messages})
}
