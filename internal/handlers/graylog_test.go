package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/graylogquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// fakeGraylog answers the API calls PatchMan makes, for one access token
// ("tok-1") and one user (ada / pw-1). A query containing "((" fails like
// OpenSearch does: HTTP 200 with an errors list.
type fakeGraylog struct {
	mu       sync.Mutex
	lastSync map[string]any
}

func (f *fakeGraylog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
	auth := r.Header.Get("Authorization")
	okTok := auth == "Basic "+base64.StdEncoding.EncodeToString([]byte("tok-1:token"))
	okPw := auth == "Basic "+base64.StdEncoding.EncodeToString([]byte("ada:pw-1"))
	if !okTok && !okPw {
		write(401, `{"type":"ApiError","message":"Unauthorized"}`)
		return
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Requested-By") == "" {
		write(400, `{"message":"CSRF protection header is missing."}`)
		return
	}
	switch r.URL.Path {
	case "/api/system":
		write(200, `{"version":"6.1.16+4a05695","hostname":"gl-1","cluster_id":"c-1"}`)
	case "/api/system/sessions":
		write(200, `{"session_id":null,"username":"ada","is_valid":false}`)
	case "/api/streams":
		write(200, `{"total":2,"streams":[{"id":"s-def","title":"Default Stream","is_default":true},{"id":"s-orders","title":"orders","description":"order services"}]}`)
	case "/api/views/search/sync":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastSync = body
		f.mu.Unlock()
		q := body["queries"].([]any)[0].(map[string]any)
		if strings.Contains(q["query"].(map[string]any)["query_string"].(string), "((") {
			write(200, `{"results":{"q":{"errors":[{"description":"OpenSearch exception [type=search_phase_execution_exception, reason=all shards failed]."}],"search_types":{}}}}`)
			return
		}
		write(200, `{"results":{"q":{"errors":[],"execution_stats":{"duration":12},"search_types":{
			"messages":{"total_results":3,"effective_timerange":{"from":"2026-11-12T07:00:00Z","to":"2026-11-12T08:00:00Z"},"messages":[
				{"index":"graylog_0","message":{"_id":"m1","timestamp":"2026-11-12T07:59:00.000Z","source":"order-api-1","message":"status=500","service":"order-api","gl2_remote_ip":"10.0.0.1","level":3}}]},
			"histogram":{"rows":[
				{"key":["2026-11-12T07:01:00.000Z"],"values":[{"key":["count()"],"value":2,"rollup":true,"source":"row-leaf"}],"source":"leaf"},
				{"key":["2026-11-12T07:00:00.000Z"],"values":[{"key":["count()"],"value":1,"rollup":true,"source":"row-leaf"}],"source":"leaf"},
				{"key":[],"values":[{"key":["count()"],"value":3,"rollup":true,"source":"row-inner"}],"source":"non-leaf"}]}}}}}`)
	case "/api/dashboards":
		write(200, `{"total":1,"elements":[{"id":"d-1","type":"DASHBOARD","title":"Orders","summary":"order errors","last_updated_at":"2026-11-10T10:00:00Z"}]}`)
	case "/api/search/saved":
		write(200, `{"total":1,"elements":[{"id":"v-1","type":"SEARCH","title":"500s"}]}`)
	default:
		write(404, `{"message":"not found"}`)
	}
}

func graylogTestHandlers(t *testing.T) (*GraylogConnectionHandler, *GraylogOpsHandler, *GraylogGrantHandler, *fakeGraylog, string, *gorm.DB) {
	t.Helper()
	gl := &fakeGraylog{}
	srv := httptest.NewServer(gl)
	t.Cleanup(srv.Close)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GraylogConnection{}, &models.GraylogConnectionGrant{}, &models.GraylogActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("CREATE TABLE IF NOT EXISTS users (id TEXT, email TEXT, name TEXT)")
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	reg := graylogquery.NewRegistry(cipher)
	conns := NewGraylogConnectionHandler(db, cipher, reg)
	return conns, NewGraylogOpsHandler(db, reg, conns), NewGraylogGrantHandler(db, conns), gl, srv.URL, db
}

