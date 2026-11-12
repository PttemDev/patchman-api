package handlers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DBQueryExecuteHandler runs the user's query and writes audit + history
// rows. Read access is enough — the intent parser refuses everything
// outside SELECT/find-class. Delegated execution: when the caller is
// not the connection owner, we stamp `delegated_owner = owner.id` on
// the history row so admins can trace activity through Alice's
// credentials.
type DBQueryExecuteHandler struct {
	db        *gorm.DB
	registry  *dbquery.Registry
	connsHand *DBConnectionHandler // reused for access checks

	// Active long-running queries — id → cancel func. Lets the FE call
	// `/api/db-query/cancel { id }` to abort a runaway query without
	// killing the entire HTTP request.
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

func NewDBQueryExecuteHandler(db *gorm.DB, reg *dbquery.Registry, conns *DBConnectionHandler) *DBQueryExecuteHandler {
	return &DBQueryExecuteHandler{
		db: db, registry: reg, connsHand: conns,
		cancel: map[string]context.CancelFunc{},
	}
}

const (
	defaultRowLimit = 500
	queryTimeout    = 60 * time.Second
)

type dbExecuteInput struct {
	ConnectionID string `json:"connectionId" binding:"required"`
	Query        string `json:"query" binding:"required"`
	Limit        *int   `json:"limit"`   // omit → defaultRowLimit; 0 → no cap (owner-only)
	Offset       int    `json:"offset"`  // server-side pagination cursor
	QueryID      string `json:"queryId"` // optional client-supplied id for cancel
	// AllowWrite is an AI agent's (MCP) explicit opt-in to a write. People in
	// the app don't send it: turning read-write on is their opt-in.
	AllowWrite bool `json:"allowWrite"`
}

type dbExecuteOutput struct {
	QueryID       string               `json:"queryId"`
	Columns       []dbquery.ColumnMeta `json:"columns"`
	Rows          []dbquery.Row        `json:"rows"`
	RowCount      int                  `json:"rowCount"`
	ExecMs        int                  `json:"execMs"`
	RowsTruncated bool                 `json:"rowsTruncated"`
	Offset        int                  `json:"offset"`
	StatementText string               `json:"statementText,omitempty"`
}

func (h *DBQueryExecuteHandler) Execute(c *gin.Context) {
	userID := c.GetString("userID")
	var input dbExecuteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	conn, role, err := h.connsHand.loadAccessible(c.Request.Context(), input.ConnectionID, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	// Intent enforcement (DB connection permissions v2).
	//
	//   1. A provably read-only statement (SELECT / find-class) is allowed
	//      for EVERY accessor — owner, co-owner, or read-grantee. Unchanged.
	//   2. Anything ValidateReadOnly rejects is either a genuine write/DML
	//      statement OR structurally invalid (bad syntax, a Mongo
	//      multi-statement `;` injection). Structural problems are ALWAYS
	//      rejected here — the AllowWrite switch can never bypass the
	//      injection / multi-statement guard. Only a WELL-FORMED write
	//      statement (IsWriteStatement) reaches the AllowWrite + role gate.
	//   3. A write runs only when the connection has read-write enabled
	//      (conn.AllowWrite) AND the caller can manage it (owner/co-owner).
	//      A read-grantee can never write; nobody can write while AllowWrite
	//      is off — identical to v1's blanket read-only behavior.
	if err := dbquery.ValidateReadOnly(conn.Kind, input.Query); err != nil {
		if !dbquery.IsWriteStatement(conn.Kind, input.Query) {
			// Structural / unsupported statement — surface the precise
			// ValidateReadOnly message (syntax error, Mongo multi-statement,
			// etc.) regardless of AllowWrite.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// A real write statement — gate it.
		if !conn.AllowWrite {
			c.JSON(http.StatusBadRequest, gin.H{"error": "This connection is read-only. Enable read-write from its ⋯ menu to run write statements."})
			return
		}
		if !canManage(role) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "You have read-only access to this shared connection."})
			return
		}
		// An agent must also say it means to write, so a query it meant as a
		// read can never change data by accident (same as es_request /
		// redis_command).
		if c.GetHeader(mcpClientHeader) == mcpClientHeaderValue && !input.AllowWrite {
			c.JSON(http.StatusBadRequest, gin.H{"error": "This statement writes data. Pass allowWrite=true to run it (the connection has read-write enabled)."})
			return
		}
		// AllowWrite on + caller can manage → the write is permitted; fall
		// through to execution.
	}

	limit := defaultRowLimit
	if input.Limit != nil {
		if *input.Limit == 0 {
			// Lifting the row cap is an owner/co-owner affordance.
			if !canManage(role) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner or a co-owner can lift the row limit"})
				return
			}
			limit = 0
		} else if *input.Limit > 0 {
			limit = *input.Limit
		}
	}

	queryID := input.QueryID
	if queryID == "" {
		queryID = uuid.New().String()
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), queryTimeout)
	h.registerCancel(queryID, cancel)
	defer h.unregisterCancel(queryID)
	defer cancel()

	// Multi-statement support — only for SQL. Mongo is single-call by
	// design (the intent parser already rejects unquoted `;`).
	var statements []string
	if conn.Kind == "postgres" || conn.Kind == "mysql" || conn.Kind == "sqlserver" {
		statements = dbquery.SplitSQL(input.Query)
	} else {
		statements = []string{input.Query}
	}
	if len(statements) == 0 {
		statements = []string{input.Query}
	}

	start := time.Now()
	sess, err := h.registry.Session(ctx, *conn)
	if err != nil {
		h.recordHistory(userID, role, conn, input.Query, 0, 0, false, err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	results := make([]dbExecuteOutput, 0, len(statements))
	totalRows := 0
	for stmtIdx, stmt := range statements {
		cur, err := sess.Execute(ctx, stmt, dbquery.ExecOpts{Limit: limit + input.Offset + 1})
		if err != nil {
			ms := int(time.Since(start).Milliseconds())
			h.recordHistory(userID, role, conn, input.Query, totalRows, ms, false, err.Error())
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		rows := []dbquery.Row{}
		truncated := false
		skipped := 0
		for {
			row, err := cur.Next(ctx)
			if err != nil {
				_ = cur.Close()
				ms := int(time.Since(start).Milliseconds())
				h.recordHistory(userID, role, conn, input.Query, totalRows+len(rows), ms, truncated, err.Error())
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			if row == nil {
				break
			}
			if skipped < input.Offset {
				skipped++
				continue
			}
			if limit > 0 && len(rows) >= limit {
				truncated = true
				break
			}
			rows = append(rows, row)
		}
		_ = cur.Close()
		totalRows += len(rows)
		results = append(results, dbExecuteOutput{
			QueryID:       queryID,
			Columns:       cur.Columns(),
			Rows:          rows,
			RowCount:      len(rows),
			RowsTruncated: truncated,
			Offset:        input.Offset,
			StatementText: stmt,
		})
		_ = stmtIdx
	}

	ms := int(time.Since(start).Milliseconds())
	for i := range results {
		results[i].ExecMs = ms / max(1, len(results))
	}
	h.recordHistory(userID, role, conn, input.Query, totalRows, ms, anyTruncated(results), "")

	// Backwards-compat: single-statement responses keep the flat shape.
	if len(results) == 1 {
		c.JSON(http.StatusOK, gin.H{"data": results[0]})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"results": results,
		"queryId": queryID,
		"execMs":  ms,
	}})
}

func anyTruncated(rs []dbExecuteOutput) bool {
	for _, r := range rs {
		if r.RowsTruncated {
			return true
		}
	}
	return false
}

func max(a, b int) int { if a > b { return a }; return b }

func (h *DBQueryExecuteHandler) Cancel(c *gin.Context) {
	var input struct {
		QueryID string `json:"queryId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	cancel, ok := h.cancel[input.QueryID]
	h.mu.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Query not running"})
		return
	}
	cancel()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"cancelled": true}})
}

func (h *DBQueryExecuteHandler) registerCancel(id string, fn context.CancelFunc) {
	h.mu.Lock()
	h.cancel[id] = fn
	h.mu.Unlock()
}

func (h *DBQueryExecuteHandler) unregisterCancel(id string) {
	h.mu.Lock()
	delete(h.cancel, id)
	h.mu.Unlock()
}

func (h *DBQueryExecuteHandler) recordHistory(userID, role string, conn *models.DBConnection, query string, rowCount, execMs int, truncated bool, errText string) {
	excerpt := query
	if len(excerpt) > 2048 {
		excerpt = excerpt[:2048]
	}
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	row := models.DBQueryHistory{
		ID:             uuid.New().String(),
		UserID:         userID,
		DelegatedOwner: delegated,
		ConnectionID:   conn.ID,
		Kind:           conn.Kind,
		QueryText:      query,
		QueryExcerpt:   excerpt,
		RowCount:       rowCount,
		ExecMs:         execMs,
		RowsTruncated:  truncated,
		ErrorText:      errText,
		ExecutedAt:     time.Now(),
	}
	// Fire-and-forget: a failed history insert shouldn't ever break
	// the user's actual query path.
	go h.db.Create(&row)
}

