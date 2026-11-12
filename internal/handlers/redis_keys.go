package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/redisquery"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RedisOpsHandler serves the key-browse + value + command-console surface.
// It reuses the connection handler's access check so owner + grant holders
// are gated consistently, and every op writes a fire-and-forget audit row.
type RedisOpsHandler struct {
	db        *gorm.DB
	registry  *redisquery.Registry
	connsHand *RedisConnectionHandler
}

func NewRedisOpsHandler(db *gorm.DB, reg *redisquery.Registry, connsHand *RedisConnectionHandler) *RedisOpsHandler {
	return &RedisOpsHandler{db: db, registry: reg, connsHand: connsHand}
}

// access resolves the connection + enforces the required role. `write`
// requests require owner or a write grant; `read` accepts any access.
// Returns the effective role so audit can flag delegated actions.
func (h *RedisOpsHandler) access(c *gin.Context, needWrite bool) (*models.RedisConnection, string, bool) {
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
	if needWrite && !conn.AllowWrite {
		c.JSON(http.StatusForbidden, gin.H{"error": redisReadOnlyMsg})
		return nil, "", false
	}
	return conn, role, true
}

// redisReadOnlyMsg answers any write on a connection whose read-write switch
// is off.
const redisReadOnlyMsg = "This connection is read-only. Enable read-write from its ⋯ menu to run write commands."

func (h *RedisOpsHandler) ctx(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}

func (h *RedisOpsHandler) ScanKeys(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	db := parseInt(c.Query("db"), conn.DBIndex)
	cursor, _ := strconv.ParseUint(c.Query("cursor"), 10, 64)
	// count = how many MATCHING keys to return; the registry keeps SCANning
	// (bounded by a time/iteration budget) until it has them.
	count := parseInt(c.Query("count"), 100)
	// smart=1 (the FE search box): plain text becomes a contains match,
	// an explicit glob is used as-is. Without it `match` is a raw glob.
	match := c.Query("match")
	if c.Query("smart") == "1" {
		match = redisquery.BuildMatch(match)
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	listing, err := h.registry.ScanKeys(ctx, *conn, db, match, cursor, count)
	h.audit(c, conn, role, "scan", "", gin.H{"match": match, "db": db}, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": listing})
}

func (h *RedisOpsHandler) KeyMeta(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	db := parseInt(c.Query("db"), conn.DBIndex)
	key := c.Query("key")
	ctx, cancel := h.ctx(c, 15*time.Second)
	defer cancel()
	meta, err := h.registry.KeyMeta(ctx, *conn, db, key)
	h.audit(c, conn, role, "meta", key, nil, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meta})
}

func (h *RedisOpsHandler) GetValue(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	db := parseInt(c.Query("db"), conn.DBIndex)
	key := c.Query("key")
	start := int64(parseInt(c.Query("start"), 0))
	stop := int64(parseInt(c.Query("stop"), 0))
	cursor, _ := strconv.ParseUint(c.Query("cursor"), 10, 64)
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	val, err := h.registry.GetValue(ctx, *conn, db, key, start, stop, cursor)
	h.audit(c, conn, role, "get", key, nil, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": val})
}

func (h *RedisOpsHandler) DeleteKey(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	db := parseInt(c.Query("db"), conn.DBIndex)
	key := c.Query("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	ctx, cancel := h.ctx(c, 15*time.Second)
	defer cancel()
	err := h.registry.DeleteKey(ctx, *conn, db, key)
	h.audit(c, conn, role, "delete", key, gin.H{"db": db}, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// RemoveElement deletes one element of a collection key (hash field, list
// element, set / sorted-set member, stream entry). Same write gate as
// DeleteKey: owner or write grant AND the connection's read-write switch.
// 409 when nothing was removed (element gone / list changed under the view).
func (h *RedisOpsHandler) RemoveElement(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var input redisquery.ElementRef
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := input.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.ctx(c, 15*time.Second)
	defer cancel()
	res, err := h.registry.RemoveElement(ctx, *conn, input)
	detail := gin.H{"db": input.DB, "type": input.Type}
	switch {
	case input.Field != nil:
		detail["field"] = *input.Field
	case input.Member != nil:
		detail["member"] = *input.Member
	case input.Index != nil:
		detail["index"] = *input.Index
	case input.EntryID != "":
		detail["entryId"] = input.EntryID
	}
	h.audit(c, conn, role, "remove_element", input.Key, detail, err)
	if redisquery.IsElementGone(err) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *RedisOpsHandler) SetTTL(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		DB     int    `json:"db"`
		Key    string `json:"key"`
		TTLSec int64  `json:"ttlSec"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	ctx, cancel := h.ctx(c, 15*time.Second)
	defer cancel()
	err := h.registry.SetTTL(ctx, *conn, input.DB, input.Key, input.TTLSec)
	h.audit(c, conn, role, "set_ttl", input.Key, gin.H{"ttlSec": input.TTLSec}, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// audit writes a best-effort activity row. DelegatedOwner is set to the
// connection owner when the actor is a grant holder (role != owner). Never
// blocks the response.
func (h *RedisOpsHandler) audit(c *gin.Context, conn *models.RedisConnection, role, action, key string, detail any, actErr error) {
	userID := c.GetString("userID")
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	var detailJSON string
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	row := models.RedisActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Key: key, DetailJSON: detailJSON,
		ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