func glCall(t *testing.T, fn gin.HandlerFunc, user, method, id, query string, body any) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x?"+query, rdr)
	c.Request.Header.Set("Content-Type", "application/json")
	if id != "" {
		c.Params = gin.Params{{Key: "id", Value: id}}
	}
	c.Set("userID", user)
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestGraylogConnectionLifecycle(t *testing.T) {
	conns, _, _, _, url, _ := graylogTestHandlers(t)
	// The pasted API-browser URL is cut back to the server URL.
	code, out := glCall(t, conns.Create, "owner", "POST", "", "", map[string]any{"name": "logs", "url": url + "/api/", "secret": "tok-1"})
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	d := out["data"].(map[string]any)
	id := d["id"].(string)
	if d["url"] != url || d["authKind"] != "token" || d["hasSecret"] != true || strings.Contains(mustJSON(out), "tok-1") {
		t.Fatalf("create dto = %v", d)
	}
	if code, out := glCall(t, conns.Create, "owner", "POST", "", "", map[string]any{"name": "x", "url": url, "authKind": "basic", "secret": "pw"}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "username") {
		t.Fatalf("basic without username = %d %v", code, out)
	}
	code, out = glCall(t, conns.Test, "owner", "POST", id, "", nil)
	td := out["data"].(map[string]any)
	if code != 200 || td["ok"] != true || td["version"] != "6.1.16+4a05695" || td["username"] != "ada" || td["streams"] != float64(2) {
		t.Fatalf("test saved = %d %v", code, out)
	}
	// The stored token is never sent to a changed URL.
	_, out = glCall(t, conns.Test, "owner", "POST", id, "", map[string]any{"url": "http://elsewhere.example"})
	if td := out["data"].(map[string]any); td["ok"] != false || !strings.Contains(fmt.Sprint(td["error"]), "again") {
		t.Fatalf("test changed url = %v", out)
	}
	// Unsaved basic: wrong password is a readable auth failure.
	_, out = glCall(t, conns.Test, "owner", "POST", "", "", map[string]any{"name": "n", "url": url, "authKind": "basic", "username": "ada", "secret": "nope"})
	if td := out["data"].(map[string]any); td["ok"] != false || td["code"] != "auth" {
		t.Fatalf("test wrong password = %v", out)
	}
	// Switching to basic needs the password.
	if code, _ := glCall(t, conns.Update, "owner", "PATCH", id, "", map[string]any{"authKind": "basic", "username": "ada"}); code != 400 {
		t.Fatalf("switch kind without secret = %d", code)
	}
	code, out = glCall(t, conns.Update, "owner", "PATCH", id, "", map[string]any{"authKind": "basic", "username": "ada", "secret": "pw-1"})
	if code != 200 || out["data"].(map[string]any)["username"] != "ada" {
		t.Fatalf("switch kind = %d %v", code, out)
	}
	if _, out := glCall(t, conns.Test, "owner", "POST", id, "", nil); out["data"].(map[string]any)["ok"] != true {
		t.Fatalf("test after switch = %v", out)
	}
	if code, out := glCall(t, conns.RevealSecret, "someone", "POST", id, "", nil); code != 404 {
		t.Fatalf("reveal by non-owner = %d %v", code, out)
	}
}

