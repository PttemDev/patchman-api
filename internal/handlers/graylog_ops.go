package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/graylogquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GraylogOpsHandler serves the Graylog read surface: streams, log search
// (messages + histogram) and the dashboards / saved searches list. Any
// access may read — PatchMan never changes Graylog. Searches are audited
// (log data can be sensitive); browsing lists is not.
type GraylogOpsHandler struct {
	db       *gorm.DB
	registry *graylogquery.Registry
	conns    *GraylogConnectionHandler
}

func NewGraylogOpsHandler(db *gorm.DB, reg *graylogquery.Registry, conns *GraylogConnectionHandler) *GraylogOpsHandler {
	return &GraylogOpsHandler{db: db, registry: reg, conns: conns}
}

func (h *GraylogOpsHandler) access(c *gin.Context) (*models.GraylogConnection, string, bool) {
	conn, role, err := h.conns.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	return conn, role, true
}

// fail answers a Graylog failure. Auth problems on the Graylog side map to
// 502 (never 401/403, which the FE treats as its own session failing).
func (h *GraylogOpsHandler) fail(c *gin.Context, err error) {
	if ae, ok := graylogquery.AsAPIError(err); ok {
		status := http.StatusBadGateway
		switch ae.Code {
		case "bad_request", "bad_query":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "unreachable":
			status = http.StatusGatewayTimeout
		}
		c.JSON(status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "Graylog did not answer in time", "code": "unreachable"})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
}

func (h *GraylogOpsHandler) audit(c *gin.Context, conn *models.GraylogConnection, role, action, target string, actErr error) {
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
	row := models.GraylogActivityHistory{
		ID: uuid.New().String(), UserID: c.GetString("userID"), DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// Streams lists the streams the connection's Graylog user can read.
// GET /graylog/:id/streams
func (h *GraylogOpsHandler) Streams(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	streams, err := h.registry.Streams(ctx, *conn)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": streams})
}

// Search runs a log search. POST /graylog/:id/search {query, range, streams,
// limit, offset, sort, noHistogram}. The answer carries webUrl — the same
// search in Graylog's own UI.
func (h *GraylogOpsHandler) Search(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	var req graylogquery.SearchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "bad_request"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	res, err := h.registry.Search(ctx, *conn, req)
	h.audit(c, conn, role, "search", searchTarget(req), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	base, _ := graylogquery.BaseURL(conn.URL)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"total": res.Total, "messages": res.Messages, "histogram": res.Histogram, "interval": res.Interval,
		"from": res.From, "to": res.To, "tookMs": res.TookMs, "webUrl": graylogquery.WebSearchURL(base, req),
	}})
}

func searchTarget(r graylogquery.SearchRequest) string {
	var b strings.Builder
	b.WriteString(r.Query)
	switch r.Range.Type {
	case "absolute":
		b.WriteString(" | " + r.Range.From + " → " + r.Range.To)
	case "keyword":
		b.WriteString(" | " + r.Range.Keyword)
	default:
		b.WriteString(" | last " + (time.Duration(r.Range.Range) * time.Second).String())
	}
	if len(r.Streams) > 0 {
		b.WriteString(" | streams " + strings.Join(r.Streams, ","))
	}
	return b.String()
}

// Views lists dashboards and saved searches with their Graylog links.
// GET /graylog/:id/views?query=
func (h *GraylogOpsHandler) Views(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	views, err := h.registry.Views(ctx, *conn, c.Query("query"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views})
}
