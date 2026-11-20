package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
)

// The askpass helper answers only for the connection's host: a remote the
// user added on another host must never receive the token.
func TestAskpassAnswersOnlyForOriginHost(t *testing.T) {
	path, cleanup, err := writeGitAskpass()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ask := func(host, prompt string) string {
		cmd := exec.Command(path, prompt)
		cmd.Env = append(os.Environ(), "PATCHMAN_GIT_TOKEN=secret-tok", "PATCHMAN_GIT_HOST="+host)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("askpass: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	cases := []struct{ host, prompt, want string }{
		{"git.example.com", "Username for 'https://git.example.com': ", "oauth2"},
		{"git.example.com", "Password for 'https://oauth2@git.example.com': ", "secret-tok"},
		{"git.example.com:8443", "Password for 'https://oauth2@git.example.com:8443': ", "secret-tok"},
		{"git.example.com", "Password for 'https://oauth2@evil.example.net': ", ""},
		{"git.example.com", "Password for 'https://oauth2@git.example.com.evil.net': ", ""},
		{"git.example.com", "Password for 'https://oauth2@git.example.com:9999': ", ""},
		{"", "Password for 'https://oauth2@git.example.com': ", ""},
	}
	for _, c := range cases {
		if got := ask(c.host, c.prompt); got != c.want {
			t.Errorf("host=%q prompt=%q: got %q, want %q", c.host, c.prompt, got, c.want)
		}
	}
}

func TestTokenHostAndRemoteURLValidation(t *testing.T) {
	if got := tokenHost("https://gitlab.example.com/g/p.git"); got != "gitlab.example.com" {
		t.Errorf("tokenHost: %q", got)
	}
	for _, u := range []string{"git@gitlab.example.com:g/p.git", "ssh://git@h/p", "", "file:///tmp/x"} {
		if tokenHost(u) != "" {
			t.Errorf("tokenHost(%q) must be empty", u)
		}
	}
	good := []string{"https://h.example.com/g/p.git", "ssh://git@h.example.com/g/p.git", "git@h.example.com:g/p.git"}
	bad := []string{"https://user:pw@h/p.git", "https://tok@h/p.git", "ext::sh -c touch% /tmp/pwn", "file:///etc", "-uhttps://h", "fd::17", "ssh://u:pw@h/p", "/local/path"}
	for _, u := range good {
		if !validRemoteURL(u) {
			t.Errorf("want valid: %q", u)
		}
	}
	for _, u := range bad {
		if validRemoteURL(u) {
			t.Errorf("want invalid: %q", u)
		}
	}
	if redactURL("https://u:p@h/x") != "https://h/x" {
		t.Error("redactURL")
	}
}

func TestForceDeleteBranchIsSensitive(t *testing.T) {
	if !middleware.IsSensitiveRoute("POST", "/api/git/repos/abc/branches/force-delete") {
		t.Fatal("force-delete must need step-up")
	}
	if !middleware.IsSensitiveRoute("POST", "/api/git/repos/abc/push-to/force") {
		t.Fatal("force push must need step-up")
	}
	if middleware.IsSensitiveRoute("POST", "/api/git/repos/abc/push-to") {
		t.Fatal("plain push must not need step-up")
	}
	if middleware.IsSensitiveRoute("POST", "/api/git/repos/abc/branches/delete") {
		t.Fatal("safe delete must not need step-up")
	}
}

// ── integration: a real origin + clone, driven through the handlers ──

type gitRig struct {
	t      *testing.T
	h      *GitOpsHandler
	r      *gin.Engine
	origin string
	work   string // the managed clone
	other  string // a second clone pushing to origin
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRig(t *testing.T) *gitRig {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	gin.SetMode(gin.TestMode)
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	seed := filepath.Join(base, "seed")
	run(t, base, "init", "--bare", "-b", "main", origin)
	run(t, base, "init", "-b", "main", seed)
	for _, k := range [][2]string{{"user.name", "t"}, {"user.email", "t@t"}} {
		run(t, seed, "config", k[0], k[1])
	}
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	run(t, seed, "add", ".")
	run(t, seed, "commit", "-m", "init")
	run(t, seed, "remote", "add", "origin", origin)
	run(t, seed, "push", "origin", "main")

	work := filepath.Join(base, "work")
	run(t, base, "clone", origin, work)
	other := filepath.Join(base, "other")
	run(t, base, "clone", origin, other)
	for _, d := range []string{work, other} {
		run(t, d, "config", "user.name", "t")
		run(t, d, "config", "user.email", "t@t")
	}

	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitRepo{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Create(&models.GitRepo{ID: "r1", UserID: "u1", ConnectionID: "c1", LocalPath: work, RemoteURL: "https://git.example.com/g/p.git", DefaultBranch: "main"})
	h := NewGitOpsHandler(db, nil, true)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "u1") })
	g := r.Group("/repos/:repoId")
	g.GET("/state", h.State)
	g.GET("/refs", h.Refs)
	g.POST("/fetch", h.Fetch)
	g.POST("/pull", h.Pull)
	g.POST("/checkout", h.Checkout)
	g.POST("/branches", h.CreateBranch)
	g.POST("/branches/rename", h.RenameBranch)
	g.POST("/branches/delete", h.DeleteBranch)
	g.POST("/branches/force-delete", h.ForceDeleteBranch)
	g.POST("/merge", h.Merge)
	g.POST("/rebase", h.Rebase)
	g.POST("/operation", h.Operation)
	g.GET("/remotes", h.Remotes)
	g.GET("/outgoing", h.Outgoing)
	g.GET("/commit-files", h.CommitFiles)
	g.POST("/push-to", h.PushTo)
	g.POST("/push-to/force", h.ForcePushTo)
	g.GET("/ls", h.LsDir)
	g.GET("/log", h.Log)
	g.GET("/blame", h.Blame)
	g.POST("/fs", h.FsOp)
	g.GET("/grep", h.Grep)
	g.GET("/files", h.Files)
	g.GET("/symbols", h.Symbols)
	g.POST("/remotes", h.AddRemote)
	g.POST("/remotes/update", h.UpdateRemote)
	g.POST("/remotes/delete", h.DeleteRemote)
	return &gitRig{t: t, h: h, r: r, origin: origin, work: work, other: other}
}

