package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/rabbitquery"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RabbitOpsHandler serves the broker surface. vhost and queue / exchange
// names travel as query params or in the body (never path segments: gin
// would decode the default vhost's "%2F"). Any access may read, peek (get
// with requeue), read streams and tail; changing the broker — publish,
// purge, remove, move, create/delete, bind, close connection — needs the
// owner or a write grant. Everything that touches messages is audited.
type RabbitOpsHandler struct {
	db       *gorm.DB
	registry *rabbitquery.Registry
	conns    *RabbitConnectionHandler
}

func NewRabbitOpsHandler(db *gorm.DB, reg *rabbitquery.Registry, conns *RabbitConnectionHandler) *RabbitOpsHandler {
	return &RabbitOpsHandler{db: db, registry: reg, conns: conns}
}

func (h *RabbitOpsHandler) access(c *gin.Context, needWrite bool) (*models.RabbitConnection, string, bool) {
	conn, role, err := h.conns.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Read-only access: this action needs write access to the RabbitMQ connection.", "code": "read_only"})
		return nil, "", false
	}
	return conn, role, true
}

func (h *RabbitOpsHandler) fail(c *gin.Context, err error) {
	if ae, ok := rabbitquery.AsAPIError(err); ok {
		status := http.StatusBadGateway
		switch ae.Code {
		case "not_found":
			status = http.StatusNotFound
		case "bad_request":
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "RabbitMQ did not answer in time"})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
}

func (h *RabbitOpsHandler) audit(c *gin.Context, conn *models.RabbitConnection, role, action, target string, actErr error) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	if len(target) > 1024 {
		target = target[:1024]
	}
	row := models.RabbitActivityHistory{
		ID: uuid.New().String(), UserID: c.GetString("userID"), DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

func ctxFor(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}

func raw(c *gin.Context, b []byte) {
	if len(b) == 0 {
		b = []byte("null")
	}
	c.JSON(http.StatusOK, gin.H{"data": json.RawMessage(b)})
}

func listParams(c *gin.Context) rabbitquery.ListParams {
	page, _ := strconv.Atoi(c.Query("page"))
	size, _ := strconv.Atoi(c.Query("pageSize"))
	return rabbitquery.ListParams{
		Vhost: c.Query("vhost"), Page: page, PageSize: size, Name: c.Query("name"),
		Regex: c.Query("regex") == "1" || c.Query("regex") == "true", Sort: c.Query("sort"),
		SortReverse: c.Query("reverse") == "1" || c.Query("reverse") == "true",
	}
}

func target(vhost, name string) string { return vhost + " :: " + name }

// ── reads ───────────────────────────────────────────────────────────

func (h *RabbitOpsHandler) Overview(c *gin.Context) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	ov, err := h.registry.Overview(ctx, *conn)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ov})
}

func (h *RabbitOpsHandler) listing(c *gin.Context, fn func(context.Context, models.RabbitConnection, rabbitquery.ListParams) (json.RawMessage, error)) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	b, err := fn(ctx, *conn, listParams(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	raw(c, b)
}

func (h *RabbitOpsHandler) Queues(c *gin.Context)      { h.listing(c, h.registry.Queues) }
func (h *RabbitOpsHandler) Exchanges(c *gin.Context)   { h.listing(c, h.registry.Exchanges) }
func (h *RabbitOpsHandler) Connections(c *gin.Context) { h.listing(c, h.registry.Connections) }
func (h *RabbitOpsHandler) Channels(c *gin.Context)    { h.listing(c, h.registry.Channels) }

func (h *RabbitOpsHandler) vhostRead(c *gin.Context, fn func(context.Context, models.RabbitConnection, string) (json.RawMessage, error)) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	b, err := fn(ctx, *conn, c.Query("vhost"))
	if err != nil {
		h.fail(c, err)
		return
	}
	raw(c, b)
}

func (h *RabbitOpsHandler) Consumers(c *gin.Context) { h.vhostRead(c, h.registry.Consumers) }
func (h *RabbitOpsHandler) Policies(c *gin.Context)  { h.vhostRead(c, h.registry.Policies) }

