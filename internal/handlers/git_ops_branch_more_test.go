package handlers

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/middleware"
)

func (g *gitRig) routeMore() {
	r := g.r.Group("/repos/:repoId")
	r.POST("/branches/update", g.h.UpdateBranch)
	r.POST("/branches/upstream", g.h.SetUpstream)
	r.GET("/compare", g.h.Compare)
	r.GET("/diff-worktree", g.h.DiffWorkTree)
	r.POST("/remote-branches/delete", g.h.DeleteRemoteBranch)
	r.POST("/tags/delete", g.h.DeleteTag)
	r.POST("/worktrees", g.h.AddWorktree)
}

// JetBrains "Update" on a branch that is not checked out: fast-forward it
// from its upstream without touching the working tree; refuse the current
// branch and a diverged one.
func TestUpdateBranchNotCheckedOut(t *testing.T) {
	g := newGitRig(t)
	g.routeMore()
	run(t, g.work, "branch", "--track", "side", "origin/main")
	run(t, g.work, "branch", "--track", "div", "origin/main")
	// origin moves on
	os.WriteFile(filepath.Join(g.other, "b.txt"), []byte("b\n"), 0o644)
	run(t, g.other, "add", ".")
	run(t, g.other, "commit", "-m", "upstream")
	run(t, g.other, "push", "origin", "main")
	// "div" gets a local commit → diverged
	run(t, g.work, "checkout", "-q", "div")
	os.WriteFile(filepath.Join(g.work, "c.txt"), []byte("c\n"), 0o644)
	run(t, g.work, "add", ".")
	run(t, g.work, "commit", "-m", "local")
	run(t, g.work, "checkout", "-q", "main")
	headBefore := run(t, g.work, "rev-parse", "HEAD")

	code, body := g.call("POST", "/branches/update", gin.H{"name": "side"})
	if code != 200 || data(body)["updated"] != true {
		t.Fatalf("update side: %d %v", code, body)
	}
	if run(t, g.work, "rev-parse", "side") != run(t, g.other, "rev-parse", "HEAD") {
		t.Fatal("side not fast-forwarded to origin/main")
	}
	if run(t, g.work, "rev-parse", "HEAD") != headBefore {
		t.Fatal("checked-out branch moved")
	}
	if code, body := g.call("POST", "/branches/update", gin.H{"name": "main"}); code != 400 || body["current"] != true {
		t.Fatalf("current branch must be refused: %d %v", code, body)
	}
	if code, body := g.call("POST", "/branches/update", gin.H{"name": "div"}); code != http.StatusConflict || body["diverged"] != true {
		t.Fatalf("diverged must 409: %d %v", code, body)
	}
	run(t, g.work, "branch", "lonely")
	if code, body := g.call("POST", "/branches/update", gin.H{"name": "lonely"}); code != 400 || body["noUpstream"] != true {
		t.Fatalf("no upstream: %d %v", code, body)
	}
}

