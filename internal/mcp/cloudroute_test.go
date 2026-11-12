package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