func (h *RabbitOpsHandler) Queue(c *gin.Context) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	d, err := h.registry.Queue(ctx, *conn, c.Query("vhost"), c.Query("name"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

func (h *RabbitOpsHandler) Exchange(c *gin.Context) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	d, err := h.registry.Exchange(ctx, *conn, c.Query("vhost"), c.Query("name"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

func (h *RabbitOpsHandler) Topology(c *gin.Context) {
	conn, _, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	t, err := h.registry.Topology(ctx, *conn, c.Query("vhost"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": t})
}

// ── messages ───────────────────────────────────────────────────────

// GetMessages peeks (requeue — any access) or removes (write) messages.
func (h *RabbitOpsHandler) GetMessages(c *gin.Context) {
	var in struct {
		Vhost    string `json:"vhost"`
		Queue    string `json:"queue"`
		Count    int    `json:"count"`
		Remove   bool   `json:"remove"`
		Truncate int    `json:"truncate"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, in.Remove)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 60*time.Second)
	defer cancel()
	b, err := h.registry.GetMessages(ctx, *conn, rabbitquery.GetParams{Vhost: in.Vhost, Queue: in.Queue, Count: in.Count, Remove: in.Remove, Truncate: in.Truncate})
	action := "peek"
	if in.Remove {
		action = "get_remove"
	}
	h.audit(c, conn, role, action, fmt.Sprintf("%s (%d)", target(in.Vhost, in.Queue), in.Count), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	raw(c, b)
}

// StreamRead reads a stream queue from an offset (no side effects).
func (h *RabbitOpsHandler) StreamRead(c *gin.Context) {
	var in struct {
		Vhost  string `json:"vhost"`
		Queue  string `json:"queue"`
		Offset string `json:"offset"`
		Count  int    `json:"count"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	msgs, err := h.registry.StreamRead(ctx, *conn, rabbitquery.StreamParams{Vhost: in.Vhost, Queue: in.Queue, Offset: in.Offset, Count: in.Count})
	h.audit(c, conn, role, "stream_read", fmt.Sprintf("%s @%s (%d)", target(in.Vhost, in.Queue), in.Offset, in.Count), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": msgs})
}

// Tail streams an exchange's messages as SSE through a temporary exclusive
// queue: events open | message | ping | error | end.
func (h *RabbitOpsHandler) Tail(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	p := rabbitquery.TailParams{Vhost: c.Query("vhost"), Exchange: c.Query("exchange"), RoutingKey: c.Query("routingKey")}
	if a := c.Query("arguments"); strings.TrimSpace(a) != "" {
		if err := json.Unmarshal([]byte(a), &p.Arguments); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "arguments must be a JSON object"})
			return
		}
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	writeSSEEvent(c.Writer, "open", `{}`)
	c.Writer.Flush()
	h.audit(c, conn, role, "tail", target(p.Vhost, p.Exchange)+" key="+p.RoutingKey, nil)
	emit := func(m rabbitquery.Message) error {
		b, err := json.Marshal(m)
		if err != nil {
			return nil
		}
		writeSSEEvent(c.Writer, "message", string(b))
		c.Writer.Flush()
		return c.Request.Context().Err()
	}
	ping := func() error {
		writeSSEEvent(c.Writer, "ping", `{}`)
		c.Writer.Flush()
		return c.Request.Context().Err()
	}
	err := h.registry.Tail(c.Request.Context(), *conn, p, emit, ping)
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":%q}`, err.Error()))
	}
	writeSSEEvent(c.Writer, "end", `{}`)
	c.Writer.Flush()
}

// ── changes (write) ────────────────────────────────────────────────

func (h *RabbitOpsHandler) Publish(c *gin.Context) {
	var in rabbitquery.PublishParams
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	routed, err := h.registry.Publish(ctx, *conn, in)
	ex := in.Exchange
	if ex == "" {
		ex = "(default)"
	}
	h.audit(c, conn, role, "publish", target(in.Vhost, ex)+" key="+in.RoutingKey, err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"routed": routed}})
}

func (h *RabbitOpsHandler) Purge(c *gin.Context) {
	var in struct {
		Vhost string `json:"vhost"`
		Queue string `json:"queue"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.Purge(ctx, *conn, in.Vhost, in.Queue)
	h.audit(c, conn, role, "purge", target(in.Vhost, in.Queue), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"purged": true}})
}

func (h *RabbitOpsHandler) Move(c *gin.Context) {
	var in rabbitquery.MoveParams
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 5*time.Minute)
	defer cancel()
	res, err := h.registry.Move(ctx, *conn, in)
	dest := in.TargetQueue
	if in.Target == "exchange" {
		dest = in.TargetExchange + " key=" + in.TargetRoutingKey
	} else if in.Target == "origin" {
		dest = "(x-death origin)"
	}
	moved := 0
	if res != nil {
		moved = res.Moved
	}
	h.audit(c, conn, role, "move", fmt.Sprintf("%s → %s (%d moved)", target(in.Vhost, in.Source), dest, moved), err)
	if err != nil {
		if res != nil && res.Moved > 0 {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"moved": res.Moved, "stopped": err.Error()}})
			return
		}
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *RabbitOpsHandler) CreateQueue(c *gin.Context) {
	var in rabbitquery.QueueSpec
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.CreateQueue(ctx, *conn, in)
	h.audit(c, conn, role, "create_queue", target(in.Vhost, in.Name), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"created": true}})
}

func (h *RabbitOpsHandler) DeleteQueue(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	vhost, name := c.Query("vhost"), c.Query("name")
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.DeleteQueue(ctx, *conn, vhost, name, c.Query("ifEmpty") == "1", c.Query("ifUnused") == "1")
	h.audit(c, conn, role, "delete_queue", target(vhost, name), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func (h *RabbitOpsHandler) CreateExchange(c *gin.Context) {
	var in rabbitquery.ExchangeSpec
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.CreateExchange(ctx, *conn, in)
	h.audit(c, conn, role, "create_exchange", target(in.Vhost, in.Name), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"created": true}})
}

func (h *RabbitOpsHandler) DeleteExchange(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	vhost, name := c.Query("vhost"), c.Query("name")
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.DeleteExchange(ctx, *conn, vhost, name)
	h.audit(c, conn, role, "delete_exchange", target(vhost, name), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func (h *RabbitOpsHandler) Bind(c *gin.Context) {
	var in rabbitquery.BindingSpec
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.Bind(ctx, *conn, in)
	h.audit(c, conn, role, "bind", fmt.Sprintf("%s → %s key=%s", target(in.Vhost, in.Source), in.Destination, in.RoutingKey), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"bound": true}})
}

func (h *RabbitOpsHandler) Unbind(c *gin.Context) {
	var in rabbitquery.BindingSpec
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.Unbind(ctx, *conn, in)
	h.audit(c, conn, role, "unbind", fmt.Sprintf("%s → %s key=%s", target(in.Vhost, in.Source), in.Destination, in.RoutingKey), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"unbound": true}})
}

func (h *RabbitOpsHandler) CloseConnection(c *gin.Context) {
	var in struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	ctx, cancel := ctxFor(c, 30*time.Second)
	defer cancel()
	err := h.registry.CloseConnection(ctx, *conn, in.Name, in.Reason)
	h.audit(c, conn, role, "close_connection", in.Name, err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"closed": true}})
}