func (g *gitRig) call(method, path string, body any) (int, map[string]any) {
	g.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/repos/r1"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func data(m map[string]any) map[string]any { d, _ := m["data"].(map[string]any); return d }

// Diverged branches: ff-only refuses, merge stops on conflicts (200 +
// state), abort restores, rebase stops too, and continue after resolving.
func TestPullStrategiesAndConflictFlow(t *testing.T) {
	g := newGitRig(t)
	os.WriteFile(filepath.Join(g.other, "a.txt"), []byte("one\nTHEIRS\nthree\n"), 0o644)
	run(t, g.other, "commit", "-am", "theirs")
	run(t, g.other, "push", "origin", "main")
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("one\nOURS\nthree\n"), 0o644)
	run(t, g.work, "commit", "-am", "ours")

	// Fetch through the handler: origin is a local path, no token involved.
	if code, out := g.call("POST", "/fetch", map[string]any{}); code != 200 {
		t.Fatalf("fetch: %d %v", code, out)
	}
	if code, _ := g.call("POST", "/pull", map[string]any{"strategy": "ff", "accessToken": "x"}); code != http.StatusBadGateway {
		t.Fatalf("ff-only on diverged: want 502, got %d", code)
	}
	code, out := g.call("POST", "/pull", map[string]any{"strategy": "merge", "accessToken": "x"})
	if code != 200 || data(out)["conflicts"] != true {
		t.Fatalf("merge pull: %d %v", code, out)
	}
	_, st := g.call("GET", "/state", nil)
	if data(st)["operation"] != "merge" {
		t.Fatalf("state after conflict: %v", st)
	}
	// diff3 markers: the base section is there for the 3-pane resolver.
	if b, _ := os.ReadFile(filepath.Join(g.work, "a.txt")); !strings.Contains(string(b), "|||||||") {
		t.Fatalf("want diff3 markers, got:\n%s", b)
	}
	if code, _ := g.call("POST", "/operation", map[string]any{"action": "continue"}); code != http.StatusConflict {
		t.Fatalf("continue with conflicts must be refused, got %d", code)
	}
	if code, out := g.call("POST", "/operation", map[string]any{"action": "abort"}); code != 200 {
		t.Fatalf("abort: %d %v", code, out)
	}
	_, st = g.call("GET", "/state", nil)
	if data(st)["operation"] != "" {
		t.Fatalf("after abort: %v", st)
	}

	// Rebase strategy: stops on the conflict, resolve, continue (no editor).
	code, out = g.call("POST", "/pull", map[string]any{"strategy": "rebase", "accessToken": "x"})
	if code != 200 || data(out)["conflicts"] != true {
		t.Fatalf("rebase pull: %d %v", code, out)
	}
	_, st = g.call("GET", "/state", nil)
	if data(st)["operation"] != "rebase" {
		t.Fatalf("state: %v", st)
	}
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("one\nBOTH\nthree\n"), 0o644)
	run(t, g.work, "add", "a.txt")
	if code, out := g.call("POST", "/operation", map[string]any{"action": "continue"}); code != 200 || data(out)["ok"] != true {
		t.Fatalf("rebase continue: %d %v", code, out)
	}
	if got := run(t, g.work, "log", "--format=%s", "-2"); got != "ours\ntheirs" {
		t.Fatalf("history after rebase: %q", got)
	}
}