// Push a branch that is not checked out, compare it, track/untrack, delete it
// on the remote, and checkout-and-rebase.
func TestPushSourceCompareTrackDeleteRemote(t *testing.T) {
	g := newGitRig(t)
	g.routeMore()
	run(t, g.work, "checkout", "-q", "-b", "topic")
	os.WriteFile(filepath.Join(g.work, "t.txt"), []byte("t\n"), 0o644)
	run(t, g.work, "add", ".")
	run(t, g.work, "commit", "-m", "topic work")
	run(t, g.work, "checkout", "-q", "main")

	if code, body := g.call("POST", "/push-to", gin.H{"source": "topic", "remote": "origin"}); code != 200 || data(body)["branch"] != "topic" {
		t.Fatalf("push topic: %d %v", code, body)
	}
	if run(t, g.origin, "rev-parse", "refs/heads/topic") != run(t, g.work, "rev-parse", "topic") {
		t.Fatal("origin/topic not at local topic")
	}
	if run(t, g.work, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatal("push of another branch must not switch branches")
	}

	code, body := g.call("GET", "/compare?base=main&ref=topic", nil)
	d := data(body)
	if code != 200 || len(d["refOnly"].([]any)) != 1 || len(d["baseOnly"].([]any)) != 0 {
		t.Fatalf("compare: %d %v", code, body)
	}

	run(t, g.work, "fetch", "-q", "origin")
	if code, _ := g.call("POST", "/branches/upstream", gin.H{"name": "topic", "upstream": "origin/topic"}); code != 200 {
		t.Fatalf("set upstream: %d", code)
	}
	if got := run(t, g.work, "rev-parse", "--abbrev-ref", "topic@{u}"); got != "origin/topic" {
		t.Fatalf("upstream = %q", got)
	}
	if code, _ := g.call("POST", "/branches/upstream", gin.H{"name": "topic", "upstream": "origin/nope"}); code != 400 {
		t.Fatal("unknown upstream must 400")
	}
	if code, _ := g.call("POST", "/branches/upstream", gin.H{"name": "topic", "upstream": ""}); code != 200 {
		t.Fatal("unset upstream")
	}

	// Checkout and Rebase: main gets a commit, topic is rebased onto it.
	os.WriteFile(filepath.Join(g.work, "m.txt"), []byte("m\n"), 0o644)
	run(t, g.work, "add", ".")
	run(t, g.work, "commit", "-m", "main work")
	if code, body := g.call("POST", "/rebase", gin.H{"onto": "main", "branch": "topic"}); code != 200 || data(body)["ok"] != true {
		t.Fatalf("checkout and rebase: %d %v", code, body)
	}
	if run(t, g.work, "rev-parse", "--abbrev-ref", "HEAD") != "topic" || run(t, g.work, "merge-base", "main", "topic") != run(t, g.work, "rev-parse", "main") {
		t.Fatal("topic not checked out / not rebased onto main")
	}

	if code, _ := g.call("POST", "/remote-branches/delete", gin.H{"remote": "origin", "branch": "topic"}); code != 200 {
		t.Fatalf("delete remote branch: %d", code)
	}
	if out, _ := exec_(g.origin, "show-ref", "refs/heads/topic"); out != "" {
		t.Fatal("origin still has topic")
	}
}

func TestDiffWorkTreeTagDeleteWorktree(t *testing.T) {
	g := newGitRig(t)
	g.routeMore()
	os.WriteFile(filepath.Join(g.work, "a.txt"), []byte("changed\n"), 0o644)
	run(t, g.work, "mv", "a.txt", "renamed.txt")
	os.WriteFile(filepath.Join(g.work, "renamed.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	code, body := g.call("GET", "/diff-worktree?ref=HEAD", nil)
	files := data(body)["files"].([]any)
	if code != 200 || len(files) != 1 {
		t.Fatalf("diff-worktree: %d %v", code, body)
	}
	f := files[0].(map[string]any)
	if f["status"] != "R" || f["oldPath"] != "a.txt" || f["path"] != "renamed.txt" {
		t.Fatalf("rename entry: %v", f)
	}

	run(t, g.work, "tag", "v1")
	if code, _ := g.call("POST", "/tags/delete", gin.H{"name": "v1"}); code != 200 {
		t.Fatal("delete tag")
	}
	if code, _ := g.call("POST", "/tags/delete", gin.H{"name": "-x"}); code != 400 {
		t.Fatal("option-looking tag must 400")
	}

	run(t, g.work, "branch", "wt-branch")
	wt := filepath.Join(t.TempDir(), "wt")
	if code, body := g.call("POST", "/worktrees", gin.H{"path": wt, "ref": "wt-branch"}); code != 200 {
		t.Fatalf("worktree: %d %v", code, body)
	}
	if run(t, wt, "rev-parse", "--abbrev-ref", "HEAD") != "wt-branch" {
		t.Fatal("worktree not on wt-branch")
	}
	wt2 := filepath.Join(t.TempDir(), "wt2")
	if code, _ := g.call("POST", "/worktrees", gin.H{"path": wt2, "ref": "origin/main", "newBranch": "from-remote"}); code != 200 {
		t.Fatal("worktree with new branch")
	}
	if code, _ := g.call("POST", "/worktrees", gin.H{"path": wt, "ref": "main"}); code != http.StatusConflict {
		t.Fatal("non-empty target must 409")
	}
	if code, _ := g.call("POST", "/worktrees", gin.H{"path": "rel/dir", "ref": "main"}); code != 400 {
		t.Fatal("relative path must 400")
	}
}

func exec_(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func TestDeleteRemoteBranchIsSensitive(t *testing.T) {
	if !middleware.IsSensitiveRoute("POST", "/api/git/repos/abc/remote-branches/delete") {
		t.Fatal("deleting a remote branch must need step-up")
	}
}
