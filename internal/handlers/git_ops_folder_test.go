package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/models"
)

func TestRemoteHostPath(t *testing.T) {
	cases := []struct{ in, host, path string }{
		{"https://gitlab.example.com/grp/sub/proj.git", "gitlab.example.com", "grp/sub/proj"},
		{"https://gitlab.example.com:8443/grp/proj", "gitlab.example.com", "grp/proj"},
		{"ssh://git@gitlab.example.com:2222/grp/proj.git", "gitlab.example.com", "grp/proj"},
		{"git@gitlab.example.com:grp/proj.git", "gitlab.example.com", "grp/proj"},
		{"/some/local/origin.git", "", ""},
		{"", "", ""},
		{"https://gitlab.example.com/", "", ""},
	}
	for _, c := range cases {
		h, p := remoteHostPath(c.in)
		if h != c.host || p != c.path {
			t.Errorf("remoteHostPath(%q) = %q,%q; want %q,%q", c.in, h, p, c.host, c.path)
		}
	}
}

func TestOriginProject(t *testing.T) {
	cases := []struct{ origin, base, want string }{
		{"https://git.example.com/grp/proj.git", "https://git.example.com", "grp/proj"},
		{"https://git.example.com/grp/proj.git", "https://git.example.com/", "grp/proj"},
		{"git@git.example.com:grp/proj.git", "https://git.example.com", "grp/proj"},
		{"ssh://git@git.example.com:2222/grp/proj.git", "https://git.example.com", "grp/proj"},
		{"https://host.example.com/gitlab/grp/proj.git", "https://host.example.com/gitlab", "grp/proj"},
		// Look-alikes must not link (the token would be answered for them).
		{"https://git.example.com:9999/grp/proj.git", "https://git.example.com", ""},
		{"http://git.example.com/grp/proj.git", "https://git.example.com", ""},
		{"https://git.example.com.evil.net/grp/proj.git", "https://git.example.com", ""},
		{"https://github.com/grp/proj.git", "https://git.example.com", ""},
		{"https://host.example.com/other/proj.git", "https://host.example.com/gitlab", ""},
		{"/local/origin.git", "https://git.example.com", ""},
	}
	for _, c := range cases {
		if got := originProject(c.origin, c.base); got != c.want {
			t.Errorf("originProject(%q, %q) = %q, want %q", c.origin, c.base, got, c.want)
		}
	}
	if connBaseURL(models.GitConnection{}) == "" {
		t.Error("empty BaseURL must fall back to the default GitLab")
	}
}