func TestBranchesCheckoutAndRemotes(t *testing.T) {
	g := newGitRig(t)
	run(t, g.other, "switch", "-c", "feature/x")
	os.WriteFile(filepath.Join(g.other, "b.txt"), []byte("b\n"), 0o644)
	run(t, g.other, "add", ".")
	run(t, g.other, "commit", "-m", "feat")
	run(t, g.other, "push", "origin", "feature/x")
	g.call("POST", "/fetch", map[string]any{})

	// Remote branch → local tracking branch.
	if code, out := g.call("POST", "/checkout", map[string]any{"ref": "origin/feature/x"}); code != 200 {
		t.Fatalf("checkout remote: %d %v", code, out)
	}
	if run(t, g.work, "rev-parse", "--abbrev-ref", "HEAD@{upstream}") != "origin/feature/x" {
		t.Fatal("tracking not set")
	}
	_, refs := g.call("GET", "/refs", nil)
	d := data(refs)
	if d["current"] != "feature/x" || len(d["recent"].([]any)) == 0 {
		t.Fatalf("refs: %v", d)
	}

	// Dirty tree that conflicts with the target → 409 localChanges; smart works.
	os.WriteFile(filepath.Join(g.work, "b.txt"), []byte("dirty\n"), 0o644)
	if code, out := g.call("POST", "/checkout", map[string]any{"ref": "main"}); code != 200 {
		// b.txt does not exist on main: git keeps the change and switches,
		// or refuses; both are fine — only check smart below.
		_ = out
	}
	run(t, g.work, "switch", "feature/x")
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("local edit\n"), 0o644)
	run(t, g.work, "switch", "-c", "side")
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("side\n"), 0o644)
	run(t, g.work, "commit", "-am", "side")
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("dirty on side\n"), 0o644)
	code, out := g.call("POST", "/checkout", map[string]any{"ref": "main"})
	if code != http.StatusConflict || out["localChanges"] != true {
		t.Fatalf("dirty checkout: %d %v", code, out)
	}
	code, out = g.call("POST", "/checkout", map[string]any{"ref": "main", "smart": true})
	if code != 200 || data(out)["stashConflict"] != true {
		t.Fatalf("smart checkout: %d %v", code, out)
	}
	run(t, g.work, "reset", "-q", "--hard")
	run(t, g.work, "stash", "drop")

	// New branch, rename, safe delete refused when unmerged, force works.
	if code, out := g.call("POST", "/branches", map[string]any{"name": "tmp", "startPoint": "side"}); code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, _ := g.call("POST", "/branches", map[string]any{"name": "bad..name"}); code != 400 {
		t.Fatal("invalid name must be refused")
	}
	if code, _ := g.call("POST", "/branches/rename", map[string]any{"name": "tmp", "newName": "tmp2"}); code != 200 {
		t.Fatal("rename")
	}
	code, out = g.call("POST", "/branches/delete", map[string]any{"name": "tmp2"})
	if code != http.StatusConflict || out["notMerged"] != true {
		t.Fatalf("safe delete of unmerged: %d %v", code, out)
	}
	if code, _ := g.call("POST", "/branches/force-delete", map[string]any{"name": "tmp2"}); code != 200 {
		t.Fatal("force delete")
	}

	// Merge a branch into current (no-ff) without an editor.
	if code, out := g.call("POST", "/merge", map[string]any{"ref": "feature/x", "mode": "no-ff"}); code != 200 || data(out)["ok"] != true {
		t.Fatalf("merge: %d %v", code, out)
	}

	// Remotes: add (validated), list with auth flag, rename, origin protected.
	if code, _ := g.call("POST", "/remotes", map[string]any{"name": "up", "url": "https://tok@evil/x.git"}); code != 400 {
		t.Fatal("credentials in URL must be refused")
	}
	if code, _ := g.call("POST", "/remotes", map[string]any{"name": "up", "url": "ext::sh -c id"}); code != 400 {
		t.Fatal("ext:: must be refused")
	}
	if code, out := g.call("POST", "/remotes", map[string]any{"name": "up", "url": "https://other.example.com/x.git"}); code != 200 {
		t.Fatalf("add remote: %d %v", code, out)
	}
	if code, _ := g.call("POST", "/remotes/update", map[string]any{"name": "up", "newName": "upstream"}); code != 200 {
		t.Fatal("rename remote")
	}
	_, rl := g.call("GET", "/remotes", nil)
	list := rl["data"].([]any)
	if len(list) != 2 || list[1].(map[string]any)["name"] != "upstream" || list[1].(map[string]any)["authenticated"] != false {
		t.Fatalf("remotes: %v", list)
	}
	if code, _ := g.call("POST", "/remotes/delete", map[string]any{"name": "origin"}); code != 400 {
		t.Fatal("origin must be protected")
	}
	if code, _ := g.call("POST", "/remotes/delete", map[string]any{"name": "upstream"}); code != 200 {
		t.Fatal("delete remote")
	}
}

