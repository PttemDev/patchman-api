package handlers

// The rest of the JetBrains branch popup (GIT_IDE_PLAN P1 follow-up):
// Update / Push a branch that is NOT checked out, tracked branch, Compare,
// Show Diff with Working Tree, delete a remote branch or a tag, and New
// Worktree. Checkout-and-Rebase rides Rebase's optional `branch`.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// upstreamOf returns a local branch's remote and the remote branch name it
// tracks (branch.<b>.remote / branch.<b>.merge), or "" when it tracks none.
func (h *GitOpsHandler) upstreamOf(c *gin.Context, repo *models.GitRepo, branch string) (remote, remoteBranch string) {
	ctx := c.Request.Context()
	r, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return "", ""
	}
	m, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(r), strings.TrimPrefix(strings.TrimSpace(m), "refs/heads/")
}

func (h *GitOpsHandler) currentBranch(c *gin.Context, repo *models.GitRepo) string {
	out, _, err := h.runGit(c.Request.Context(), nil, "-C", repo.LocalPath, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ── Update a branch that is not checked out ─────────────────────────

type gitBranchUpdateInput struct {
	AccessToken string `json:"accessToken"`
	Name        string `json:"name"`
}

// UpdateBranch — POST /git/repos/:repoId/branches/update {name}. Fast-forwards
// a local branch that is NOT checked out to its upstream, without touching the
// working tree: `git fetch <remote> <branch>:refs/heads/<name>` (git refuses a
// non-fast-forward without '+', and the checked-out branch). A diverged
// branch answers 409 diverged=true.
func (h *GitOpsHandler) UpdateBranch(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitBranchUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	name := strings.TrimSpace(in.Name)
	if !h.validBranchName(ctx, name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch"})
		return
	}
	if name == h.currentBranch(c, repo) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this branch is checked out — use Update Project", "current": true})
		return
	}
	remote, rb := h.upstreamOf(c, repo, name)
	if remote == "" || rb == "" || !validRemoteName(remote) || !safeArg(rb) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "'" + name + "' tracks no remote branch — set a tracked branch first", "noUpstream": true})
		return
	}
	before, _, _ := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "refs/heads/"+name)
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	userID := c.GetString("userID")
	detail := gin.H{"repoId": repo.ID, "branch": name, "upstream": remote + "/" + rb}
	_, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, "fetch", remote, "refs/heads/"+rb+":refs/heads/"+name)
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "branch_update", false, detail)
		diverged := strings.Contains(stderr, "non-fast-forward") || strings.Contains(stderr, "rejected")
		c.JSON(http.StatusConflict, gin.H{"error": "git fetch: " + gitErrMsg(stderr, err), "diverged": diverged})
		return
	}
	after, _, _ := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "refs/heads/"+name)
	auditGit(h.db, userID, repo.ConnectionID, "branch_update", true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "updated": strings.TrimSpace(before) != strings.TrimSpace(after)}})
}

// ── Tracked branch ──────────────────────────────────────────────────

type gitUpstreamInput struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"` // "origin/x"; "" = stop tracking
}

// SetUpstream — POST /git/repos/:repoId/branches/upstream {name, upstream}.
func (h *GitOpsHandler) SetUpstream(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitUpstreamInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	name, up := strings.TrimSpace(in.Name), strings.TrimSpace(in.Upstream)
	if !h.validBranchName(ctx, name) || (up != "" && !safeArg(up)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch"})
		return
	}
	args := []string{"-C", repo.LocalPath, "branch", "--unset-upstream", name}
	if up != "" {
		if _, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "show-ref", "--verify", "--quiet", "refs/remotes/"+up); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no remote branch " + up})
			return
		}
		args = []string{"-C", repo.LocalPath, "branch", "--set-upstream-to=" + up, name}
	}
	_, stderr, err := h.runGit(ctx, nil, args...)
	auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "branch_upstream", err == nil, gin.H{"repoId": repo.ID, "branch": name, "upstream": up})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "git branch: " + gitErrMsg(stderr, err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ── Compare ─────────────────────────────────────────────────────────

// Compare — GET /git/repos/:repoId/compare?base=&ref=. JetBrains "Compare
// with": the commits <ref> has that <base> lacks, and the other way round.
func (h *GitOpsHandler) Compare(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	base, ref := strings.TrimSpace(c.Query("base")), strings.TrimSpace(c.Query("ref"))
	if base == "" {
		base = "HEAD"
	}
	if !safeArg(base) || !safeArg(ref) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
		return
	}
	ctx := c.Request.Context()
	side := func(from, to string) ([]gitRemoteCommit, bool) {
		out, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "log", commitLogFmt, "-n", "500", from+".."+to, "--")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "git log: " + gitErrMsg(stderr, err)})
			return nil, false
		}
		return parseCommitLog(out), true
	}
	only, ok := side(base, ref)
	if !ok {
		return
	}
	missing, ok := side(ref, base)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"base": base, "ref": ref, "refOnly": only, "baseOnly": missing}})
}

