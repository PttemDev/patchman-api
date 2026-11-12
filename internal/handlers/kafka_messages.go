package handlers

import (
	"context"
	"net/http"
	"time"

	"patchman-be/internal/kafkaquery"

	"github.com/gin-gonic/gin"
)

// KafkaMessageHandler serves the message peek (read-only, never commits)
// and the guarded produce path.
type KafkaMessageHandler struct {
	admin *KafkaAdminHandler
}

func NewKafkaMessageHandler(admin *KafkaAdminHandler) *KafkaMessageHandler {
	return &KafkaMessageHandler{admin: admin}
}

// Consume peeks messages. Read access is enough — it joins no group and
// commits no offset.
func (h *KafkaMessageHandler) Consume(c *gin.Context) {
	conn, ok := h.admin.access(c, false)
	if !ok {
		return
	}
	var req kafkaquery.ConsumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Convention: Partition -1 = all partitions. The FE always sends it
	// explicitly (JSON can't distinguish an omitted 0 from partition 0).
	// Above the peek's own 20 s budget, so a slow peek returns what arrived.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	res, err := h.admin.registry.Consume(ctx, *conn, req)
	if err != nil {
		h.admin.audit(c, conn.ID, "consume", req.Topic, req, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	h.admin.audit(c, conn.ID, "consume", req.Topic, gin.H{"mode": req.Mode, "n": req.N}, nil)
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Produce publishes a single record. Requires write access.
func (h *KafkaMessageHandler) Produce(c *gin.Context) {
	conn, ok := h.admin.access(c, true)
	if !ok {
		return
	}
	var req kafkaquery.ProduceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Above the peek's own 20 s budget, so a slow peek returns what arrived.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	partition, offset, err := h.admin.registry.Produce(ctx, *conn, req)
	h.admin.audit(c, conn.ID, "produce", req.Topic, gin.H{"partition": req.Partition}, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "partition": partition, "offset": offset}})
}
