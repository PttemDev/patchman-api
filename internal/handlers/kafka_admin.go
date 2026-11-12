package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"patchman-be/internal/kafkaquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// KafkaAdminHandler serves the cluster-admin surface (topics, configs,
// create/alter/partitions, brokers, groups). It reuses the connection
// handler's access check so owner + grant holders are gated consistently.
type KafkaAdminHandler struct {
	db        *gorm.DB
	registry  *kafkaquery.Registry
	connsHand *KafkaConnectionHandler
}

func NewKafkaAdminHandler(db *gorm.DB, reg *kafkaquery.Registry, connsHand *KafkaConnectionHandler) *KafkaAdminHandler {
	return &KafkaAdminHandler{db: db, registry: reg, connsHand: connsHand}
}

// access resolves the connection + enforces the required role. `write`
// requests require owner or a write grant; `read` accepts any access.
func (h *KafkaAdminHandler) access(c *gin.Context, needWrite bool) (*models.KafkaConnection, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.connsHand.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return nil, false
	}
	return conn, true
}

func (h *KafkaAdminHandler) reqCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), 30*time.Second)
}

func (h *KafkaAdminHandler) Topics(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	topics, err := h.registry.ListTopics(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": topics})
}

// TopicsOverview serves the enriched Topics-tab list (partitions, RF,
// approximate message count, health flags). Read access is enough.
func (h *KafkaAdminHandler) TopicsOverview(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	topics, err := h.registry.TopicsOverview(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": topics})
}

// TopicDetail serves the expanded per-topic view (partition placement +
// watermarks + config). Read access is enough.
func (h *KafkaAdminHandler) TopicDetail(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	detail, err := h.registry.TopicDetail(ctx, *conn, c.Param("topic"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail})
}

func (h *KafkaAdminHandler) TopicConfig(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	cfg, err := h.registry.TopicConfig(ctx, *conn, c.Param("topic"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": cfg})
}

func (h *KafkaAdminHandler) AlterTopicConfig(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		Config map[string]string `json:"config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	topic := c.Param("topic")
	err := h.registry.AlterTopicConfig(ctx, *conn, topic, input.Config)
	h.audit(c, conn.ID, "alter_config", topic, input.Config, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *KafkaAdminHandler) CreateTopic(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		Name              string            `json:"name" binding:"required"`
		Partitions        int32             `json:"partitions"`
		ReplicationFactor int16             `json:"replicationFactor"`
		Config            map[string]string `json:"config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Partitions < 1 {
		input.Partitions = 1
	}
	if input.ReplicationFactor < 1 {
		input.ReplicationFactor = 1
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	err := h.registry.CreateTopic(ctx, *conn, input.Name, input.Partitions, input.ReplicationFactor, input.Config)
	h.audit(c, conn.ID, "create_topic", input.Name, input, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"ok": true}})
}

func (h *KafkaAdminHandler) AddPartitions(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		Total int `json:"total"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	topic := c.Param("topic")
	err := h.registry.AddPartitions(ctx, *conn, topic, input.Total)
	h.audit(c, conn.ID, "add_partitions", topic, input, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *KafkaAdminHandler) DeleteTopic(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	topic := c.Param("topic")
	err := h.registry.DeleteTopic(ctx, *conn, topic)
	h.audit(c, conn.ID, "delete_topic", topic, nil, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *KafkaAdminHandler) Brokers(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	brokers, err := h.registry.Brokers(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": brokers})
}

func (h *KafkaAdminHandler) Groups(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	groups, err := h.registry.ListGroups(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": groups})
}

func (h *KafkaAdminHandler) GroupDetail(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	detail, err := h.registry.DescribeGroup(ctx, *conn, c.Param("group"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail})
}

// GroupsLag serves the cluster-wide consumer-group lag summary for the lag
// dashboard. Read access is enough. Optional ?topic=<T> filters to groups
// with lag on that topic.
func (h *KafkaAdminHandler) GroupsLag(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	summaries, err := h.registry.GroupsLag(ctx, *conn, c.Query("topic"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": summaries})
}

// GroupLag serves the full lag detail for a single consumer group. Read
// access is enough.
func (h *KafkaAdminHandler) GroupLag(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.reqCtx(c)
	defer cancel()
	detail, err := h.registry.GroupLag(ctx, *conn, c.Param("group"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail})
}

// audit writes a best-effort activity row. Never blocks the response.
func (h *KafkaAdminHandler) audit(c *gin.Context, connID, action, topic string, detail any, actErr error) {
	userID := c.GetString("userID")
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
	row := models.KafkaActivityHistory{
		ID: uuid.New().String(), UserID: userID, ConnectionID: connID,
		Action: action, Topic: topic, DetailJSON: detailJSON, ErrorText: errText,
		ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// parseInt is a tiny query-param helper for optional integer params.
func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