func TestGraylogSearchSharedAndAudited(t *testing.T) {
	conns, ops, grants, gl, url, db := graylogTestHandlers(t)
	_, out := glCall(t, conns.Create, "owner", "POST", "", "", map[string]any{"name": "logs", "url": url, "secret": "tok-1"})
	id := out["data"].(map[string]any)["id"].(string)

	search := map[string]any{"query": "service:order-api", "range": map[string]any{"type": "relative", "range": 3600}, "streams": []string{"s-orders", " "}, "limit": 50}
	if code, _ := glCall(t, ops.Search, "reader", "POST", id, "", search); code != 404 {
		t.Fatalf("search without a grant = %d", code)
	}
	if code, out := glCall(t, grants.Add, "owner", "POST", id, "", map[string]any{"subjectKind": "user", "subjectId": "reader", "role": "read"}); code != 201 {
		t.Fatalf("grant = %d %v", code, out)
	}
	code, out := glCall(t, ops.Search, "reader", "POST", id, "", search)
	if code != 200 {
		t.Fatalf("search = %d %v", code, out)
	}
	d := out["data"].(map[string]any)
	msgs := d["messages"].([]any)
	m := msgs[0].(map[string]any)
	f := m["fields"].(map[string]any)
	if d["total"] != float64(3) || m["id"] != "m1" || m["source"] != "order-api-1" || f["service"] != "order-api" || f["gl2_remote_ip"] != nil {
		t.Fatalf("search result = %v", d)
	}
	// Histogram: leaf rows only, in time order, gaps filled with zero across
	// the searched hour; 1h span → 1m buckets.
	hist := d["histogram"].([]any)
	if len(hist) != 60 || d["interval"] != "1m" || mustJSON(hist[:3]) != `[{"count":1,"time":"2026-11-12T07:00:00.000Z"},{"count":2,"time":"2026-11-12T07:01:00.000Z"},{"count":0,"time":"2026-11-12T07:02:00.000Z"}]` {
		t.Fatalf("histogram (%d) = %v interval %v", len(hist), hist, d["interval"])
	}
	if web := d["webUrl"].(string); !strings.HasPrefix(web, url+"/search?") || !strings.Contains(web, "streams=s-orders") || !strings.Contains(web, "relative=3600") {
		t.Fatalf("webUrl = %s", web)
	}
	// What went to Graylog: stream filter (blank id dropped), limit, sort, interval.
	gl.mu.Lock()
	sent := mustJSON(gl.lastSync)
	gl.mu.Unlock()
	for _, want := range []string{`"filters":[{"id":"s-orders","type":"stream"}]`, `"limit":50`, `"order":"DESC"`, `"timeunit":"1m"`, `"query_string":"service:order-api"`} {
		if !strings.Contains(sent, want) {
			t.Fatalf("sync body lacks %s: %s", want, sent)
		}
	}
	// Broken query → 400 bad_query with a readable reason.
	search["query"] = "service:(("
	code, out = glCall(t, ops.Search, "reader", "POST", id, "", search)
	if code != 400 || out["code"] != "bad_query" || !strings.Contains(fmt.Sprint(out["error"]), "not valid search syntax") {
		t.Fatalf("bad query = %d %v", code, out)
	}
	if code, out := glCall(t, ops.Search, "reader", "POST", id, "", map[string]any{"query": "*", "range": map[string]any{"type": "absolute", "from": "2026-11-12T08:00:00Z", "to": "2026-11-12T07:00:00Z"}}); code != 400 {
		t.Fatalf("reversed range = %d %v", code, out)
	}
	// Both searches were audited against the owner's connection.
	time.Sleep(100 * time.Millisecond)
	var rows []models.GraylogActivityHistory
	db.Where("connection_id = ?", id).Order("executed_at").Find(&rows)
	if len(rows) != 2 || rows[0].UserID != "reader" || rows[0].DelegatedOwner == nil || *rows[0].DelegatedOwner != "owner" ||
		!strings.Contains(rows[0].Target, "service:order-api | last 1h0m0s | streams s-orders") || rows[1].ErrorText == "" {
		t.Fatalf("audit = %+v", rows)
	}

	code, out = glCall(t, ops.Streams, "reader", "GET", id, "", nil)
	if code != 200 || mustJSON(out["data"]) != `[{"id":"s-def","isDefault":true,"title":"Default Stream"},{"description":"order services","id":"s-orders","title":"orders"}]` {
		t.Fatalf("streams = %d %v", code, out)
	}
	code, out = glCall(t, ops.Views, "reader", "GET", id, "", nil)
	views := out["data"].([]any)
	if code != 200 || len(views) != 2 || views[0].(map[string]any)["webUrl"] != url+"/dashboards/d-1" || views[1].(map[string]any)["type"] != "search" || views[1].(map[string]any)["webUrl"] != url+"/search/v-1" {
		t.Fatalf("views = %d %v", code, out)
	}
}

func TestGraylogBaseURL(t *testing.T) {
	cases := map[string]string{
		"graylog.example.com:9000":                      "http://graylog.example.com:9000",
		"https://logs.example.com/":                     "https://logs.example.com",
		"https://logs.example.com/api/":                 "https://logs.example.com",
		"https://example.com/graylog/api":               "https://example.com/graylog",
		"https://example.com/graylog/search?q=x#frag":   "https://example.com/graylog",
		"https://logs.example.com/dashboards/abc":       "https://logs.example.com",
		"https://logs.example.com/streams/1/search?q=a": "https://logs.example.com",
	}
	for in, want := range cases {
		if got, err := graylogquery.BaseURL(in); err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := graylogquery.BaseURL("  "); err == nil {
		t.Error("empty URL should fail")
	}
}
