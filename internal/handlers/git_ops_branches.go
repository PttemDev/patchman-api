package handlers

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// JetBrains-style branch / remote / update surface for a managed clone
// (GIT_IDE_PLAN.md §B, §E). Embedded-only like the rest of git_ops.go.
//
// Clones are blobless, so checkout / merge / rebase may lazily fetch blobs
// from origin: every op that can touch history runs with the connection's
// token (authEnv), which the askpass helper only ever answers for origin's
// host.

// ── shapes ──────────────────────────────────────────────────────────

type gitOpState struct {
	// "" | "merge" | "rebase" | "cherry-pick" | "revert"
	Operation  string   `json:"operation"`
	Branch     string   `json:"branch"`
	Detached   bool     `json:"detached"`
	Head       string   `json:"head"`
	Conflicted []string `json:"conflicted"`
}

type gitRefBranch struct {
	Name      string `json:"name"`
	CommitSha string `json:"commitSha"`
	Current   bool   `json:"current,omitempty"`
	IsDefault bool   `json:"isDefault,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Gone      bool   `json:"gone,omitempty"`
	Ahead     int    `json:"ahead,omitempty"`
	Behind    int    `json:"behind,omitempty"`
	Remote    string `json:"remote,omitempty"` // remote branches: the remote's name
	Subject   string `json:"subject,omitempty"`
	Date      string `json:"date,omitempty"`
}

type gitRefsDTO struct {
	Current string         `json:"current"`
	Local   []gitRefBranch `json:"local"`
	Remote  []gitRefBranch `json:"remote"`
	Tags    []gitRefBranch `json:"tags"`
	Recent  []string       `json:"recent"`
	State   gitOpState     `json:"state"`
}

type gitRemoteDTO struct {
	Name     string `json:"name"`
	FetchURL string `json:"fetchUrl"`
	PushURL  string `json:"pushUrl"`
	// Origin's host is the only one the connection token is answered for.
	Authenticated bool `json:"authenticated"`
}

// ── validation ──────────────────────────────────────────────────────

var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validRemoteName(s string) bool { return remoteNamePattern.MatchString(s) }

// validBranchName asks git itself (check-ref-format --branch) on top of the
// option-injection guard.
func (h *GitOpsHandler) validBranchName(ctx context.Context, name string) bool {
	if !safeArg(name) || strings.ContainsAny(name, " \t\n") {
		return false
	}
	_, _, err := h.runGit(ctx, nil, "check-ref-format", "--branch", name)
	return err == nil
}

// validRemoteURL accepts https / http / ssh URLs and scp-style git@host:path.
// Never credentials in the URL (they would land in .git/config) and never a
// transport that runs commands (ext::, file://, fd::).
func validRemoteURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-") || strings.Contains(raw, "::") || strings.ContainsAny(raw, " \t\n") {
		return false
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "https", "http":
			return u.Host != "" && u.User == nil
		case "ssh":
			_, hasPw := u.User.Password()
			return u.Host != "" && !hasPw
		}
		return false
	}
	// scp-like: user@host:path (no scheme).
	at, colon := strings.Index(raw, "@"), strings.Index(raw, ":")
	return colon > 0 && (at < 0 || at < colon) && !strings.Contains(raw[:colon], "/")
}

// redactURL drops any userinfo a hand-edited .git/config may carry.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil && u.Scheme != "" {
		u.User = nil
		return u.String()
	}
	return raw
}

// ── shared helpers ──────────────────────────────────────────────────

// withAuth binds the optional accessToken, prepares the token env (origin's
// host only) and returns a cleanup. ok=false means the response is written.
func (h *GitOpsHandler) withAuth(c *gin.Context, repo *models.GitRepo, supplied string) ([]string, func(), bool) {
	token := h.opsToken(c.GetString("userID"), repo.ConnectionID, supplied)
	if token == "" || !tokenAllowed(repo) {
		// No token: still usable for refs/blobs already on disk.
		return nil, func() {}, true
	}
	env, cleanup, err := authEnvAs(h.gitUser(repo.ConnectionID), token, repo.RemoteURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare auth: " + err.Error()})
		return nil, nil, false
	}
	return env, cleanup, true
}

// opState reads the in-progress operation from the git dir: rebase keeps
// rebase-merge/ or rebase-apply/ directories, the others a *_HEAD file.
func (h *GitOpsHandler) opState(ctx context.Context, dir string) gitOpState {
	st := gitOpState{Conflicted: []string{}}
	if out, _, err := h.runGit(ctx, nil, "-C", dir, "rev-parse", "--absolute-git-dir"); err == nil {
		gd := strings.TrimSpace(out)
		exists := func(n string) bool { _, err := os.Stat(filepath.Join(gd, n)); return err == nil }
		switch {
		case exists("rebase-merge") || exists("rebase-apply"):
			st.Operation = "rebase"
		case exists("MERGE_HEAD"):
			st.Operation = "merge"
		case exists("CHERRY_PICK_HEAD"):
			st.Operation = "cherry-pick"
		case exists("REVERT_HEAD"):
			st.Operation = "revert"
		}
	}
	if out, _, err := h.runGit(ctx, nil, "-C", dir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		st.Branch = strings.TrimSpace(out)
	} else {
		st.Detached = true
	}
	if out, _, err := h.runGit(ctx, nil, "-C", dir, "rev-parse", "--short", "HEAD"); err == nil {
		st.Head = strings.TrimSpace(out)
	}
	if out, _, err := h.runGit(ctx, nil, "-C", dir, "-c", "core.quotepath=false", "diff", "--name-only", "--diff-filter=U"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				st.Conflicted = append(st.Conflicted, l)
			}
		}
	}
	return st
}

// finishHistoryOp answers a merge / rebase / pull-like op: success, stopped
// on conflicts (200 + state, the FE opens the resolver), or a real failure.
func (h *GitOpsHandler) finishHistoryOp(c *gin.Context, repo *models.GitRepo, op string, detail gin.H, stderr string, err error) {
	userID := c.GetString("userID")
	st := h.opState(c.Request.Context(), repo.LocalPath)
	if err != nil {
		if st.Operation != "" && len(st.Conflicted) > 0 {
			detail["conflicts"] = len(st.Conflicted)
			auditGit(h.db, userID, repo.ConnectionID, op, true, detail)
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "conflicts": true, "state": st}})
			return
		}
		auditGit(h.db, userID, repo.ConnectionID, op, false, detail)
		c.JSON(http.StatusConflict, gin.H{"error": "git " + op + ": " + gitErrMsg(stderr, err), "state": st})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, op, true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "state": st}})
}

// ── State / Refs ────────────────────────────────────────────────────

// State — GET /git/repos/:repoId/state. Branch, HEAD, in-progress operation.
func (h *GitOpsHandler) State(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.opState(c.Request.Context(), repo.LocalPath)})
}

// Refs — GET /git/repos/:repoId/refs. Local + remote branches, tags and the
// recently checked-out branches (reflog), for the branch popup.
func (h *GitOpsHandler) Refs(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	const f = "--format=%(refname)%1f%(objectname:short)%1f%(HEAD)%1f%(upstream:short)%1f%(upstream:track,nobracket)%1f%(contents:subject)%1f%(committerdate:iso-strict)"
	out, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "for-each-ref", "--sort=-committerdate", f,
		"refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git for-each-ref: " + gitErrMsg(stderr, err)})
		return
	}
	dto := gitRefsDTO{Local: []gitRefBranch{}, Remote: []gitRefBranch{}, Tags: []gitRefBranch{}, Recent: []string{}}
	for _, line := range strings.Split(out, "\n") {
		p := strings.Split(line, "\x1f")
		if len(p) < 7 {
			continue
		}
		b := gitRefBranch{CommitSha: p[1], Subject: p[5], Date: p[6]}
		switch {
		case strings.HasPrefix(p[0], "refs/heads/"):
			b.Name = strings.TrimPrefix(p[0], "refs/heads/")
			b.Current = strings.TrimSpace(p[2]) == "*"
			b.IsDefault = b.Name == repo.DefaultBranch
			b.Upstream = p[3]
			b.Gone = strings.Contains(p[4], "gone")
			b.Ahead, b.Behind = parseTrack(p[4])
			if b.Current {
				dto.Current = b.Name
			}
			dto.Local = append(dto.Local, b)
		case strings.HasPrefix(p[0], "refs/remotes/"):
			b.Name = strings.TrimPrefix(p[0], "refs/remotes/")
			if strings.HasSuffix(b.Name, "/HEAD") {
				continue
			}
			b.Remote, _, _ = strings.Cut(b.Name, "/")
			dto.Remote = append(dto.Remote, b)
		case strings.HasPrefix(p[0], "refs/tags/"):
			b.Name = strings.TrimPrefix(p[0], "refs/tags/")
			dto.Tags = append(dto.Tags, b)
		}
	}
	// Recent: "checkout: moving from A to B" entries, newest first, unique,
	// existing local branches only, current excluded.
	if rl, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "reflog", "show", "--format=%gs", "-n", "300", "HEAD", "--"); err == nil {
		local := map[string]bool{}
		for _, b := range dto.Local {
			local[b.Name] = true
		}
		seen := map[string]bool{dto.Current: true}
		for _, l := range strings.Split(rl, "\n") {
			_, rest, ok := strings.Cut(l, "checkout: moving from ")
			if !ok {
				continue
			}
			from, to, _ := strings.Cut(rest, " to ")
			for _, n := range []string{to, from} {
				if local[n] && !seen[n] {
					seen[n] = true
					dto.Recent = append(dto.Recent, n)
				}
			}
			if len(dto.Recent) >= 8 {
				break
			}
		}
	}
	dto.State = h.opState(ctx, repo.LocalPath)
	c.JSON(http.StatusOK, gin.H{"data": dto})
}

// ── Fetch ───────────────────────────────────────────────────────────

type gitFetchInput struct {
	AccessToken string `json:"accessToken"`
	Remote      string `json:"remote"` // "" = all remotes
}

// Fetch — POST /git/repos/:repoId/fetch. fetch --prune (one remote or all).
func (h *GitOpsHandler) Fetch(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitFetchInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	args := []string{"-C", repo.LocalPath, "fetch", "--prune", "--tags", "--force"}
	if in.Remote == "" {
		args = append(args, "--all")
	} else if validRemoteName(in.Remote) {
		args = append(args, in.Remote)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote"})
		return
	}
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	userID := c.GetString("userID")
	if _, stderr, err := h.runGit(c.Request.Context(), env, args...); err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "fetch", false, gin.H{"repoId": repo.ID, "remote": in.Remote})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git fetch: " + gitErrMsg(stderr, err)})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "fetch", true, gin.H{"repoId": repo.ID, "remote": in.Remote})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ── Checkout ────────────────────────────────────────────────────────

type gitCheckoutInput struct {
	AccessToken string `json:"accessToken"`
	// Ref: a local branch, a remote branch (origin/x → local x tracking it),
	// a tag or a commit (detached HEAD).
	Ref string `json:"ref"`
	// NewBranch: create this branch at Ref and switch to it.
	NewBranch string `json:"newBranch"`
	// Smart: stash local changes, switch, re-apply them (JetBrains "Smart
	// Checkout"). Without it a dirty tree that conflicts makes git refuse.
	Smart bool `json:"smart"`
}

// Checkout — POST /git/repos/:repoId/checkout.
func (h *GitOpsHandler) Checkout(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitCheckoutInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	in.Ref, in.NewBranch = strings.TrimSpace(in.Ref), strings.TrimSpace(in.NewBranch)
	if !safeArg(in.Ref) || (in.NewBranch != "" && !h.validBranchName(ctx, in.NewBranch)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref or branch name"})
		return
	}
	dir := repo.LocalPath
	exists := func(ref string) bool {
		_, _, err := h.runGit(ctx, nil, "-C", dir, "show-ref", "--verify", "--quiet", ref)
		return err == nil
	}
	var args []string
	switch {
	case in.NewBranch != "":
		args = []string{"switch", "-c", in.NewBranch, in.Ref}
	case exists("refs/heads/" + in.Ref):
		args = []string{"switch", in.Ref}
	case exists("refs/remotes/" + in.Ref):
		_, local, _ := strings.Cut(in.Ref, "/")
		if exists("refs/heads/" + local) {
			// JetBrains offers "checkout & reset"; we switch to the existing
			// local branch and let the user update it.
			args = []string{"switch", local}
		} else {
			args = []string{"switch", "--track", "-c", local, in.Ref}
		}
	default:
		args = []string{"switch", "--detach", in.Ref}
	}
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()

	stashed := false
	if in.Smart {
		if out, _, err := h.runGit(ctx, nil, "-C", dir, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
			if _, stderr, err := h.runGit(ctx, env, "-C", dir, "stash", "push", "--include-untracked", "-m", "PatchMan smart checkout"); err != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "git stash: " + gitErrMsg(stderr, err)})
				return
			}
			stashed = true
		}
	}
	_, stderr, err := h.runGit(ctx, env, append([]string{"-C", dir}, args...)...)
	detail := gin.H{"repoId": repo.ID, "ref": in.Ref, "newBranch": in.NewBranch, "smart": in.Smart}
	if err != nil {
		if stashed {
			_, _, _ = h.runGit(ctx, env, "-C", dir, "stash", "pop", "--index")
		}
		auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "checkout", false, detail)
		// 409 + dirty hint: the FE offers Smart Checkout.
		c.JSON(http.StatusConflict, gin.H{"error": "git switch: " + gitErrMsg(stderr, err),
			"localChanges": strings.Contains(stderr, "would be overwritten")})
		return
	}
	if stashed {
		if _, pst, perr := h.runGit(ctx, env, "-C", dir, "stash", "pop"); perr != nil {
			// The switch happened; re-applying conflicted. The stash is kept
			// and the conflicts are in the tree for the resolver.
			detail["stashPopConflict"] = true
			auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "checkout", true, detail)
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "stashConflict": true, "message": gitErrMsg(pst, perr),
				"state": h.opState(ctx, dir)}})
			return
		}
	}
	auditGit(h.db, c.GetString("userID"), repo.ConnectionID, "checkout", true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "state": h.opState(ctx, dir)}})
}

// ── Branch create / rename / delete ─────────────────────────────────

type gitBranchInput struct {
	Name       string `json:"name"`
	StartPoint string `json:"startPoint"`
	NewName    string `json:"newName"`
}

// CreateBranch — POST /git/repos/:repoId/branches {name, startPoint}.
// (Create-and-checkout goes through Checkout{newBranch}.)
func (h *GitOpsHandler) CreateBranch(c *gin.Context) {
	h.branchOp(c, "branch_create")
}

// RenameBranch — POST /git/repos/:repoId/branches/rename {name, newName}.
func (h *GitOpsHandler) RenameBranch(c *gin.Context) { h.branchOp(c, "branch_rename") }

// DeleteBranch — POST /git/repos/:repoId/branches/delete {name}. Safe delete:
// git refuses a branch that is not merged.
func (h *GitOpsHandler) DeleteBranch(c *gin.Context) { h.branchOp(c, "branch_delete") }

// ForceDeleteBranch — POST /git/repos/:repoId/branches/force-delete {name}.
// Drops unmerged commits; a sensitive route (step-up).
func (h *GitOpsHandler) ForceDeleteBranch(c *gin.Context) { h.branchOp(c, "branch_force_delete") }

func (h *GitOpsHandler) branchOp(c *gin.Context, op string) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitBranchInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	in.Name, in.NewName, in.StartPoint = strings.TrimSpace(in.Name), strings.TrimSpace(in.NewName), strings.TrimSpace(in.StartPoint)
	if !h.validBranchName(ctx, in.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch name"})
		return
	}
	args := []string{"-C", repo.LocalPath, "branch"}
	switch op {
	case "branch_create":
		if in.StartPoint == "" {
			in.StartPoint = "HEAD"
		}
		if !safeArg(in.StartPoint) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start point"})
			return
		}
		args = append(args, "--", in.Name, in.StartPoint)
	case "branch_rename":
		if !h.validBranchName(ctx, in.NewName) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid new branch name"})
			return
		}
		args = append(args, "-m", "--", in.Name, in.NewName)
	case "branch_delete":
		args = append(args, "-d", "--", in.Name)
	case "branch_force_delete":
		args = append(args, "-D", "--", in.Name)
	}
	userID := c.GetString("userID")
	detail := gin.H{"repoId": repo.ID, "name": in.Name, "newName": in.NewName}
	if _, stderr, err := h.runGit(ctx, nil, args...); err != nil {
		auditGit(h.db, userID, repo.ConnectionID, op, false, detail)
		c.JSON(http.StatusConflict, gin.H{"error": "git branch: " + gitErrMsg(stderr, err),
			"notMerged": strings.Contains(stderr, "not fully merged")})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, op, true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ── Merge / Rebase / continue-abort ─────────────────────────────────

type gitMergeInput struct {
	AccessToken string `json:"accessToken"`
	Ref         string `json:"ref"`
	// Mode: "" (ff when possible) | no-ff | ff-only | squash
	Mode string `json:"mode"`
}

// Merge — POST /git/repos/:repoId/merge {ref, mode}: merge ref into current.
func (h *GitOpsHandler) Merge(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitMergeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !safeArg(strings.TrimSpace(in.Ref)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
		return
	}
	args := []string{"-C", repo.LocalPath, "merge", "--no-edit"}
	switch in.Mode {
	case "":
	case "no-ff", "ff-only", "squash":
		args = append(args, "--"+in.Mode)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be no-ff, ff-only or squash"})
		return
	}
	args = append(args, strings.TrimSpace(in.Ref))
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	_, stderr, err := h.runGit(c.Request.Context(), env, args...)
	h.finishHistoryOp(c, repo, "merge", gin.H{"repoId": repo.ID, "ref": in.Ref, "mode": in.Mode}, stderr, err)
}

type gitRebaseInput struct {
	AccessToken string `json:"accessToken"`
	Onto        string `json:"onto"`
	// Branch (optional): check this branch out first, then rebase it onto
	// Onto — JetBrains "Checkout and Rebase onto <current>".
	Branch string `json:"branch"`
}

// Rebase — POST /git/repos/:repoId/rebase {onto}: rebase current onto ref.
func (h *GitOpsHandler) Rebase(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitRebaseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !safeArg(strings.TrimSpace(in.Onto)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
		return
	}
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	args := []string{"-C", repo.LocalPath, "rebase", strings.TrimSpace(in.Onto)}
	if b := strings.TrimSpace(in.Branch); b != "" {
		if !h.validBranchName(c.Request.Context(), b) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid branch"})
			return
		}
		args = append(args, b)
	}
	_, stderr, err := h.runGit(c.Request.Context(), env, args...)
	h.finishHistoryOp(c, repo, "rebase", gin.H{"repoId": repo.ID, "onto": in.Onto, "branch": in.Branch}, stderr, err)
}

type gitOperationInput struct {
	AccessToken string `json:"accessToken"`
	Action      string `json:"action"` // abort | continue | skip
}

// Operation — POST /git/repos/:repoId/operation {action}: abort / continue /
// skip whatever merge, rebase, cherry-pick or revert is in progress.
func (h *GitOpsHandler) Operation(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitOperationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	st := h.opState(ctx, repo.LocalPath)
	if st.Operation == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no merge, rebase, cherry-pick or revert in progress"})
		return
	}
	switch in.Action {
	case "abort", "continue":
	case "skip":
		if st.Operation == "merge" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a merge cannot be skipped"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be abort, continue or skip"})
		return
	}
	if in.Action == "continue" && len(st.Conflicted) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "resolve all conflicts first", "state": st})
		return
	}
	env, cleanup, ok := h.withAuth(c, repo, in.AccessToken)
	if !ok {
		return
	}
	defer cleanup()
	_, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, st.Operation, "--"+in.Action)
	h.finishHistoryOp(c, repo, st.Operation+"_"+in.Action, gin.H{"repoId": repo.ID}, stderr, err)
}

// ── Remotes ─────────────────────────────────────────────────────────

// Remotes — GET /git/repos/:repoId/remotes.
func (h *GitOpsHandler) Remotes(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	out, stderr, err := h.runGit(c.Request.Context(), nil, "-C", repo.LocalPath, "remote", "-v")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git remote: " + gitErrMsg(stderr, err)})
		return
	}
	byName := map[string]*gitRemoteDTO{}
	order := []string{}
	allowed := tokenHost(repo.RemoteURL)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		r := byName[f[0]]
		if r == nil {
			r = &gitRemoteDTO{Name: f[0]}
			byName[f[0]] = r
			order = append(order, f[0])
		}
		u := redactURL(f[1])
		if f[2] == "(push)" {
			r.PushURL = u
		} else {
			r.FetchURL = u
			r.Authenticated = allowed != "" && tokenHost(f[1]) == allowed
		}
	}
	list := make([]gitRemoteDTO, 0, len(order))
	for _, n := range order {
		list = append(list, *byName[n])
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

type gitRemoteInput struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	NewName string `json:"newName"`
}

// AddRemote — POST /git/repos/:repoId/remotes {name, url}.
func (h *GitOpsHandler) AddRemote(c *gin.Context) { h.remoteOp(c, "remote_add") }

// UpdateRemote — POST /git/repos/:repoId/remotes/update {name, url?, newName?}.
func (h *GitOpsHandler) UpdateRemote(c *gin.Context) { h.remoteOp(c, "remote_update") }

// DeleteRemote — POST /git/repos/:repoId/remotes/delete {name}. origin stays:
// pull / push / the MR flow are bound to it.
func (h *GitOpsHandler) DeleteRemote(c *gin.Context) { h.remoteOp(c, "remote_delete") }

func (h *GitOpsHandler) remoteOp(c *gin.Context, op string) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitRemoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.Name, in.NewName, in.URL = strings.TrimSpace(in.Name), strings.TrimSpace(in.NewName), strings.TrimSpace(in.URL)
	if !validRemoteName(in.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote name"})
		return
	}
	if in.URL != "" && !validRemoteURL(in.URL) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "remote URL must be https or ssh, without credentials"})
		return
	}
	ctx := c.Request.Context()
	dir := repo.LocalPath
	var cmds [][]string
	switch op {
	case "remote_add":
		if in.URL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
			return
		}
		cmds = append(cmds, []string{"remote", "add", "--", in.Name, in.URL})
	case "remote_update":
		if in.NewName != "" && in.NewName != in.Name {
			if in.Name == "origin" || !validRemoteName(in.NewName) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "origin cannot be renamed / invalid new name"})
				return
			}
		}
		if in.URL != "" {
			cmds = append(cmds, []string{"remote", "set-url", "--", in.Name, in.URL})
		}
		if in.NewName != "" && in.NewName != in.Name {
			cmds = append(cmds, []string{"remote", "rename", "--", in.Name, in.NewName})
		}
	case "remote_delete":
		if in.Name == "origin" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "origin cannot be removed"})
			return
		}
		cmds = append(cmds, []string{"remote", "remove", "--", in.Name})
	}
	userID := c.GetString("userID")
	detail := gin.H{"repoId": repo.ID, "name": in.Name, "newName": in.NewName, "url": redactURL(in.URL)}
	for _, a := range cmds {
		if _, stderr, err := h.runGit(ctx, nil, append([]string{"-C", dir}, a...)...); err != nil {
			auditGit(h.db, userID, repo.ConnectionID, op, false, detail)
			c.JSON(http.StatusConflict, gin.H{"error": "git remote: " + gitErrMsg(stderr, err)})
			return
		}
	}
	auditGit(h.db, userID, repo.ConnectionID, op, true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
