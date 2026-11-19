package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"patchman-be/internal/cloudsession"

	"github.com/gin-gonic/gin"
)

// TestCloudOrLocalRouting verifies the whiteboard tools' routing seam: with a
// live cloud session, calls go to the hosted BE with the user's Bearer token;
// without one, they run in-process against the embedded engine.
func TestCloudOrLocalRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Embedded (local) engine: a stub whiteboards route the in-process path hits.
	eng := gin.New()
	eng.GET("/api/whiteboards", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": "local"})
	})
	c := &caller{engine: eng, jwtSecret: "s", userID: "u"}

	// Fake hosted BE for the cloud path — records the auth header + path.
	var gotAuth, gotPath string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":"cloud"}`))
	}))
	defer cloud.Close()

	// No session → local.
	cloudsession.Clear()
	if code, body := c.cloudOrLocal("GET", "/api/whiteboards", nil); code != 200 || !strings.Contains(body, "local") {
		t.Fatalf("no-session should route local; got %d %s", code, body)
	}

	// Session set → cloud, carrying the Bearer token; a trailing /api on the
	// base is trimmed so the tool path's /api isn't doubled.
	cloudsession.Set(cloud.URL+"/api", "tok123")
	code, body := c.cloudOrLocal("GET", "/api/whiteboards", nil)
	if code != 200 || !strings.Contains(body, "cloud") {
		t.Fatalf("session should route cloud; got %d %s", code, body)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("cloud call missing bearer token; got %q", gotAuth)
	}
	if gotPath != "/api/whiteboards" {
		t.Errorf("cloud path doubled/wrong; got %q", gotPath)
	}

	// Cleared → back to local.
	cloudsession.Clear()
	if _, body := c.cloudOrLocal("GET", "/api/whiteboards", nil); !strings.Contains(body, "local") {
		t.Errorf("after clear should route local; got %s", body)
	}
}

// TestGitToolsFollowCloudSession: the git tools authenticate as the caller's
// linked GitLab account, which lives on the hosted BE — so with a cloud
// session they go there (as the signed-in user), without one they stay
// in-process. Other connection tools never leave the device.
func TestGitToolsFollowCloudSession(t *testing.T) {
	var cloudPaths []string
	var cloudAuth string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudPaths = append(cloudPaths, r.Method+" "+r.URL.RequestURI())
		cloudAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":"cloud"}`))
	}))
	defer cloud.Close()
	cs := connect(t, echoEngine())

	// No session → in-process (echo engine answers with the minted user).
	cloudsession.Clear()
	if e := decode(t, call(t, cs, "git_repos", map[string]any{"connectionId": "g1", "search": "api"})); e.Path != "/api/git/connections/g1/repos" || e.UserID != testUser {
		t.Fatalf("no session: git_repos should run locally, got %+v", e)
	}

	cloudsession.Set(cloud.URL, "cloud-tok")
	t.Cleanup(cloudsession.Clear)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"list_git_connections", map[string]any{}, "GET /api/git/connections"},
		{"git_repos", map[string]any{"connectionId": "g1", "search": "api"}, "GET /api/git/connections/g1/repos?search=api"},
		{"git_merge_request", map[string]any{"connectionId": "g1", "project": "grp/repo", "iid": 7}, "GET /api/git/connections/g1/merge_request?iid=7&project=grp%2Frepo"},
		{"git_create_merge_request_note", map[string]any{"connectionId": "g1", "project": "1", "iid": 2, "body": "lgtm"}, "POST /api/git/connections/g1/merge_request_notes?iid=2&project=1"},
	} {
		cloudPaths = nil
		res := call(t, cs, tc.tool, tc.args)
		if res.IsError || len(cloudPaths) != 1 || cloudPaths[0] != tc.want {
			t.Errorf("%s with session: cloud got %v (err=%v), want %q", tc.tool, cloudPaths, res.IsError, tc.want)
		}
	}
	if cloudAuth != "Bearer cloud-tok" {
		t.Errorf("cloud git call auth = %q", cloudAuth)
	}

	// Non-git connection tools stay on the device even with a session.
	cloudPaths = nil
	if e := decode(t, call(t, cs, "list_db_connections", map[string]any{})); e.Path != "/api/db-connections" || len(cloudPaths) != 0 {
		t.Errorf("list_db_connections must stay local; echo=%+v cloud=%v", e, cloudPaths)
	}
}

