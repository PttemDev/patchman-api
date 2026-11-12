package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/esquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ESOpsHandler serves the cluster / index / mapping browse surface plus the
// generic request forwarder. It reuses the connection handler's access check
// so owner + grant holders are gated consistently, and every op writes a
// fire-and-forget audit row. Mirrors RedisOpsHandler.
type ESOpsHandler struct {
	db        *gorm.DB
	registry  *esquery.Registry
	connsHand *ESConnectionHandler
}

func NewESOpsHandler(db *gorm.DB, reg *esquery.Registry, connsHand *ESConnectionHandler) *ESOpsHandler {
	return &ESOpsHandler{db: db, registry: reg, connsHand: connsHand}
}

// access resolves the connection + enforces the required role. `write`
// requests require owner or a write grant; `read` accepts any access. Returns
// the effective role so audit can flag delegated actions.
func (h *ESOpsHandler) access(c *gin.Context, needWrite bool) (*models.ESConnection, string, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.connsHand.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return nil, "", false
	}
	return conn, role, true
}

func (h *ESOpsHandler) ctx(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}

func (h *ESOpsHandler) Cluster(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	res, err := h.registry.Cluster(ctx, *conn)
	h.audit(c, conn, role, "cluster", "GET", "/_cluster/health", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *ESOpsHandler) Indices(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	res, err := h.registry.Indices(ctx, *conn)
	h.audit(c, conn, role, "indices", "GET", "/_cat/indices", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *ESOpsHandler) Aliases(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	res, err := h.registry.Aliases(ctx, *conn)
	h.audit(c, conn, role, "aliases", "GET", "/_cat/aliases", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *ESOpsHandler) Mapping(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	index := c.Query("index")
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	res, err := h.registry.Mapping(ctx, *conn, index)
	h.audit(c, conn, role, "mapping", "GET", index+"/_field_caps", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Request forwards a generic ES request. Read access reaches the endpoint; a
// write (per the classifier) re-gates to the write grant AND requires the FE
// allowWrite toggle — the same three-layer gate the Redis console uses.
func (h *ESOpsHandler) Request(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	var input struct {
		Method     string          `json:"method"`
		Path       string          `json:"path"`
		Body       json.RawMessage `json:"body"`
		AllowWrite bool            `json:"allowWrite"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	method := strings.TrimSpace(input.Method)
	if method == "" {
		method = http.MethodGet
	}
	if strings.TrimSpace(input.Path) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	// Re-gate write requests to the write grant even if the FE toggle asked for
	// a write — a read-only grant can never mutate.
	if esquery.Classify(method, input.Path) == esquery.ClassWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return
	}
	// Body may be a raw JSON value or an NDJSON string (e.g. _bulk); pass the
	// raw bytes through untouched. A JSON string body is unwrapped to its text.
	body := unwrapBody(input.Body)

	ctx, cancel := h.ctx(c, 60*time.Second)
	defer cancel()
	res, err := h.registry.Do(ctx, *conn, method, input.Path, body, input.AllowWrite)
	h.audit(c, conn, role, "request", method, input.Path, err)
	if esquery.IsWriteRequired(err) {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// unwrapBody normalizes the request body. A JSON string is treated as an
// NDJSON/raw payload and unwrapped to its bytes; any other JSON value is
// forwarded verbatim; null/empty forwards nothing.
func unwrapBody(raw json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			return []byte(s)
		}
	}
	return []byte(trimmed)
}

// audit writes a best-effort activity row. DelegatedOwner is set to the
// connection owner when the actor is a grant holder (role != owner). Never
// blocks the response.
func (h *ESOpsHandler) audit(c *gin.Context, conn *models.ESConnection, role, action, method, path string, actErr error) {
	userID := c.GetString("userID")
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	row := models.ESActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Method: method, Path: path,
		ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
