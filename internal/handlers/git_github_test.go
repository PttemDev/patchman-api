package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// fakeGitHub answers the REST calls the GitHub provider makes.
func fakeGitHub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		p := r.URL.Path
		q := r.URL.Query()
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case p == "/user/repos":
			if q.Get("page") == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/user/repos?page=2>; rel="next"`, r.Host))
				_, _ = w.Write([]byte(`[{"id":11,"name":"api","full_name":"acme/api","default_branch":"main","clone_url":"https://github.com/acme/api.git","pushed_at":"2026-11-18T10:00:00Z","private":true,"visibility":"private"}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":12,"name":"web","full_name":"acme/web","default_branch":"master","clone_url":"https://github.com/acme/web.git","private":false}]`))
		case p == "/repositories/11" || p == "/repos/acme/api":
			_, _ = w.Write([]byte(`{"full_name":"acme/api","default_branch":"main"}`))
		case p == "/repos/acme/api/branches":
			_, _ = w.Write([]byte(`[{"name":"feature","commit":{"sha":"f1"}},{"name":"main","commit":{"sha":"m1"}}]`))
		case p == "/repos/acme/api/commits":
			if q.Get("sha") != "main" {
				t.Errorf("commits ref = %q", q.Get("sha"))
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/acme/api/commits?page=2>; rel="next"`, r.Host))
			_, _ = w.Write([]byte(`[{"sha":"abcdef1234567","html_url":"https://github.com/acme/api/commit/abcdef1","commit":{"message":"Fix login\n\nDetails","author":{"name":"Ada","email":"ada@x","date":"2026-11-18T09:00:00Z"}},"parents":[{"sha":"p1"}]}]`))
		case p == "/repos/acme/api/commits/abcdef1234567":
			_, _ = w.Write([]byte(`{"sha":"abcdef1234567","commit":{"message":"Fix login","author":{"name":"Ada"}},"files":[{"filename":"b.go","previous_filename":"a.go","status":"renamed","patch":"@@ -1 +1 @@\n-a\n+b","additions":1,"deletions":1},{"filename":"new.txt","status":"added","patch":"@@ +1 @@\n+x","additions":1}]}`))
		case p == "/repos/acme/api/contents" || p == "/repos/acme/api/contents/src":
			_, _ = w.Write([]byte(`[{"name":"main.go","path":"src/main.go","type":"file","sha":"s1"},{"name":"pkg","path":"src/pkg","type":"dir","sha":"s2"}]`))
		case p == "/repos/acme/api/contents/src/main.go":
			_, _ = w.Write([]byte(`{"path":"src/main.go","content":"cGFj\na2FnZQ==\n","encoding":"base64","size":7,"sha":"s1","type":"file"}`))
		case strings.HasPrefix(p, "/repos/acme/api/compare/"):
			if p != "/repos/acme/api/compare/main...feature" {
				t.Errorf("compare path %s", p)
			}
			_, _ = w.Write([]byte(`{"ahead_by":2,"behind_by":1,"files":[{"filename":"x.go","status":"modified","patch":"@@ -1 +1 @@","additions":3,"deletions":1}]}`))
		case p == "/repos/acme/api/contributors":
			_, _ = w.Write([]byte(`[{"login":"ada","contributions":42}]`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
}

func githubRig(t *testing.T) (*GitRemoteHandler, *gorm.DB, func(method, path string, q string, fn gin.HandlerFunc) (int, map[string]any)) {
	t.Helper()
	srv := fakeGitHub(t)
	t.Cleanup(srv.Close)
	gitHubAPIBaseOverride = srv.URL
	t.Cleanup(func() { gitHubAPIBaseOverride = "" })
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitConnectionGrant{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	enc, _ := cipher.Encrypt("ghp_test")
	db.Create(&models.GitConnection{ID: "gh1", UserID: "u1", Name: "gh", Provider: "github", BaseURL: "https://github.com", AuthKind: "pat", CredentialEnc: enc})
	h := NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil)
	gin.SetMode(gin.TestMode)
	call := func(method, path, q string, fn gin.HandlerFunc) (int, map[string]any) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, path+"?"+q, nil)
		c.Params = gin.Params{{Key: "id", Value: "gh1"}}
		c.Set("userID", "u1")
		fn(c)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return h, db, call
}

func TestGitHubProviderReads(t *testing.T) {
	ghRepoCache.Range(func(k, _ any) bool { ghRepoCache.Delete(k); return true })
	h, _, call := githubRig(t)

	code, out := call("GET", "/repos", "", h.Repos)
	repos, _ := out["data"].([]any)
	if code != 200 || len(repos) != 1 {
		t.Fatalf("repos (first page only without search): %d %v", code, out)
	}
	r := repos[0].(map[string]any)
	if r["fullPath"] != "acme/api" || r["httpUrl"] != "https://github.com/acme/api.git" || r["visibility"] != "private" || r["id"].(float64) != 11 {
		t.Errorf("repo: %v", r)
	}
	if code, out = call("GET", "/repos", "search=web", h.Repos); code != 200 || len(out["data"].([]any)) != 1 || out["data"].([]any)[0].(map[string]any)["fullPath"] != "acme/web" {
		t.Errorf("search across pages: %v", out)
	}

	code, out = call("GET", "/branches", "project=11", h.Branches)
	br := out["data"].([]any)
	if code != 200 || br[0].(map[string]any)["name"] != "main" || br[0].(map[string]any)["isDefault"] != true {
		t.Errorf("branches (default first): %v", out)
	}

	code, out = call("GET", "/commits", "project=11&ref=main", h.Commits)
	d := out["data"].(map[string]any)
	cm := d["commits"].([]any)[0].(map[string]any)
	if code != 200 || cm["title"] != "Fix login" || cm["shortSha"] != "abcdef12" || d["nextPage"].(float64) != 2 {
		t.Errorf("commits: %v", out)
	}

	code, out = call("GET", "/commit", "project=11&sha=abcdef1234567", h.Commit)
	diffs := out["data"].(map[string]any)["diff"].([]any)
	if code != 200 || len(diffs) != 2 || diffs[0].(map[string]any)["oldPath"] != "a.go" || diffs[0].(map[string]any)["renamed"] != true || diffs[1].(map[string]any)["status"] != "added" {
		t.Errorf("commit diff: %v", out)
	}

	code, out = call("GET", "/tree", "project=11&path=src&ref=main", h.Tree)
	tree := out["data"].([]any)
	if code != 200 || tree[0].(map[string]any)["type"] != "tree" || tree[1].(map[string]any)["type"] != "blob" {
		t.Errorf("tree (folders first, dir→tree): %v", out)
	}

	code, out = call("GET", "/file", "project=11&path=src/main.go", h.File)
	f := out["data"].(map[string]any)
	if b, _ := base64.StdEncoding.DecodeString(f["content"].(string)); code != 200 || string(b) != "package" {
		t.Errorf("file content: %v", out)
	}

	code, out = call("GET", "/compare", "project=11&from=main&to=feature", h.Compare)
	cmp := out["data"].(map[string]any)
	if code != 200 || cmp["ahead"].(float64) != 2 || cmp["behind"].(float64) != 1 {
		t.Errorf("compare: %v", out)
	}

	if code, out = call("GET", "/contributors", "project=acme/api", h.Contributors); code != 200 || out["data"].([]any)[0].(map[string]any)["commits"].(float64) != 42 {
		t.Errorf("contributors by owner/name: %v", out)
	}
}

func TestGitHubGuardsAndTest(t *testing.T) {
	h, db, call := githubRig(t)
	// Token test: the right token answers the login, a wrong one fails.
	login, err := probeGitHubPAT(http.DefaultClient, "https://github.com", "ghp_test")
	if err != nil || login != "octo" {
		t.Errorf("probe: %q %v", login, err)
	}
	if _, err := probeGitHubPAT(http.DefaultClient, "https://github.com", "bad"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad token: %v", err)
	}
	// A GitHub connection without a token → 412 with what to do.
	db.Model(&models.GitConnection{}).Where("id = ?", "gh1").Update("credential_enc", "")
	if code, out := call("GET", "/repos", "", h.Repos); code != http.StatusPreconditionFailed || !strings.Contains(fmt.Sprint(out["error"]), "access token") {
		t.Errorf("no token: %d %v", code, out)
	}
	if got := gitHubAPIBase("https://ghe.acme.com/"); got != "https://ghe.acme.com/api/v3" {
		t.Errorf("enterprise base = %s", got)
	}
	if got := gitHubAPIBase("https://github.com"); got != "https://api.github.com" {
		t.Errorf("github.com base = %s", got)
	}
}

// git asks for a username with the token: GitHub gets x-access-token.
func TestAskpassGitHubUsername(t *testing.T) {
	env, cleanup, err := authEnvAs("x-access-token", "ghp_test", "https://github.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var script string
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_ASKPASS=") {
			script = strings.TrimPrefix(e, "GIT_ASKPASS=")
		}
	}
	cmd := exec.Command(script, "Username for 'https://github.com': ")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "x-access-token" {
		t.Fatalf("username = %q %v", out, err)
	}
}