// ── Show Diff with Working Tree ─────────────────────────────────────

type gitNameStatus struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Status  string `json:"status"` // A M D R …
}

// DiffWorkTree — GET /git/repos/:repoId/diff-worktree?ref=. The files that
// differ between <ref> and the working tree (tracked files), renames found.
func (h *GitOpsHandler) DiffWorkTree(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	ref := strings.TrimSpace(c.Query("ref"))
	if !safeArg(ref) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
		return
	}
	out, stderr, err := h.runGit(c.Request.Context(), nil, "-C", repo.LocalPath, "-c", "core.quotepath=false", "diff", "--name-status", "-z", "-M", ref, "--")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git diff: " + gitErrMsg(stderr, err)})
		return
	}
	files := make([]gitNameStatus, 0)
	parts := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i < len(parts); i++ {
		st := parts[i]
		if st == "" {
			continue
		}
		if (st[0] == 'R' || st[0] == 'C') && i+2 < len(parts) {
			files = append(files, gitNameStatus{Status: st[:1], OldPath: parts[i+1], Path: parts[i+2]})
			i += 2
			continue
		}
		if i+1 < len(parts) {
			files = append(files, gitNameStatus{Status: st[:1], Path: parts[i+1]})
			i++
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ref": ref, "files": files}})
}

// ── Delete a remote branch / a tag ──────────────────────────────────

type gitRemoteBranchInput struct {
	AccessToken string `json:"accessToken"`
	Remote      string `json:"remote"`
	Branch      string `json:"branch"`
}

// DeleteRemoteBranch — POST /git/repos/:repoId/remote-branches/delete
// {remote, branch}: `git push <remote> --delete <branch>`. Sensitive route
// (step-up): it removes the branch for everyone.
func (h *GitOpsHandler) DeleteRemoteBranch(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitRemoteBranchInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	remote, branch := strings.TrimSpace(in.Remote), strings.TrimSpace(in.Branch)
	if !validRemoteName(remote) || !h.validBranchName(ctx, branch) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote or branch"})
		return
	}
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	_, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, "push", remote, "--delete", branch)
	auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "remote_branch_delete", err == nil, gin.H{"repoId": repo.ID, "remote": remote, "branch": branch})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "git push --delete: " + gitErrMsg(stderr, err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// DeleteTag — POST /git/repos/:repoId/tags/delete {name}: local tag only.
func (h *GitOpsHandler) DeleteTag(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	name := strings.TrimSpace(in.Name)
	if !safeArg(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tag"})
		return
	}
	if _, _, err := h.runGit(ctx, nil, "check-ref-format", "refs/tags/"+name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tag"})
		return
	}
	_, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "tag", "-d", name)
	auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "tag_delete", err == nil, gin.H{"repoId": repo.ID, "tag": name})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "git tag -d: " + gitErrMsg(stderr, err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ── New Worktree ────────────────────────────────────────────────────

type gitWorktreeInput struct {
	Path string `json:"path"` // absolute, must not exist (or be an empty dir)
	// Ref to check out. With NewBranch set, the new branch starts here.
	Ref       string `json:"ref"`
	NewBranch string `json:"newBranch"`
}

// AddWorktree — POST /git/repos/:repoId/worktrees {path, ref, newBranch?}.
// `git worktree add [-b <new>] <path> <ref>`. The FE then opens the new
// folder like any other (Open folder).
func (h *GitOpsHandler) AddWorktree(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitWorktreeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	path := filepath.Clean(strings.TrimSpace(in.Path))
	ref, nb := strings.TrimSpace(in.Ref), strings.TrimSpace(in.NewBranch)
	if !filepath.IsAbs(path) || !safeArg(ref) || (nb != "" && !h.validBranchName(ctx, nb)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path, ref or branch name"})
		return
	}
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "folder already exists and is not empty: " + path})
		return
	}
	args := []string{"-C", repo.LocalPath, "worktree", "add"}
	if nb != "" {
		args = append(args, "-b", nb)
	}
	args = append(args, "--", path, ref)
	_, stderr, err := h.runGit(ctx, nil, args...)
	auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "worktree_add", err == nil, gin.H{"repoId": repo.ID, "ref": ref, "newBranch": nb})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "git worktree add: " + gitErrMsg(stderr, err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "path": path}})
}
