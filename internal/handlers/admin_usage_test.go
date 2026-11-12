package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// adminUsageTestDB opens a fresh SQLite DB per test. Set ADMIN_TEST_PG_DSN
// (a throwaway Postgres, e.g. a local docker container) to run the same tests
// on Postgres; the public schema is dropped and recreated per test.
func adminUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	var (
		db  *gorm.DB
		err error
	)
	if dsn := os.Getenv("ADMIN_TEST_PG_DSN"); dsn != "" {
		// The schema is dropped below: only ever a local throwaway server.
		if !strings.Contains(dsn, "host=127.0.0.1") && !strings.Contains(dsn, "host=localhost") {
			t.Fatalf("ADMIN_TEST_PG_DSN must point at host=127.0.0.1 or host=localhost (the test drops the public schema)")
		}
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err == nil {
			err = db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public").Error
		}
	} else {
		db, err = gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.AuditLog{}, &models.History{}, &models.MCPToken{},
		&models.Collection{}, &models.SavedRequest{}, &models.MockServer{}, &models.MockRoute{},
		&models.Environment{}, &models.Whiteboard{}, &models.TestCase{}, &models.TestRun{},
		&models.LoadTest{}, &models.Workspace{}, &models.Team{}, &models.TeamMember{},
		&models.UserGitIntegration{}, &models.Note{},
		&models.KafkaConnection{}, &models.KafkaActivityHistory{},
		&models.S3Connection{}, &models.S3ActivityHistory{},
		&models.DBConnection{}, &models.DBQueryHistory{},
		&models.GitConnection{}, &models.GitActivityHistory{},
		&models.RedisConnection{}, &models.RedisActivityHistory{},
		&models.ESConnection{}, &models.ESActivityHistory{},
		&models.K8sConnection{}, &models.K8sActivityHistory{},
		&models.GrafanaConnection{}, &models.GrafanaActivityHistory{},
		&models.RabbitConnection{}, &models.RabbitActivityHistory{},
		&models.GraylogConnection{}, &models.GraylogActivityHistory{},
		&models.ObservabilityConnection{}, &models.ObservabilityActivityHistory{},
		&models.LLMProvider{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

// at returns a fixed UTC instant `m` minutes after a base time.
func at(m int) time.Time {
	return time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute)
}

func tp(t time.Time) *time.Time { return &t }

func mustCreate(t *testing.T, db *gorm.DB, v any) {
	t.Helper()
	if err := db.Create(v).Error; err != nil {
		t.Fatalf("seed %T: %v", v, err)
	}
}

// seedAdminUsage: an admin, an active user u1 with rows across areas, an idle
// user u2 with only a login, and a stranger whose rows must never leak in.
func seedAdminUsage(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustCreate(t, db, &models.User{ID: "admin", Email: "admin@x", Role: models.RoleAdmin})
	mustCreate(t, db, &models.User{ID: "u1", Email: "u1@x", Role: models.RoleUser, LastSeenAt: tp(at(0)), LastLoginAt: tp(at(-60)), LastSeenVersion: "1.60.0"})
	mustCreate(t, db, &models.User{ID: "u2", Email: "u2@x", Role: models.RoleUser, LastLoginAt: tp(at(-500))})
	mustCreate(t, db, &models.User{ID: "zed", Email: "zed@x", Role: models.RoleUser})

	// Audit rows as the middleware writes them: `resource` is often
	// "unknown" or a sub-resource word, so the area must come from details.
	audits := []struct {
		min      int
		resource string
		details  string
	}{
		{1, "unknown", "PATCH /api/whiteboards/wb1"},
		{2, "grant", "POST /api/kafka-connections/k1/grants"},
		{3, "unknown", "POST /api/kafka/k1/topics"},
		{4, "unknown", "POST /api/notes/remember"},
		{5, "unknown", "POST /api/notes"},
		{6, "grant", "DELETE /api/collections/c1/grants/g1"},
		{7, "collection", "POST /api/integrations/gitlab/collections/c1/push"},
		{8, "proxy", "POST /api/proxy/send"},
		{9, "unknown", "POST /api/errors/report"},
		{10, "unknown", "PUT /api/s3-connections/s1"},
		{11, "unknown", "POST /api/s3/s1/objects"},
		{12, "visibility", "PATCH /api/notes/n1/visibility"},
	}
	for i, a := range audits {
		mustCreate(t, db, &models.AuditLog{ID: "a" + string(rune('a'+i)), UserID: "u1", Action: "created", Resource: a.resource, Details: a.details, CreatedAt: at(a.min)})
	}
	mustCreate(t, db, &models.AuditLog{ID: "z1", UserID: "zed", Action: "created", Resource: "unknown", Details: "POST /api/kafka/k9/topics", CreatedAt: at(900)})

	// Sent-request history (latest at +20).
	for i, m := range []int{14, 18, 20} {
		mustCreate(t, db, &models.History{ID: "h" + string(rune('a'+i)), UserID: "u1", Method: "GET", URL: "https://x", CreatedAt: at(m)})
	}
	mustCreate(t, db, &models.History{ID: "hz", UserID: "zed", Method: "GET", URL: "https://x", CreatedAt: at(950)})

	// Kafka: 2 connections + 4 ledger rows; the ledger holds u1's newest
	// signal (+30), which only the usage endpoint sees.
	mustCreate(t, db, &models.KafkaConnection{ID: "k1", UserID: "u1", Name: "k1", CreatedAt: at(-100)})
	mustCreate(t, db, &models.KafkaConnection{ID: "k2", UserID: "u1", Name: "k2", CreatedAt: at(-90)})
	for i, m := range []int{21, 25, 28, 30} {
		mustCreate(t, db, &models.KafkaActivityHistory{ID: "ka" + string(rune('a'+i)), UserID: "u1", ConnectionID: "k1", Action: "consume", ExecutedAt: at(m)})
	}
	mustCreate(t, db, &models.KafkaActivityHistory{ID: "kz", UserID: "zed", ConnectionID: "k9", Action: "consume", ExecutedAt: at(999)})

	// Collections (one root + one folder), 3 requests, 1 mock server with 2 routes.
	mustCreate(t, db, &models.Collection{ID: "c1", UserID: "u1", Name: "c1", Type: "collection", CreatedAt: at(-200)})
	mustCreate(t, db, &models.Collection{ID: "c2", UserID: "u1", Name: "f", Type: "folder", ParentID: strPtr("c1"), CreatedAt: at(-199)})
	for i, cid := range []string{"c1", "c1", "c2"} {
		mustCreate(t, db, &models.SavedRequest{ID: "r" + string(rune('a'+i)), CollectionID: cid, Name: "r", Method: "GET", URL: "https://x", CreatedAt: at(-150 + i)})
	}
	mustCreate(t, db, &models.MockServer{ID: "m1", UserID: "u1", Name: "m", Prefix: "m1", CreatedAt: at(-300)})
	mustCreate(t, db, &models.MockRoute{ID: "mr1", MockServerID: "m1", Path: "/a", Method: "GET"})
	mustCreate(t, db, &models.MockRoute{ID: "mr2", MockServerID: "m1", Path: "/b", Method: "GET"})

	// Notes: a page u1 wrote, two cards u1's agents filed (one in someone
	// else's team memory folder), a page in u1's tree written by someone
	// else, and a deleted page — only the first three count.
	mustCreate(t, db, &models.Note{ID: "n1", UserID: "u1", Type: "page", Title: "p", CreatedBy: "u1", UpdatedBy: "u1", CreatedAt: at(-50), UpdatedAt: at(5)})
	mustCreate(t, db, &models.Note{ID: "n2", UserID: "team-owner", Type: "card", Title: "c", Agent: "Claude Code", CreatedBy: "u1", UpdatedBy: "u1", CreatedAt: at(4), UpdatedAt: at(4)})
	mustCreate(t, db, &models.Note{ID: "n3", UserID: "u1", Type: "card", Title: "c2", CreatedBy: "u1", UpdatedBy: "u1", CreatedAt: at(-10), UpdatedAt: at(-10)})
	mustCreate(t, db, &models.Note{ID: "n4", UserID: "u1", Type: "page", Title: "theirs", CreatedBy: "zed", UpdatedBy: "zed", CreatedAt: at(800), UpdatedAt: at(800)})
	deleted := models.Note{ID: "n5", UserID: "u1", Type: "page", Title: "gone", CreatedBy: "u1", UpdatedBy: "u1", CreatedAt: at(-5), UpdatedAt: at(-5)}
	mustCreate(t, db, &deleted)
	db.Delete(&deleted)

	// MCP tokens: one used at +16, one never used.
	mustCreate(t, db, &models.MCPToken{ID: "t1", UserID: "u1", Name: "laptop", TokenHash: "h1", ClientName: "Claude Code", LastUsedAt: tp(at(16)), CreatedAt: at(-400)})
	mustCreate(t, db, &models.MCPToken{ID: "t2", UserID: "u1", Name: "spare", TokenHash: "h2", CreatedAt: at(-300)})

	// Team membership: one live team, one deleted.
	mustCreate(t, db, &models.Team{ID: "tm1", Name: "A", CreatedBy: "admin"})
	mustCreate(t, db, &models.Team{ID: "tm2", Name: "B", CreatedBy: "admin", DeletedAt: tp(at(-1))})
	mustCreate(t, db, &models.TeamMember{TeamID: "tm1", UserID: "u1", Role: "member"})
	mustCreate(t, db, &models.TeamMember{TeamID: "tm2", UserID: "u1", Role: "member"})
}

func strPtr(s string) *string { return &s }

func adminUsageRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewAdminHandler(db, &config.Config{})
	r := gin.New()
	g := r.Group("/api/admin")
	// Stand-in for JWTAuth: the test names the caller in a header.
	g.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-Test-User")); c.Next() })
	g.Use(middleware.RequireAdmin(db))
	g.GET("/users", h.ListUsers)
	g.GET("/users/:id/usage", h.UserUsage)
	return r
}

