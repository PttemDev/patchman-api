package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"patchman-be/internal/grafanaquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// fakeGrafanaAPI answers the handful of endpoints the handlers touch and
// records the raw SQL that reached /api/ds/query.
type fakeGrafanaAPI struct {
	mu   sync.Mutex
	sqls []string
}

func (f *fakeGrafanaAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/api/frontend/settings":
		_, _ = w.Write([]byte(`{"buildInfo":{"version":"11.6.0"},"defaultDatasource":"PG",
			"datasources":{"PG":{"id":1,"uid":"pg","type":"grafana-postgresql-datasource","meta":{"backend":true}}}}`))
	case "/api/user":
		_, _ = w.Write([]byte(`{"login":"viewer"}`))
	case "/api/ds/query":
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Queries []map[string]any `json:"queries"`
		}
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		for _, q := range body.Queries {
			f.sqls = append(f.sqls, q["rawSql"].(string))
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"results":{"A":{"frames":[{"schema":{"fields":[{"name":"v","type":"number"}]},"data":{"values":[[1]]}}]}}}`))
	default:
		w.WriteHeader(404)
	}
}

func grafanaTestEnv(t *testing.T) (*gin.Engine, *gorm.DB, *fakeGrafanaAPI, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:grafanah?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GrafanaConnection{}, &models.GrafanaConnectionGrant{}, &models.GrafanaActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("CREATE TABLE IF NOT EXISTS users (id TEXT, email TEXT, name TEXT)")
	db.Exec("CREATE TABLE IF NOT EXISTS teams (id TEXT, name TEXT, deleted_at DATETIME)")
	for _, tbl := range []string{"grafana_connections", "grafana_connection_grants", "grafana_activity_history"} {
		db.Exec("DELETE FROM " + tbl)
	}
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	reg := grafanaquery.NewRegistry(cipher, nil)
	connH := NewGrafanaConnectionHandler(db, cipher, reg)
	opsH := NewGrafanaOpsHandler(db, reg, connH)
	grantH := NewGrafanaGrantHandler(db, connH)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.POST("/api/grafana-connections", connH.Create)
	r.GET("/api/grafana-connections", connH.List)
	r.PATCH("/api/grafana-connections/:id", connH.Update)
	r.POST("/api/grafana-connections/:id/grants", grantH.Add)
	r.GET("/api/grafana-connections/:id/grants", grantH.List)
	r.POST("/api/grafana/:id/query", opsH.Query)
	r.GET("/api/grafana/:id/info", opsH.Info)
	fake := &fakeGrafanaAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return r, db, fake, srv.URL
}

func gcall(r *gin.Engine, user, method, path string, body any) (int, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", user)
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestGrafanaConnectionsGrantsAndSQLGuard(t *testing.T) {
	r, db, fake, url := grafanaTestEnv(t)

	code, body := gcall(r, "owner", "POST", "/api/grafana-connections", map[string]any{
		"id": "g1", "name": "prod", "url": url + "/", "authKind": "token", "token": "tok",
	})
	if code != 201 || strings.Contains(body, "tok\"") || !strings.Contains(body, `"hasToken":true`) {
		t.Fatalf("create: %d %s", code, body)
	}
	var row models.GrafanaConnection
	db.First(&row, "id = ?", "g1")
	if row.TokenEnc == "" || row.TokenEnc == "tok" || row.URL != url {
		t.Fatalf("stored row: %+v", row)
	}

	if code, body := gcall(r, "bob", "GET", "/api/grafana/g1/info", nil); code != 404 {
		t.Fatalf("stranger must not reach the connection: %d %s", code, body)
	}
	if code, _ := gcall(r, "bob", "POST", "/api/grafana-connections/g1/grants", map[string]any{"subjectKind": "user", "subjectId": "bob"}); code != 404 {
		t.Fatal("only the owner may grant")
	}
	if code, body := gcall(r, "owner", "POST", "/api/grafana-connections/g1/grants", map[string]any{"subjectKind": "user", "subjectId": "bob", "role": "read"}); code != 201 {
		t.Fatalf("grant: %d %s", code, body)
	}
	if _, body := gcall(r, "bob", "GET", "/api/grafana-connections", nil); !strings.Contains(body, `"grantRole":"read"`) || !strings.Contains(body, `"shared":true`) {
		t.Fatalf("bob's list: %s", body)
	}

	q := func(user, sql string) string {
		_, body := gcall(r, user, "POST", "/api/grafana/g1/query", map[string]any{
			"from": "now-1h", "to": "now",
			"targets": []any{map[string]any{"refId": "A", "datasource": map[string]any{"uid": "pg"}, "rawSql": sql, "format": "table"}},
		})
		return body
	}
	if b := q("bob", "SELECT 1"); !strings.Contains(b, `"frames":[{`) {
		t.Fatalf("read grant SELECT: %s", b)
	}
	if b := q("bob", "DELETE FROM samples"); !strings.Contains(b, "read-only access") {
		t.Fatalf("read grant DELETE must be refused: %s", b)
	}
	if b := q("owner", "DELETE FROM samples"); !strings.Contains(b, `"frames":[{`) {
		t.Fatalf("owner may run any SQL: %s", b)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, s := range fake.sqls {
		if strings.HasPrefix(s, "DELETE") && len(fake.sqls) != 2 {
			t.Fatalf("the refused DELETE reached Grafana: %v", fake.sqls)
		}
	}
	if len(fake.sqls) != 2 {
		t.Fatalf("expected SELECT + owner DELETE only, got %v", fake.sqls)
	}
}

func TestGrafanaSaveSessionAndUpdate(t *testing.T) {
	r, db, _, url := grafanaTestEnv(t)
	if code, body := gcall(r, "owner", "POST", "/api/grafana-connections", map[string]any{
		"id": "s1", "name": "sso", "url": url, "authKind": "session", "session": "grafana_session=abc",
	}); code != 201 || !strings.Contains(body, `"hasSession":true`) {
		t.Fatalf("create session conn: %d %s", code, body)
	}
	var row models.GrafanaConnection
	db.First(&row, "id = ?", "s1")
	before := row.SessionEnc
	if row.SessionRotatedAt == nil {
		t.Fatal("sessionRotatedAt should be set")
	}
	// The registry persists rotated cookies through the handler.
	var h GrafanaConnectionHandler
	h.db = db
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	h.cipher = cipher
	if err := h.SaveSession("s1", "rotated"); err != nil {
		t.Fatal(err)
	}
	db.First(&row, "id = ?", "s1")
	if v, _ := cipher.Decrypt(row.SessionEnc); v != "rotated" || row.SessionEnc == before {
		t.Fatalf("SaveSession: %q", v)
	}
	if code, body := gcall(r, "owner", "PATCH", "/api/grafana-connections/s1", map[string]any{"authKind": "nope"}); code != 400 {
		t.Fatalf("invalid authKind must be rejected: %d %s", code, body)
	}
	if code, _ := gcall(r, "other", "PATCH", "/api/grafana-connections/s1", map[string]any{"name": "x"}); code != 404 {
		t.Fatal("non-owner update must 404")
	}
}
