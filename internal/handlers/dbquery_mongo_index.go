package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/models"
)

// Mongo index management (MONGO_COMPASS_PLAN §A). Listing is a read every
// accessor may do; create / drop / hide are writes and pass the same gate as
// a write statement: read-write enabled on the connection AND the caller can
// manage it. Every call lands in the query history (the audit trail of the
// DB tool) as the equivalent shell command.

type mongoIndexTarget struct {
	DB   string `json:"db"`
	Coll string `json:"coll"`
}

func (h *DBQueryExecuteHandler) mongoIndexer(c *gin.Context, write bool) (dbquery.MongoIndexer, *models.DBConnection, string, context.Context, context.CancelFunc, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.connsHand.loadAccessible(c.Request.Context(), c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, nil, "", nil, nil, false
	}
	if conn.Kind != "mongo" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "indexes are only available on MongoDB connections"})
		return nil, nil, "", nil, nil, false
	}
	if write {
		if !conn.AllowWrite {
			c.JSON(http.StatusBadRequest, gin.H{"error": "This connection is read-only. Enable read-write from its ⋯ menu to change indexes."})
			return nil, nil, "", nil, nil, false
		}
		if !canManage(role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
			return nil, nil, "", nil, nil, false
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	sess, err := h.registry.Session(ctx, *conn)
	if err != nil {
		cancel()
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, nil, "", nil, nil, false
	}
	ix, ok := sess.(dbquery.MongoIndexer)
	if !ok {
		cancel()
		c.JSON(http.StatusBadRequest, gin.H{"error": "indexes are not supported for this connection"})
		return nil, nil, "", nil, nil, false
	}
	return ix, conn, role, ctx, cancel, true
}

func validTarget(t mongoIndexTarget) bool {
	return strings.TrimSpace(t.Coll) != "" && !strings.ContainsAny(t.Coll+t.DB, "\x00")
}

func shellColl(t mongoIndexTarget) string {
	if t.DB != "" {
		return fmt.Sprintf("db.getSiblingDB(%q).getCollection(%q)", t.DB, t.Coll)
	}
	return fmt.Sprintf("db.getCollection(%q)", t.Coll)
}

// MongoIndexes — GET /db-connections/:id/mongo/indexes?db=&coll=
func (h *DBQueryExecuteHandler) MongoIndexes(c *gin.Context) {
	t := mongoIndexTarget{DB: c.Query("db"), Coll: c.Query("coll")}
	if !validTarget(t) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coll is required"})
		return
	}
	ix, _, _, ctx, cancel, ok := h.mongoIndexer(c, false)
	if !ok {
		return
	}
	defer cancel()
	list, err := ix.MongoIndexes(ctx, t.DB, t.Coll)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

type mongoCreateIndexInput struct {
	mongoIndexTarget
	dbquery.MongoIndexSpec
}

// MongoCreateIndex — POST /db-connections/:id/mongo/indexes
func (h *DBQueryExecuteHandler) MongoCreateIndex(c *gin.Context) {
	var in mongoCreateIndexInput
	if err := c.ShouldBindJSON(&in); err != nil || !validTarget(in.mongoIndexTarget) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coll and keys are required"})
		return
	}
	ix, conn, role, ctx, cancel, ok := h.mongoIndexer(c, true)
	if !ok {
		return
	}
	defer cancel()
	start := time.Now()
	keys, _ := json.Marshal(in.Keys)
	shell := fmt.Sprintf("%s.createIndex(%s, {name: %q, unique: %v, sparse: %v, hidden: %v})", shellColl(in.mongoIndexTarget), keys, in.Name, in.Unique, in.Sparse, in.Hidden)
	name, err := ix.MongoCreateIndex(ctx, in.DB, in.Coll, in.MongoIndexSpec)
	h.recordHistory(c.GetString("userID"), role, conn, shell, 0, int(time.Since(start).Milliseconds()), false, errText(err))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": name}})
}

type mongoIndexNameInput struct {
	mongoIndexTarget
	Name   string `json:"name"`
	Hidden bool   `json:"hidden"`
}

// MongoDropIndex — POST /db-connections/:id/mongo/indexes/drop {db, coll, name}
func (h *DBQueryExecuteHandler) MongoDropIndex(c *gin.Context) { h.mongoIndexNameOp(c, false) }

// MongoHideIndex — POST /db-connections/:id/mongo/indexes/hide {db, coll, name, hidden}
func (h *DBQueryExecuteHandler) MongoHideIndex(c *gin.Context) { h.mongoIndexNameOp(c, true) }

func (h *DBQueryExecuteHandler) mongoIndexNameOp(c *gin.Context, hide bool) {
	var in mongoIndexNameInput
	if err := c.ShouldBindJSON(&in); err != nil || !validTarget(in.mongoIndexTarget) || strings.TrimSpace(in.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coll and name are required"})
		return
	}
	ix, conn, role, ctx, cancel, ok := h.mongoIndexer(c, true)
	if !ok {
		return
	}
	defer cancel()
	start := time.Now()
	var err error
	var shell string
	if hide {
		verb := "unhideIndex"
		if in.Hidden {
			verb = "hideIndex"
		}
		shell = fmt.Sprintf("%s.%s(%q)", shellColl(in.mongoIndexTarget), verb, in.Name)
		err = ix.MongoSetIndexHidden(ctx, in.DB, in.Coll, in.Name, in.Hidden)
	} else {
		shell = fmt.Sprintf("%s.dropIndex(%q)", shellColl(in.mongoIndexTarget), in.Name)
		err = ix.MongoDropIndex(ctx, in.DB, in.Coll, in.Name)
	}
	h.recordHistory(c.GetString("userID"), role, conn, shell, 0, int(time.Since(start).Milliseconds()), false, errText(err))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type mongoPreviewInput struct {
	mongoIndexTarget
	Stages []string `json:"stages"`
	UpTo   int      `json:"upTo"`
	Limit  int      `json:"limit"`
}

// MongoAggregatePreview — POST /db-connections/:id/mongo/aggregate-preview.
// The aggregation builder's per-stage preview: a read every accessor may
// run (writing stages are refused, the result is capped and time-boxed).
func (h *DBQueryExecuteHandler) MongoAggregatePreview(c *gin.Context) {
	var in mongoPreviewInput
	if err := c.ShouldBindJSON(&in); err != nil || !validTarget(in.mongoIndexTarget) || len(in.Stages) == 0 || len(in.Stages) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coll and 1–100 stages are required"})
		return
	}
	ix, _, _, ctx, cancel, ok := h.mongoIndexer(c, false)
	if !ok {
		return
	}
	defer cancel()
	p, ok := ix.(dbquery.MongoPipeliner)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "aggregation preview is not supported for this connection"})
		return
	}
	docs, err := p.MongoAggregatePreview(ctx, in.DB, in.Coll, in.Stages, in.UpTo, in.Limit)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"docs": docs}})
}