func adminGet(t *testing.T, r *gin.Engine, caller, path string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Test-User", caller)
	r.ServeHTTP(w, req)
	if out != nil && w.Code == http.StatusOK {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, w.Body.String())
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("decode data %s: %v (%s)", path, err, w.Body.String())
		}
	}
	return w.Code
}

func timeEq(t *testing.T, what string, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil || !got.Equal(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestAdminUserUsage(t *testing.T) {
	db := adminUsageTestDB(t)
	seedAdminUsage(t, db)
	r := adminUsageRouter(db)

	var u adminUserUsage
	if code := adminGet(t, r, "admin", "/api/admin/users/u1/usage", &u); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	// Newest signal is the Kafka ledger at +30.
	timeEq(t, "lastActivityAt", u.LastActivityAt, at(30))

	c := u.Counts
	want := adminUsageCounts{Collections: 1, Requests: 3, MockServers: 1, MockRoutes: 2, History: 3, Notes: 1, Cards: 2, MCPTokens: 2, Teams: 1}
	if c != want {
		t.Errorf("counts = %+v\nwant     %+v", c, want)
	}

	tools := map[string]adminToolUsage{}
	for _, tu := range u.Tools {
		tools[tu.Tool] = tu
	}
	check := func(tool string, items, actions, ops int64, last time.Time) {
		t.Helper()
		got, ok := tools[tool]
		if !ok {
			t.Errorf("tool %q missing (have %v)", tool, u.Tools)
			return
		}
		if got.Items != items || got.Actions != actions || got.Operations != ops {
			t.Errorf("%s = items %d actions %d ops %d, want %d/%d/%d", tool, got.Items, got.Actions, got.Operations, items, actions, ops)
		}
		timeEq(t, tool+".lastUsedAt", got.LastUsedAt, last)
	}
	check("kafka", 2, 2, 4, at(30))
	check("history", 0, 1, 3, at(20))
	check("whiteboards", 0, 1, 0, at(1))
	check("memory", 2, 1, 0, at(4))
	check("notes", 1, 2, 0, at(12)) // POST /notes + PATCH /notes/n1/visibility; page edit at +5
	check("requests", 3, 1, 0, at(6))
	check("git", 0, 1, 0, at(7))
	check("s3", 0, 2, 0, at(11))
	check("other", 0, 1, 0, at(9))
	check("mcp", 2, 0, 0, at(16))
	if _, ok := tools["mocks"]; !ok {
		t.Errorf("mocks tool missing")
	}
	if len(u.Tools) == 0 || u.Tools[0].Tool != "kafka" {
		t.Errorf("tools not sorted by lastUsedAt desc: first = %+v", u.Tools)
	}
	for i := 1; i < len(u.Tools); i++ {
		if timeAfter(u.Tools[i].LastUsedAt, u.Tools[i-1].LastUsedAt) {
			t.Errorf("tools out of order at %d: %v after %v", i, u.Tools[i].LastUsedAt, u.Tools[i-1].LastUsedAt)
		}
	}

	if len(u.Agents) != 2 || u.Agents[0].Agent != "Claude Code" || u.Agents[0].Cards != 1 || u.Agents[1].Agent != "" {
		t.Errorf("agents = %+v", u.Agents)
	}
	if len(u.MCPClients) != 2 || u.MCPClients[0].ClientName != "Claude Code" || u.MCPClients[1].LastUsedAt != nil {
		t.Errorf("mcpClients = %+v", u.MCPClients)
	}
	timeEq(t, "mcpClients[0].lastUsedAt", u.MCPClients[0].LastUsedAt, at(16))

	// Idle user: no tools, last activity = the login.
	var idle adminUserUsage
	adminGet(t, r, "admin", "/api/admin/users/u2/usage", &idle)
	if len(idle.Tools) != 0 {
		t.Errorf("idle tools = %+v", idle.Tools)
	}
	timeEq(t, "idle lastActivityAt", idle.LastActivityAt, at(-500))

	if code := adminGet(t, r, "admin", "/api/admin/users/nobody/usage", nil); code != http.StatusNotFound {
		t.Errorf("unknown user status = %d, want 404", code)
	}
}

func TestAdminUserUsageRequiresAdmin(t *testing.T) {
	db := adminUsageTestDB(t)
	seedAdminUsage(t, db)
	r := adminUsageRouter(db)
	if code := adminGet(t, r, "u1", "/api/admin/users/u1/usage", nil); code != http.StatusForbidden {
		t.Errorf("non-admin usage status = %d, want 403", code)
	}
	if code := adminGet(t, r, "u1", "/api/admin/users", nil); code != http.StatusForbidden {
		t.Errorf("non-admin list status = %d, want 403", code)
	}
	if code := adminGet(t, r, "", "/api/admin/users/u1/usage", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous usage status = %d, want 401", code)
	}
}

func TestAdminListUsersLastActivity(t *testing.T) {
	db := adminUsageTestDB(t)
	seedAdminUsage(t, db)
	r := adminUsageRouter(db)

	var rows []map[string]any
	if code := adminGet(t, r, "admin", "/api/admin/users", &rows); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	parse := func(id string) *time.Time {
		v, ok := byID[id]["lastActivityAt"].(string)
		if !ok {
			return nil
		}
		ts, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			t.Fatalf("parse %q: %v", v, err)
		}
		return &ts
	}
	// u1: newest cheap signal is the sent request at +20 (audit max +12, MCP
	// +16, seen 0); the Kafka ledger (+30) is only in the usage endpoint.
	timeEq(t, "u1 lastActivityAt", parse("u1"), at(20))
	timeEq(t, "u2 lastActivityAt", parse("u2"), at(-500))
	timeEq(t, "zed lastActivityAt", parse("zed"), at(950))
	if _, ok := byID["admin"]["lastActivityAt"]; ok {
		t.Errorf("admin has no activity, got %v", byID["admin"]["lastActivityAt"])
	}
	// The per-user counts moved to the usage endpoint; integration state stays.
	if _, ok := byID["u1"]["collectionCount"]; ok {
		t.Errorf("collectionCount still in the roster")
	}
	if n, _ := byID["u1"]["mcpTokenCount"].(float64); n != 2 {
		t.Errorf("mcpTokenCount = %v, want 2", byID["u1"]["mcpTokenCount"])
	}
}

// Every source's user/time columns must exist on its migrated table — the
// handler skips a failing source silently, so a typo would just read as 0.
func TestAdminUsageSourcesQueryable(t *testing.T) {
	db := adminUsageTestDB(t)
	for _, s := range usageSources {
		var row struct {
			N      int64
			LastAt flexTime
		}
		q := db.Model(s.model).Select("COUNT(*) AS n, MAX("+s.timeCol+") AS last_at").Where(s.userCol+" = ?", "x")
		if s.cond != "" {
			q = q.Where(s.cond)
		}
		if err := q.Scan(&row).Error; err != nil {
			t.Errorf("%s source %T: %v", s.tool, s.model, err)
		}
	}
}

func TestAdminUsageRouteTable(t *testing.T) {
	key := regexp.MustCompile(`^[a-z0-9]+$`)
	seg := regexp.MustCompile(`^(/[a-z0-9-]+){1,2}$`)
	seen := map[string]bool{}
	for _, r := range usageRoutes {
		if !key.MatchString(r.tool) {
			t.Errorf("tool key %q must match %s (the FE keys its labels on it)", r.tool, key)
		}
		if !seg.MatchString(r.prefix) {
			t.Errorf("prefix %q must be one or two lowercase path segments", r.prefix)
		}
		if seen[r.prefix] {
			t.Errorf("duplicate prefix %q", r.prefix)
		}
		seen[r.prefix] = true
	}
	for _, c := range []struct{ seg1, seg2, want string }{
		{"notes", "memory", "memory"},
		{"notes", "", "notes"},
		{"notes", "abc", "notes"},
		{"kafka-connections", "", "kafka"},
		{"s3", "", "s3"},
		{"integrations", "gitlab", "git"},
		{"errors", "", "other"},
	} {
		if got := usageToolFor(c.seg1, c.seg2); got != c.want {
			t.Errorf("usageToolFor(%q, %q) = %q, want %q", c.seg1, c.seg2, got, c.want)
		}
	}
	// Every source tool should also be reachable by a route so its audited
	// writes land on the same row.
	routed := map[string]bool{}
	for _, r := range usageRoutes {
		routed[r.tool] = true
	}
	for _, s := range usageSources {
		if !routed[s.tool] {
			t.Errorf("source tool %q has no route prefix", s.tool)
		}
	}
}

func TestAdminFlexTimeParsesSQLiteText(t *testing.T) {
	want := time.Date(2026, 11, 1, 10, 0, 0, 123000000, time.FixedZone("", 3*3600))
	for _, s := range []string{
		"2026-11-01 10:00:00.123+03:00",
		"2026-11-01T10:00:00.123+03:00",
		"2026-11-01 10:00:00.123 +0300 +03",
		"2026-11-01 10:00:00.123 +0300 +03 m=+0.000000001",
	} {
		var f flexTime
		_ = f.Scan(s)
		if f.Ptr() == nil || !f.Ptr().Equal(want) {
			t.Errorf("parse %q = %v", s, f.Ptr())
		}
	}
}