// Outgoing commits, their files, a push to a new branch, a rejected push
// and a force-with-lease push.
func TestOutgoingCommitFilesAndPushTo(t *testing.T) {
	g := newGitRig(t)
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("one\nTWO\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "new.txt"), []byte("x\ny\n"), 0o644)
	run(t, g.work, "add", ".")
	run(t, g.work, "commit", "-m", "local change")

	_, out := g.call("GET", "/outgoing?remote=origin&branch=main", nil)
	d := data(out)
	if cs := d["commits"].([]any); len(cs) != 1 || cs[0].(map[string]any)["title"] != "local change" || d["remoteExists"] != true {
		t.Fatalf("outgoing: %v", d)
	}
	sha := d["commits"].([]any)[0].(map[string]any)["sha"].(string)
	_, fo := g.call("GET", "/commit-files?sha="+sha, nil)
	files := fo["data"].([]any)
	got := map[string]map[string]any{}
	for _, f := range files {
		m := f.(map[string]any)
		got[m["path"].(string)] = m
	}
	if got["a.txt"]["status"] != "modified" || got["a.txt"]["additions"].(float64) != 1 || got["new.txt"]["status"] != "added" || got["new.txt"]["additions"].(float64) != 2 {
		t.Fatalf("commit files: %v", files)
	}

	// A new branch on the remote: outgoing = commits no origin branch has.
	_, out = g.call("GET", "/outgoing?remote=origin&branch=feature/new", nil)
	if d := data(out); d["remoteExists"] != false || len(d["commits"].([]any)) != 1 {
		t.Fatalf("new-branch outgoing: %v", d)
	}
	if code, out := g.call("POST", "/push-to", map[string]any{"remote": "origin", "targetBranch": "feature/new", "setUpstream": true}); code != 200 {
		t.Fatalf("push new branch: %d %v", code, out)
	}
	if run(t, g.origin, "rev-parse", "refs/heads/feature/new") != sha {
		t.Fatal("origin did not get the branch")
	}

	// Someone else moves origin/main: a plain push is rejected, force-with-lease
	// (after a fetch, so the lease matches) goes through.
	os.WriteFile(filepath.Join(g.other, "b.txt"), []byte("b"), 0o644)
	run(t, g.other, "add", ".")
	run(t, g.other, "commit", "-m", "theirs")
	run(t, g.other, "push", "origin", "main")
	code, out := g.call("POST", "/push-to", map[string]any{"remote": "origin", "targetBranch": "main"})
	if code != http.StatusConflict || out["rejected"] != true {
		t.Fatalf("diverged push must be rejected: %d %v", code, out)
	}
	g.call("POST", "/fetch", map[string]any{})
	if code, out := g.call("POST", "/push-to/force", map[string]any{"remote": "origin", "targetBranch": "main"}); code != 200 {
		t.Fatalf("force-with-lease: %d %v", code, out)
	}
	if run(t, g.origin, "rev-parse", "refs/heads/main") != sha {
		t.Fatal("force push did not land")
	}
	if code, _ := g.call("POST", "/push-to", map[string]any{"remote": "up", "targetBranch": "main", "openMr": true}); code != 400 {
		t.Fatal("MR on a non-origin remote must be refused")
	}
}

func TestWorkingTreeLsBlameFs(t *testing.T) {
	g := newGitRig(t)
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("one\nCHANGED\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "notes.md"), []byte("n"), 0o644)
	os.WriteFile(filepath.Join(g.work, ".gitignore"), []byte("*.log\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "debug.log"), []byte("x"), 0o644)

	_, out := g.call("GET", "/ls", nil)
	got := map[string]map[string]any{}
	for _, e := range out["data"].([]any) {
		m := e.(map[string]any)
		got[m["name"].(string)] = m
	}
	if _, ok := got[".git"]; ok {
		t.Fatal(".git must not be listed")
	}
	if got["a.txt"]["status"] != "modified" || got["notes.md"]["status"] != "untracked" || got["debug.log"]["ignored"] != true {
		t.Fatalf("ls: %v", got)
	}

	_, out = g.call("GET", "/blame?path=a.txt", nil)
	lines := out["data"].([]any)
	if len(lines) != 3 || !strings.HasPrefix(lines[1].(map[string]any)["sha"].(string), "0000000") || lines[0].(map[string]any)["summary"] != "init" {
		t.Fatalf("blame: %v", lines)
	}

	// create / mkdir / rename (git mv when tracked) / delete (git rm).
	if code, out := g.call("POST", "/fs", map[string]any{"op": "create", "path": "src/new.go", "content": "package src\n"}); code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, _ := g.call("POST", "/fs", map[string]any{"op": "create", "path": "src/new.go"}); code != http.StatusConflict {
		t.Fatal("create over an existing file must be refused")
	}
	if code, _ := g.call("POST", "/fs", map[string]any{"op": "mkdir", "path": "docs/api"}); code != 200 {
		t.Fatal("mkdir")
	}
	if code, out := g.call("POST", "/fs", map[string]any{"op": "rename", "path": "a.txt", "newPath": "docs/a.txt"}); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	if st := run(t, g.work, "status", "--porcelain"); !strings.Contains(st, "R  a.txt -> docs/a.txt") && !strings.Contains(st, "RM a.txt -> docs/a.txt") {
		t.Fatalf("tracked rename must be a git mv: %q", st)
	}
	if code, _ := g.call("POST", "/fs", map[string]any{"op": "delete", "path": "docs"}); code != 200 {
		t.Fatal("delete folder")
	}
	if _, err := os.Stat(filepath.Join(g.work, "docs")); !os.IsNotExist(err) {
		t.Fatal("folder still on disk")
	}

	// Escapes are refused: .., .git, a symlink leading out.
	for _, bad := range []map[string]any{
		{"op": "create", "path": "../evil.txt"},
		{"op": "create", "path": ".git/hooks/pre-commit"},
		{"op": "delete", "path": "/etc/hosts"},
	} {
		if code, _ := g.call("POST", "/fs", bad); code != 400 {
			t.Fatalf("must refuse %v (got %d)", bad, code)
		}
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(g.work, "out"))
	if code, _ := g.call("POST", "/fs", map[string]any{"op": "create", "path": "out/pwn.txt"}); code != 400 {
		t.Fatal("a symlink leading out must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); err == nil {
		t.Fatal("wrote through the symlink")
	}
}

func TestSearchGrepFilesSymbols(t *testing.T) {
	g := newGitRig(t)
	os.MkdirAll(filepath.Join(g.work, "src", "web"), 0o755)
	os.WriteFile(filepath.Join(g.work, "src", "greet.go"), []byte("package main\n\n// greet says hi\nfunc greet(name string) string {\n\treturn \"hi \" + name\n}\n\ntype Greeter struct{}\n\nfunc (g *Greeter) Greet() {}\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "src", "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(greet(\"x\"))\n}\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "src", "web", "app.ts"), []byte("export function greetUser(u: string) { return u }\nexport class GreetService {}\nconst greeting = greetUser('a')\n"), 0o644)
	os.WriteFile(filepath.Join(g.work, "logo.png"), []byte("\x89PNG\x00\x00greet"), 0o644)

	_, out := g.call("GET", "/grep?q=greet&word=1", nil)
	hits := data(out)["hits"].([]any)
	paths := map[string]int{}
	for _, h := range hits {
		paths[h.(map[string]any)["path"].(string)]++
	}
	if paths["src/greet.go"] != 3 || paths["src/main.go"] != 1 || paths["logo.png"] != 0 {
		t.Fatalf("grep word: %v", paths)
	}
	_, out = g.call("GET", "/grep?q=greet%5BA-Z%5D&regex=1&case=1&glob=*.ts", nil)
	// case-sensitive: greetUser (lines 1, 3) yes, GreetService no; the .go files are outside the glob.
	if hits := data(out)["hits"].([]any); len(hits) != 2 || hits[0].(map[string]any)["line"].(float64) != 1 || hits[1].(map[string]any)["line"].(float64) != 3 {
		t.Fatalf("grep regex+glob: %v", hits)
	}

	_, out = g.call("GET", "/files?q=grt", nil)
	files := out["data"].([]any)
	if len(files) == 0 || files[0] != "src/greet.go" {
		t.Fatalf("files fuzzy: %v", files)
	}

	_, out = g.call("GET", "/symbols?q=greet&exact=1", nil)
	syms := out["data"].([]any)
	if len(syms) != 1 || syms[0].(map[string]any)["path"] != "src/greet.go" || syms[0].(map[string]any)["line"].(float64) != 4 {
		t.Fatalf("exact definition: %v", syms)
	}
	_, out = g.call("GET", "/symbols?q=Greet", nil)
	names := map[string]bool{}
	for _, s := range out["data"].([]any) {
		names[s.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"greet", "Greeter", "greetUser", "GreetService"} {
		if !names[want] {
			t.Errorf("prefix symbols missing %s: %v", want, names)
		}
	}
	if names["main"] {
		t.Error("prefix search must not return unrelated names")
	}
}

func TestFsRollbackAndFolderStatus(t *testing.T) {
	g := newGitRig(t)
	orig := run(t, g.work, "show", "HEAD:a.txt")

	// A modified tracked file goes back to HEAD (index and working tree).
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("changed\n"), 0o644)
	run(t, g.work, "add", "a.txt")
	if code, out := g.call("POST", "/fs", map[string]any{"op": "rollback", "path": "a.txt"}); code != 200 {
		t.Fatalf("rollback modified: %d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(g.work, "a.txt")); strings.TrimSpace(string(b)) != orig {
		t.Fatalf("not restored: %q", b)
	}
	if st := run(t, g.work, "status", "--porcelain"); st != "" {
		t.Fatalf("still changed: %q", st)
	}

	// A folder with a change under it is marked in its parent listing.
	os.MkdirAll(filepath.Join(g.work, "src"), 0o755)
	os.WriteFile(filepath.Join(g.work, "src", "new.go"), []byte("package src\n"), 0o644)
	_, out := g.call("GET", "/ls?path=", nil)
	found := false
	for _, e := range out["data"].([]any) {
		m := e.(map[string]any)
		if m["path"] == "src" {
			found = m["status"] == "modified"
		}
	}
	if !found {
		t.Fatalf("folder status: %v", out["data"])
	}

	// A new (staged) file not in HEAD is unstaged and removed.
	run(t, g.work, "add", "src/new.go")
	if code, out := g.call("POST", "/fs", map[string]any{"op": "rollback", "path": "src/new.go"}); code != 200 {
		t.Fatalf("rollback new: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(g.work, "src", "new.go")); !os.IsNotExist(err) {
		t.Fatal("new file still on disk")
	}
	if st := run(t, g.work, "status", "--porcelain"); st != "" {
		t.Fatalf("index still has it: %q", st)
	}
}

func TestLogPathFilter(t *testing.T) {
	g := newGitRig(t)
	os.WriteFile(filepath.Join(g.work, "b.txt"), []byte("b\n"), 0o644)
	run(t, g.work, "add", "b.txt")
	run(t, g.work, "commit", "-m", "add b")
	_, all := g.call("GET", "/log", nil)
	_, onlyB := g.call("GET", "/log?path=b.txt", nil)
	if len(all["data"].([]any)) < 2 || len(onlyB["data"].([]any)) != 1 {
		t.Fatalf("all=%d onlyB=%d", len(all["data"].([]any)), len(onlyB["data"].([]any)))
	}
	if code, _ := g.call("GET", "/log?path=../etc", nil); code != 400 {
		t.Fatal("path escape must be refused")
	}
}

func TestLsStatusInSubfolderRepo(t *testing.T) {
	g := newGitRig(t)
	os.MkdirAll(filepath.Join(g.work, "svc", "pkg"), 0o755)
	os.WriteFile(filepath.Join(g.work, "svc", "pkg", "x.go"), []byte("package pkg\n"), 0o644)
	// porcelainStatus run in a subfolder must report paths relative to it.
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest("GET", "/", nil)
	st, _ := g.h.porcelainStatus(gc, filepath.Join(g.work, "svc"))
	if st["pkg/x.go"] != "untracked" || st["pkg"] != "modified" {
		t.Fatalf("subfolder status: %v", st)
	}
}