// TestGitToolsRoutePerConnection: signed in, each git call runs where its
// connection works — a token (PAT) connection or one kept only on this
// computer runs here; an OAuth connection the server also has goes there.
// list_git_connections merges both sides with runsOn.
func TestGitToolsRoutePerConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	localConns := map[string]string{"g-pat": "pat", "g-local-oauth": "oauth", "g-both": "oauth"}
	eng.GET("/api/git/connections", func(c *gin.Context) {
		out := []gin.H{}
		for _, id := range []string{"g-pat", "g-local-oauth", "g-both"} {
			out = append(out, gin.H{"id": id, "authKind": localConns[id]})
		}
		c.JSON(200, gin.H{"data": out})
	})
	eng.GET("/api/git/connections/:id", func(c *gin.Context) {
		if k, ok := localConns[c.Param("id")]; ok {
			c.JSON(200, gin.H{"data": gin.H{"id": c.Param("id"), "authKind": k}})
			return
		}
		c.JSON(404, gin.H{"error": "Connection not found"})
	})
	eng.GET("/api/git/connections/:id/repos", func(c *gin.Context) { c.JSON(200, gin.H{"data": "local:" + c.Param("id")}) })

	var cloudCalls []string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudCalls = append(cloudCalls, r.URL.Path)
		switch r.URL.Path {
		case "/api/git/connections":
			_, _ = w.Write([]byte(`{"data":[{"id":"g-both","authKind":"oauth"},{"id":"g-cloud","authKind":"oauth"}]}`))
		case "/api/git/connections/g-both", "/api/git/connections/g-cloud":
			_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
		case "/api/git/connections/g-both/repos", "/api/git/connections/g-cloud/repos":
			_, _ = w.Write([]byte(`{"data":"cloud"}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Connection not found"}`))
		}
	}))
	defer cloud.Close()
	cloudsession.Set(cloud.URL, "tok")
	t.Cleanup(cloudsession.Clear)
	cloudConnCache = sync.Map{}
	c := &caller{engine: eng, jwtSecret: "s", userID: "u"}
	ctx := context.Background()

	for id, want := range map[string]string{
		"g-pat":         "local:g-pat",         // own token → here
		"g-local-oauth": "local:g-local-oauth", // the server doesn't have it → here
		"g-both":        "cloud",               // OAuth on both → server (linked account)
		"g-cloud":       "cloud",               // only on the server
	} {
		code, body := c.gitCallCtx(ctx, id, "GET", gitPath(id, "/repos", nil), nil)
		if code != 200 || !strings.Contains(body, `"`+want+`"`) {
			t.Errorf("%s: got %d %s, want %s", id, code, body, want)
		}
	}

	code, body := c.listGitConnections(ctx)
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	got := map[string]any{}
	for _, m := range out.Data {
		got[m["id"].(string)] = m["runsOn"]
	}
	want := map[string]any{"g-pat": "local", "g-local-oauth": "local", "g-both": "cloud", "g-cloud": "cloud"}
	if code != 200 || len(got) != 4 {
		t.Fatalf("list: %d %s", code, body)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("list %s runsOn=%v want %v", id, got[id], w)
		}
	}

	// The server's answer is cached: a second call costs no lookup.
	cloudCalls = nil
	c.gitCallCtx(ctx, "g-both", "GET", gitPath("g-both", "/repos", nil), nil)
	if len(cloudCalls) != 1 {
		t.Errorf("cached route: want only the repos call, got %v", cloudCalls)
	}

	// Not signed in: everything in-process, list untouched.
	cloudsession.Clear()
	if _, body := c.gitCallCtx(ctx, "g-both", "GET", gitPath("g-both", "/repos", nil), nil); !strings.Contains(body, "local:g-both") {
		t.Errorf("no session must run locally: %s", body)
	}
}

// On the hosted server an unknown git connection id gets a hint pointing to
// the desktop app's MCP; the desktop's own MCP keeps the plain error.
func TestGitUnknownConnectionHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.GET("/api/git/connections/:id/repos", func(c *gin.Context) { c.JSON(404, gin.H{"error": "Connection not found"}) })
	c := &caller{engine: eng, jwtSecret: "s", userID: "u"}
	cloudsession.Clear()
	SetEmbedded(false)
	t.Cleanup(func() { SetEmbedded(false) })
	if _, body := c.gitCallCtx(context.Background(), "x", "GET", gitPath("x", "/repos", nil), nil); !strings.Contains(body, "patchman-local") {
		t.Errorf("hosted: want desktop hint, got %s", body)
	}
	SetEmbedded(true)
	if _, body := c.gitCallCtx(context.Background(), "x", "GET", gitPath("x", "/repos", nil), nil); strings.Contains(body, "patchman-local") {
		t.Errorf("desktop: no hint expected, got %s", body)
	}
}
