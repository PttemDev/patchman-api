package handlers

import (
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/redisquery"

	"github.com/gin-gonic/gin"
)

// RunCommand executes a single Redis command from the console. Read access
// is enough to reach the endpoint; a write command re-gates to the write
// grant (three-layer gate: FE toggle → this grant check → registry
// classifier). Blocked commands are rejected regardless of grant.
func (h *RedisOpsHandler) RunCommand(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	var input struct {
		DB         int      `json:"db"`
		Args       []string `json:"args"`
		AllowWrite bool     `json:"allowWrite"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(input.Args) == 0 || strings.TrimSpace(input.Args[0]) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "args is required"})
		return
	}
	// Re-gate write commands to the write grant even if the FE toggle asked
	// for a write — a read-only grant can never run a mutating command.
	if redisquery.Classify(input.Args[0]) == redisquery.ClassWrite {
		if role != "owner" && role != "write" {
			c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
			return
		}
		if !conn.AllowWrite {
			c.JSON(http.StatusForbidden, gin.H{"error": redisReadOnlyMsg})
			return
		}
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	res, err := h.registry.RunCommand(ctx, *conn, input.DB, input.Args, input.AllowWrite)
	h.audit(c, conn, role, "command", "", gin.H{"args": input.Args, "allowWrite": input.AllowWrite}, err)
	if redisquery.IsWriteRequired(err) {
		c.JSON(http.StatusForbidden, gin.H{"error": "This command writes data. Turn on writes for it (allowWrite=true) to run it."})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// PeekChannel subscribes to channels/pattern on a throwaway connection and
// collects a bounded set of messages. Commit-less by design — Redis pub/sub
// has no durable consumer state, so peeking leaves no trace. Read access.
func (h *RedisOpsHandler) PeekChannel(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	var input struct {
		DB       int      `json:"db"`
		Channels []string `json:"channels"`
		Pattern  string   `json:"pattern"`
		N        int      `json:"n"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.ctx(c, 12*time.Second)
	defer cancel()
	res, err := h.registry.PeekChannel(ctx, *conn, input.DB, input.Channels, input.Pattern, input.N)
	h.audit(c, conn, role, "pubsub_peek", "", gin.H{"channels": input.Channels, "pattern": input.Pattern}, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}