func TestOpenFolder(t *testing.T) {
	g := newGitRig(t)
	g.h.db.Create(&models.GitConnection{ID: "c1", UserID: "u1", Name: "gl", BaseURL: "https://git.example.com"})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "u1") })
	r.POST("/repos/open", g.h.OpenFolder)
	r.GET("/repos", g.h.ListRepos)
	open := func(id, path string) (int, models.GitRepo) {
		b, _ := json.Marshal(gitOpenFolderInput{RepoID: id, Path: path})
		req := httptest.NewRequest("POST", "/repos/open", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out struct {
			Data struct {
				Repo models.GitRepo `json:"repo"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data.Repo
	}

	// A subfolder resolves to the work-tree root; no connection matches a
	// local-path origin, so the repo stands alone.
	sub := filepath.Join(g.other, "pkg")
	os.MkdirAll(sub, 0o755)
	code, repo := open("x1", sub)
	root, _ := filepath.EvalSymlinks(g.other)
	if code != 200 || repo.LocalPath != root || repo.ProjectRef != "local:x1" || repo.ConnectionID != "" || repo.CloneKind != cloneKindExternal {
		t.Fatalf("open: %d %+v", code, repo)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("default branch: %q", repo.DefaultBranch)
	}
	// Reopening the same folder returns the existing row.
	if code, again := open("x2", g.other); code != 200 || again.ID != "x1" {
		t.Fatalf("reopen: %d %+v", code, again)
	}

	// origin on a connection's host ties it to that connection + project path.
	run(t, g.work, "remote", "set-url", "origin", "https://git.example.com/grp/proj.git")
	if code, linked := open("x3", g.work); code != 200 {
		t.Fatalf("linked: %d", code)
	} else if linked.ID != "r1" {
		// g.work is the rig's managed clone (r1): already registered → reused.
		t.Fatalf("managed clone must be reused, got %+v", linked)
	}
	third := filepath.Join(t.TempDir(), "third")
	run(t, filepath.Dir(third), "clone", g.origin, third)
	run(t, third, "remote", "set-url", "origin", "git@git.example.com:grp/proj.git")
	if code, linked := open("x4", third); code != 200 || linked.ConnectionID != "c1" || linked.ProjectRef != "grp/proj" {
		t.Fatalf("ssh origin match: %d %+v", code, linked)
	}
	// Credentials embedded in origin never reach the stored row / response.
	fourth := filepath.Join(t.TempDir(), "fourth")
	run(t, filepath.Dir(fourth), "clone", g.origin, fourth)
	run(t, fourth, "remote", "set-url", "origin", "https://oauth2:s3cret@git.example.com:9999/grp/proj.git")
	if code, rp := open("x8", fourth); code != 200 || rp.ConnectionID != "" || strings.Contains(rp.RemoteURL, "s3cret") {
		t.Fatalf("look-alike origin: %d %+v", code, rp)
	}

	// Rejections.
	plain := t.TempDir()
	if code, _ := open("x5", plain); code != http.StatusUnprocessableEntity {
		t.Errorf("non-git folder: %d", code)
	}
	if code, _ := open("x6", filepath.Join(plain, "nope")); code != http.StatusNotFound {
		t.Errorf("missing folder: %d", code)
	}
	if code, _ := open("x7", "relative/dir"); code != http.StatusBadRequest {
		t.Errorf("relative path: %d", code)
	}
	if code, _ := open("../bad", g.other); code != http.StatusBadRequest {
		t.Errorf("bad repoId: %d", code)
	}

	// A folder removed outside the app is flagged, not dropped.
	os.RemoveAll(third)
	req := httptest.NewRequest("GET", "/repos", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var list struct{ Data []models.GitRepo }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	found := false
	for _, rp := range list.Data {
		if rp.ID == "x4" {
			found = true
			if !rp.Missing {
				t.Error("x4 must be flagged missing")
			}
		} else if rp.Missing {
			t.Errorf("%s flagged missing", rp.ID)
		}
	}
	if !found {
		t.Error("x4 missing from list")
	}
	if _, err := os.Stat(filepath.Join(g.other, "a.txt")); err != nil {
		t.Error("opened folder's files must be untouched")
	}
}

// A stand-alone folder's origin may be any host: a PatchMan token must never
// be offered to it, even when the FE supplies one.
func TestExternalFolderNeverGetsToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &GitOpsHandler{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	standalone := &models.GitRepo{CloneKind: cloneKindExternal, RemoteURL: "https://github.com/x/y.git"}
	env, cleanup, ok := h.withAuth(c, standalone, "gitlab-token")
	defer cleanup()
	if !ok || env != nil {
		t.Fatalf("stand-alone folder got a token env: %v", env)
	}
	linked := &models.GitRepo{CloneKind: cloneKindExternal, ConnectionID: "c1", RemoteURL: "https://git.example.com/g/p.git"}
	env, cleanup2, ok := h.withAuth(c, linked, "gitlab-token")
	defer cleanup2()
	if !ok || env == nil {
		t.Fatal("linked folder must get the token env")
	}
}

// A save carrying the loaded content's hash is refused once the file changed
// on disk (another editor, a checkout) — auto save must never clobber it.
func TestWriteFileExpectedHash(t *testing.T) {
	g := newGitRig(t)
	g.r.POST("/repos/:repoId/file", g.h.WriteFile)
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	orig, _ := os.ReadFile(filepath.Join(g.work, "a.txt"))
	if code, _ := g.call("POST", "/file", gin.H{"path": "a.txt", "content": "v2\n", "expectedSha256": sum(string(orig))}); code != 200 {
		t.Fatalf("matching hash: %d", code)
	}
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("changed elsewhere\n"), 0o644)
	if code, body := g.call("POST", "/file", gin.H{"path": "a.txt", "content": "v3\n", "expectedSha256": sum("v2\n")}); code != http.StatusConflict || body["code"] != "changed_on_disk" {
		t.Fatalf("stale hash must 409: %d %v", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(g.work, "a.txt")); string(b) != "changed elsewhere\n" {
		t.Fatalf("file was overwritten: %q", b)
	}
	// No hash = old behaviour (explicit overwrite).
	if code, _ := g.call("POST", "/file", gin.H{"path": "a.txt", "content": "v3\n"}); code != 200 {
		t.Fatalf("force write: %d", code)
	}
	// New file: the hash of empty content matches a missing file.
	if code, _ := g.call("POST", "/file", gin.H{"path": "new/b.txt", "content": "x", "expectedSha256": sum("")}); code != 200 {
		t.Fatalf("new file: %d", code)
	}
}

// A folder opened before its connection matched is linked on a later open,
// can be linked by hand (only to a connection on its origin's host), and is
// what "Open locally" reuses instead of cloning again.
func TestFolderRelinkLinkAndReuse(t *testing.T) {
	g := newGitRig(t)
	g.h.db.Create(&models.GitConnection{ID: "c1", UserID: "u1", Name: "gl", BaseURL: "https://git.example.com"})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "u1") })
	r.POST("/repos/open", g.h.OpenFolder)
	r.POST("/repos/:repoId/link", g.h.Link)
	call := func(path string, body any) (int, models.GitRepo) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out struct {
			Data struct {
				Repo models.GitRepo `json:"repo"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data.Repo
	}

	// Opened while its origin matches nothing → stand-alone.
	if code, rp := call("/repos/open", gitOpenFolderInput{RepoID: "f1", Path: g.other}); code != 200 || rp.ConnectionID != "" {
		t.Fatalf("open: %d %+v", code, rp)
	}
	// Linking by hand is refused while the origin is on another host.
	if code, _ := call("/repos/f1/link", gitLinkInput{ConnectionID: "c1", ProjectRef: "7", ProjectName: "grp/other"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("link to foreign host: %d", code)
	}
	// Once origin points at the connection's host, reopening links it.
	run(t, g.other, "remote", "set-url", "origin", "https://git.example.com/grp/other.git")
	if code, rp := call("/repos/open", gitOpenFolderInput{RepoID: "f2", Path: g.other}); code != 200 || rp.ID != "f1" || rp.ConnectionID != "c1" || rp.ProjectRef != "grp/other" {
		t.Fatalf("relink on reopen: %d %+v", code, rp)
	}
	// "Open locally" for that project (selected by numeric id, path as name)
	// finds this tree instead of cloning.
	if got := g.h.findProjectRepo(context.Background(), "u1", "c1", "999", "grp/other"); got == nil || got.ID != "f1" {
		t.Fatalf("reuse by path: %+v", got)
	}
	if got := g.h.findProjectRepo(context.Background(), "u1", "c1", "999", "grp/none"); got != nil {
		t.Fatalf("no reuse for another project: %+v", got)
	}

	// Manual link to a chosen project on the right host.
	fifth := filepath.Join(t.TempDir(), "fifth")
	run(t, filepath.Dir(fifth), "clone", g.origin, fifth)
	run(t, fifth, "remote", "set-url", "origin", "https://mirror.example.org/a/b.git")
	if code, rp := call("/repos/open", gitOpenFolderInput{RepoID: "f5", Path: fifth}); code != 200 || rp.ConnectionID != "" {
		t.Fatalf("open fifth: %d %+v", code, rp)
	}
	run(t, fifth, "remote", "set-url", "origin", "git@git.example.com:grp/renamed.git")
	if code, rp := call("/repos/f5/link", gitLinkInput{ConnectionID: "c1", ProjectRef: "42", ProjectName: "grp/real"}); code != 200 || rp.ConnectionID != "c1" || rp.ProjectRef != "42" || rp.ProjectName != "grp/real" {
		t.Fatalf("manual link: %d %+v", code, rp)
	}
	if code, _ := call("/repos/f5/link", gitLinkInput{ConnectionID: "nope", ProjectRef: "42"}); code != http.StatusNotFound {
		t.Fatalf("unknown connection: %d", code)
	}
	if code, _ := call("/repos/zzz/link", gitLinkInput{ConnectionID: "c1", ProjectRef: "42"}); code != http.StatusNotFound {
		t.Fatalf("unknown repo: %d", code)
	}
}
