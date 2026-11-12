package handlers

import (
	"database/sql/driver"
	"net/http"
	"sort"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Per-user usage for the admin dashboard (GET /api/admin/users/:id/usage).
//
// Metadata only — counts, timestamps, MCP client names, agent names. Never
// content (request bodies, note text, secrets). Every number comes from a
// grouped COUNT/MAX per table; nothing loops per row. Most filter on an
// indexed user column; two don't: notes by created_by/updated_by (the actor,
// not the indexed tree owner) and git_activity_history.user_id. Both scan
// their table once, and only when an admin expands a row.

// usageRoutes maps an API path prefix (without "/api") to a product area
// ("tool"). The audit log has no area column — its `resource` keeps the LAST
// known path segment (so /kafka-connections/:id/grants reads "grant", and
// notes/whiteboards/kafka/… read "unknown") — so areas are derived from the
// logged path in `details` ("POST /api/notes/…"): SQL groups the user's rows
// by the first path segment (plus the second one for the two-segment
// prefixes below) and Go maps each group through this table. Prefixes are one
// or two segments deep; a two-segment one wins over its parent.
var usageRoutes = []struct{ prefix, tool string }{
	{"/notes/memory", "memory"},
	{"/notes/remember", "memory"},
	{"/notes/recall", "memory"},
	{"/notes/suggestions", "memory"},
	{"/notes/inbox", "memory"},
	{"/import/junit", "qa"},
	{"/integrations/gitlab", "git"},
	// Sent HTTP requests (server proxy) and their history.
	{"/proxy", "history"},
	{"/history", "history"},
	{"/whiteboards", "whiteboards"},
	{"/requests", "requests"},
	{"/collections", "requests"},
	{"/notes", "notes"},
	{"/environments", "environments"},
	{"/runner", "requests"},
	{"/saved-responses", "requests"},
	{"/cookies", "requests"},
	{"/import", "requests"},
	{"/codegen", "requests"},
	{"/script", "requests"},
	{"/ws", "requests"},
	{"/sse", "requests"},
	{"/db-query", "db"},
	{"/db-connections", "db"},
	{"/saved-db-queries", "db"},
	{"/kafka", "kafka"},
	{"/kafka-connections", "kafka"},
	{"/redis", "redis"},
	{"/redis-connections", "redis"},
	{"/es", "es"},
	{"/es-connections", "es"},
	{"/k8s", "k8s"},
	{"/k8s-connections", "k8s"},
	{"/s3", "s3"},
	{"/s3-connections", "s3"},
	{"/rabbit", "rabbitmq"},
	{"/rabbit-connections", "rabbitmq"},
	{"/graylog", "graylog"},
	{"/graylog-connections", "graylog"},
	{"/grafana", "grafana"},
	{"/grafana-connections", "grafana"},
	{"/service-map", "observability"},
	{"/observability-connections", "observability"},
	{"/observability-providers", "observability"},
	{"/git", "git"},
	{"/mock-servers", "mocks"},
	{"/mock-routes", "mocks"},
	{"/test-cases", "qa"},
	{"/test-plans", "qa"},
	{"/test-runs", "qa"},
	{"/test-folders", "qa"},
	{"/requirements", "qa"},
	{"/defects", "qa"},
	{"/shared-steps", "qa"},
	{"/load-tests", "loadtests"},
	{"/llm-providers", "ai"},
	{"/llm", "ai"},
	{"/ai", "ai"},
	{"/mcp", "mcp"},
	{"/workspaces", "collab"},
	{"/teams", "collab"},
	{"/comments", "collab"},
	{"/devtools", "devtools"},
	{"/sync", "sync"},
}

// usageOtherTool collects audited calls no prefix above claims (crash
// reports, …) so lastActivityAt still sees them.
const usageOtherTool = "other"

var (
	usageRouteIndex = map[string]string{} // "notes" / "notes/memory" → tool
	usageNested     []string              // the two-segment prefixes, "notes/memory"
)

func init() {
	for _, r := range usageRoutes {
		key := strings.TrimPrefix(r.prefix, "/")
		usageRouteIndex[key] = r.tool
		if strings.Contains(key, "/") {
			usageNested = append(usageNested, key)
		}
	}
}

// usageToolFor maps an audited path's first segment (and, for a two-segment
// prefix, its second) to a tool.
func usageToolFor(seg1, seg2 string) string {
	if seg2 != "" {
		if t, ok := usageRouteIndex[seg1+"/"+seg2]; ok {
			return t
		}
	}
	if t, ok := usageRouteIndex[seg1]; ok {
		return t
	}
	return usageOtherTool
}

// auditSegmentsSQL returns, per dialect, the expressions that cut an audit
// row's details ("POST /api/notes/x/links") down to r1 ("notes/x/links") and
// then to its first and second path segments (seg1/seg2 are written over the
// column r1). Postgres has split_part; SQLite only has instr/substr. Both are
// plain string functions per row, then one hash/sort aggregate — no per-row
// LIKE chain.
func auditSegmentsSQL(dialect string) (r1, seg1, seg2 string) {
	if dialect == "postgres" {
		return "substr(details, strpos(details, '/api/') + 5)",
			"split_part(r1, '/', 1)",
			"split_part(r1, '/', 2)"
	}
	rest := "substr(r1, instr(r1 || '/', '/') + 1)"
	return "substr(details, instr(details, '/api/') + 5)",
		"substr(r1, 1, instr(r1 || '/', '/') - 1)",
		"substr(" + rest + ", 1, instr(" + rest + " || '/', '/') - 1)"
}

// usageSource is one per-user table feeding a tool row: either things the
// user owns ("items": connections, boards, …) or a ledger of what they did
// ("ops": queries run, messages consumed, requests sent). timeCol is the
// column that dates the user's own action (created_at for owned items — an
// updated_at can be a collaborator's edit).
type usageSource struct {
	tool    string
	model   any
	userCol string
	timeCol string
	cond    string
	ops     bool
}

var usageSources = []usageSource{
	{tool: "environments", model: &models.Environment{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "mocks", model: &models.MockServer{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "history", model: &models.History{}, userCol: "user_id", timeCol: "created_at", ops: true},
	{tool: "whiteboards", model: &models.Whiteboard{}, userCol: "user_id", timeCol: "created_at", cond: "type <> 'folder'"},
	{tool: "qa", model: &models.TestCase{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "qa", model: &models.TestRun{}, userCol: "user_id", timeCol: "created_at", ops: true},
	{tool: "loadtests", model: &models.LoadTest{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "db", model: &models.DBConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "db", model: &models.DBQueryHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "kafka", model: &models.KafkaConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "kafka", model: &models.KafkaActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "s3", model: &models.S3Connection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "s3", model: &models.S3ActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "redis", model: &models.RedisConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "redis", model: &models.RedisActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "es", model: &models.ESConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "es", model: &models.ESActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "k8s", model: &models.K8sConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "k8s", model: &models.K8sActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "grafana", model: &models.GrafanaConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "grafana", model: &models.GrafanaActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "rabbitmq", model: &models.RabbitConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "rabbitmq", model: &models.RabbitActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "graylog", model: &models.GraylogConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "graylog", model: &models.GraylogActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "observability", model: &models.ObservabilityConnection{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "observability", model: &models.ObservabilityActivityHistory{}, userCol: "user_id", timeCol: "executed_at", ops: true},
	{tool: "git", model: &models.GitConnection{}, userCol: "user_id", timeCol: "created_at"},
	// git_activity_history has no user_id index; the table is small (clone /
	// pull / push events) and this runs only when an admin opens a row.
	{tool: "git", model: &models.GitActivityHistory{}, userCol: "user_id", timeCol: "created_at", ops: true},
	{tool: "ai", model: &models.LLMProvider{}, userCol: "user_id", timeCol: "created_at"},
	{tool: "collab", model: &models.Workspace{}, userCol: "owner_id", timeCol: "created_at"},
}

type adminToolUsage struct {
	Tool string `json:"tool"`
	// Items the user owns in this tool (connections, boards, test cases, …).
	Items int64 `json:"items"`
	// Actions are audit-log rows: API writes and POST operations (UI and MCP).
	Actions int64 `json:"actions"`
	// Operations are rows in the tool's own ledger (queries run, messages
	// consumed, requests sent — reads included). A UI-run POST op can be in
	// both ledgers, so the two are reported apart, never summed.
	Operations int64      `json:"operations"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

type adminUsageCounts struct {
	Collections  int64 `json:"collections"`
	Requests     int64 `json:"requests"`
	MockServers  int64 `json:"mockServers"`
	MockRoutes   int64 `json:"mockRoutes"`
	Environments int64 `json:"environments"`
	History      int64 `json:"history"`
	Notes        int64 `json:"notes"`
	Cards        int64 `json:"cards"`
	Whiteboards  int64 `json:"whiteboards"`
	TestCases    int64 `json:"testCases"`
	Workspaces   int64 `json:"workspaces"`
	Teams        int64 `json:"teams"`
	MCPTokens    int64 `json:"mcpTokens"`
}

type adminMCPClient struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ClientName string     `json:"clientName,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

type adminAgentUsage struct {
	// Agent is the MCP client that wrote the cards; "" = written by the person.
	Agent  string     `json:"agent"`
	Cards  int64      `json:"cards"`
	LastAt *time.Time `json:"lastAt,omitempty"`
}

type adminUserUsage struct {
	UserID          string            `json:"userId"`
	LastActivityAt  *time.Time        `json:"lastActivityAt,omitempty"`
	LastLoginAt     *time.Time        `json:"lastLoginAt,omitempty"`
	LastSeenAt      *time.Time        `json:"lastSeenAt,omitempty"`
	LastSeenVersion string            `json:"lastSeenVersion,omitempty"`
	HasGitLab       bool              `json:"hasGitlab"`
	Counts          adminUsageCounts  `json:"counts"`
	Tools           []adminToolUsage  `json:"tools"`
	MCPClients      []adminMCPClient  `json:"mcpClients"`
	Agents          []adminAgentUsage `json:"agents"`
	GeneratedAt     time.Time         `json:"generatedAt"`
}

// UserUsage is GET /api/admin/users/:id/usage — one user's per-tool usage,
// fetched when an admin expands the user's row.
func (h *AdminHandler) UserUsage(c *gin.Context) {
	var u models.User
	if err := h.db.Session(&gorm.Session{}).First(&u, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.userUsage(&u)})
}

func (h *AdminHandler) userUsage(u *models.User) adminUserUsage {
	fresh := func() *gorm.DB { return h.db.Session(&gorm.Session{}) }
	tools := map[string]*adminToolUsage{}
	tool := func(key string) *adminToolUsage {
		t := tools[key]
		if t == nil {
			t = &adminToolUsage{Tool: key}
			tools[key] = t
		}
		return t
	}
	out := adminUserUsage{
		UserID:          u.ID,
		LastLoginAt:     u.LastLoginAt,
		LastSeenAt:      u.LastSeenAt,
		LastSeenVersion: u.LastSeenVersion,
		MCPClients:      []adminMCPClient{},
		Agents:          []adminAgentUsage{},
		GeneratedAt:     time.Now().UTC(),
	}

	// 1. Audit log → actions per tool: one grouped scan of the user's rows
	// (index on user_id), grouped by path segment; Go maps segments → tools.
	r1, seg1, seg2 := auditSegmentsSQL(h.db.Dialector.Name())
	var auditRows []struct {
		Seg1   string
		Seg2   string
		N      int64
		LastAt flexTime
	}
	fresh().Raw(`SELECT seg1, seg2, COUNT(*) AS n, MAX(created_at) AS last_at FROM (
		SELECT seg1, CASE WHEN seg1 || '/' || seg2 IN ? THEN seg2 ELSE '' END AS seg2, created_at FROM (
			SELECT `+seg1+` AS seg1, `+seg2+` AS seg2, created_at FROM (
				SELECT `+r1+` AS r1, created_at FROM audit_logs WHERE user_id = ?
			) a
		) b
	) c GROUP BY seg1, seg2`, usageNested, u.ID).Scan(&auditRows)
	for _, r := range auditRows {
		t := tool(usageToolFor(r.Seg1, r.Seg2))
		t.Actions += r.N
		t.LastUsedAt = laterOf(t.LastUsedAt, r.LastAt.Ptr())
	}

	// 2. Owned items and tool ledgers: one COUNT/MAX per table. A table that
	// is missing (or errors) just contributes nothing.
	for _, s := range usageSources {
		var row struct {
			N      int64
			LastAt flexTime
		}
		q := fresh().Model(s.model).Select("COUNT(*) AS n, MAX("+s.timeCol+") AS last_at").Where(s.userCol+" = ?", u.ID)
		if s.cond != "" {
			q = q.Where(s.cond)
		}
		if q.Scan(&row).Error != nil || row.N == 0 {
			continue
		}
		t := tool(s.tool)
		if s.ops {
			t.Operations += row.N
		} else {
			t.Items += row.N
		}
		t.LastUsedAt = laterOf(t.LastUsedAt, row.LastAt.Ptr())
		switch {
		case s.tool == "environments":
			out.Counts.Environments = row.N
		case s.tool == "mocks":
			out.Counts.MockServers = row.N
		case s.tool == "history":
			out.Counts.History = row.N
		case s.tool == "whiteboards":
			out.Counts.Whiteboards = row.N
		case s.tool == "qa" && !s.ops:
			out.Counts.TestCases = row.N
		case s.tool == "collab":
			out.Counts.Workspaces = row.N
		}
	}

	// 3. Collections + requests. Requests carry no user_id — they belong to
	// the owner of their collection (same join the old roster column used).
	var coll struct {
		N, Folders int64
		LastAt     flexTime
	}
	if fresh().Model(&models.Collection{}).
		Select("COUNT(*) AS n, SUM(CASE WHEN type = 'folder' THEN 1 ELSE 0 END) AS folders, MAX(created_at) AS last_at").
		Where("user_id = ?", u.ID).Scan(&coll).Error == nil && coll.N > 0 {
		out.Counts.Collections = coll.N - coll.Folders
		t := tool("requests")
		t.LastUsedAt = laterOf(t.LastUsedAt, coll.LastAt.Ptr())
	}
	var reqs struct {
		N      int64
		LastAt flexTime
	}
	if fresh().Table("saved_requests AS r").Select("COUNT(*) AS n, MAX(r.created_at) AS last_at").
		Joins("JOIN collections AS c ON c.id = r.collection_id").
		Where("c.user_id = ?", u.ID).Scan(&reqs).Error == nil && reqs.N > 0 {
		out.Counts.Requests = reqs.N
		t := tool("requests")
		t.Items += reqs.N
		t.LastUsedAt = laterOf(t.LastUsedAt, reqs.LastAt.Ptr())
	}
	var routes int64
	fresh().Table("mock_routes AS r").Joins("JOIN mock_servers AS s ON s.id = r.mock_server_id").
		Where("s.user_id = ?", u.ID).Count(&routes)
	out.Counts.MockRoutes = routes

	// 4. Notes and agent-memory cards. notes.user_id is the owner of the TREE;
	// created_by / updated_by are the actor, so a card the user's agent filed
	// into a team memory folder still counts for them.
	var noteRows []struct {
		Kind        string
		Agent       string
		N           int64
		CreatedLast flexTime
		UpdatedLast flexTime
	}
	fresh().Raw(`SELECT kind, agent, SUM(mine) AS n, MAX(created_last) AS created_last, MAX(updated_last) AS updated_last FROM (
		SELECT CASE WHEN type = 'card' THEN 'card' WHEN type = 'folder' THEN 'folder' ELSE 'page' END AS kind,
			COALESCE(agent, '') AS agent,
			CASE WHEN created_by = ? THEN 1 ELSE 0 END AS mine,
			CASE WHEN created_by = ? THEN created_at END AS created_last,
			CASE WHEN updated_by = ? THEN updated_at END AS updated_last
		FROM notes WHERE deleted_at IS NULL AND (created_by = ? OR updated_by = ?)
	) n GROUP BY kind, agent`, u.ID, u.ID, u.ID, u.ID, u.ID).Scan(&noteRows)
	for _, r := range noteRows {
		last := laterOf(r.CreatedLast.Ptr(), r.UpdatedLast.Ptr())
		if r.Kind == "card" {
			t := tool("memory")
			t.Items += r.N
			t.LastUsedAt = laterOf(t.LastUsedAt, last)
			out.Counts.Cards += r.N
			if r.N > 0 {
				out.Agents = append(out.Agents, adminAgentUsage{Agent: r.Agent, Cards: r.N, LastAt: r.CreatedLast.Ptr()})
			}
			continue
		}
		t := tool("notes")
		if r.Kind == "page" {
			t.Items += r.N
			out.Counts.Notes += r.N
		}
		t.LastUsedAt = laterOf(t.LastUsedAt, last)
	}
	// Newest first (NULL and '' agents were merged by the COALESCE above).
	sort.SliceStable(out.Agents, func(i, j int) bool { return timeAfter(out.Agents[i].LastAt, out.Agents[j].LastAt) })

	// 5. MCP tokens: one row per token (client name + last use).
	var toks []struct {
		ID         string
		Name       string
		ClientName string
		CreatedAt  flexTime
		LastUsedAt flexTime
	}
	fresh().Model(&models.MCPToken{}).Select("id, name, client_name, created_at, last_used_at").
		Where("user_id = ?", u.ID).Scan(&toks)
	for _, tk := range toks {
		out.MCPClients = append(out.MCPClients, adminMCPClient{
			ID: tk.ID, Name: tk.Name, ClientName: tk.ClientName,
			CreatedAt: tk.CreatedAt.Ptr(), LastUsedAt: tk.LastUsedAt.Ptr(),
		})
	}
	if len(toks) > 0 {
		t := tool("mcp")
		t.Items += int64(len(toks))
		for _, c := range out.MCPClients {
			t.LastUsedAt = laterOf(t.LastUsedAt, c.LastUsedAt)
		}
		out.Counts.MCPTokens = int64(len(toks))
	}
	sort.SliceStable(out.MCPClients, func(i, j int) bool {
		return timeAfter(out.MCPClients[i].LastUsedAt, out.MCPClients[j].LastUsedAt)
	})

	// 6. Teams (membership in live teams) and the GitLab link.
	var teams int64
	fresh().Table("team_members AS m").Joins("JOIN teams AS t ON t.id = m.team_id").
		Where("m.user_id = ? AND t.deleted_at IS NULL", u.ID).Count(&teams)
	out.Counts.Teams = teams
	var gitlab int64
	fresh().Model(&models.UserGitIntegration{}).Where("user_id = ?", u.ID).Count(&gitlab)
	out.HasGitLab = gitlab > 0

	// lastActivityAt: the latest of every signal — app open / login on the
	// user row, every audited API write (incl. MCP-driven ones), every tool
	// ledger, owned-item creation, notes edits and MCP token use.
	last := laterOf(u.LastSeenAt, u.LastLoginAt)
	out.Tools = make([]adminToolUsage, 0, len(tools))
	for _, t := range tools {
		last = laterOf(last, t.LastUsedAt)
		out.Tools = append(out.Tools, *t)
	}
	out.LastActivityAt = last
	sort.SliceStable(out.Tools, func(i, j int) bool {
		a, b := out.Tools[i], out.Tools[j]
		if timeAfter(a.LastUsedAt, b.LastUsedAt) {
			return true
		}
		if timeAfter(b.LastUsedAt, a.LastUsedAt) {
			return false
		}
		return a.Tool < b.Tool
	})
	return out
}

// lastActivityByUser returns each user's latest activity from the cheap
// signals: audited API writes, sent-request history and MCP token use. One
// query; each correlated MAX is an index probe on (user_id, created_at), so
// the cost grows with the number of users, not with the log size. The user
// row's last_seen_at / last_login_at are merged in by the caller. The
// per-user usage endpoint adds every tool ledger on top of these.
func (h *AdminHandler) lastActivityByUser() map[string]*time.Time {
	var rows []struct {
		ID        string
		AuditAt   flexTime
		HistoryAt flexTime
		MCPAt     flexTime `gorm:"column:mcp_at"`
	}
	stmt := h.db.Session(&gorm.Session{})
	err := stmt.Raw(`SELECT u.id AS id,
		(SELECT MAX(a.created_at) FROM audit_logs a WHERE a.user_id = u.id) AS audit_at,
		(SELECT MAX(hi.created_at) FROM ` + tableOf(h.db, &models.History{}) + ` hi WHERE hi.user_id = u.id) AS history_at,
		(SELECT MAX(m.last_used_at) FROM mcp_tokens m WHERE m.user_id = u.id) AS mcp_at
		FROM users u`).Scan(&rows).Error
	out := make(map[string]*time.Time, len(rows))
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = laterOf(laterOf(r.AuditAt.Ptr(), r.HistoryAt.Ptr()), r.MCPAt.Ptr())
	}
	return out
}

// tableOf resolves a model's table name through GORM's naming strategy
// (History → "histories") so raw SQL can't drift from the migrated schema.
func tableOf(db *gorm.DB, model any) string {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil || stmt.Schema == nil {
		return ""
	}
	return stmt.Schema.Table
}

func laterOf(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil || !b.After(*a) {
		return a
	}
	return b
}

// timeAfter orders newest first with nil last.
func timeAfter(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

// flexTime scans a timestamp that may arrive as time.Time (Postgres, plain
// SQLite columns) or as text: SQLite returns MAX()/CASE over a datetime
// column untyped, so the driver hands back the stored string.
type flexTime struct{ t *time.Time }

var flexTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
}

func (f *flexTime) Scan(v any) error {
	f.t = nil
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		t := x
		f.t = &t
	case []byte:
		f.t = parseFlexTime(string(x))
	case string:
		f.t = parseFlexTime(x)
	case int64:
		t := time.Unix(x, 0)
		f.t = &t
	}
	return nil
}

func (f flexTime) Ptr() *time.Time { return f.t }

// Value makes flexTime a driver.Valuer like sql.NullTime, which is what lets
// GORM accept it as a plain column in a Scan destination struct.
func (f flexTime) Value() (driver.Value, error) {
	if f.t == nil {
		return nil, nil
	}
	return *f.t, nil
}

func parseFlexTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Go's time.String() appends a monotonic clock reading ("m=+0.01").
	if i := strings.Index(s, " m="); i > 0 {
		s = s[:i]
	}
	for _, layout := range flexTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
