package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GitOpsHandler serves the local working-tree surface for the Git tool —
// clone / status / log / branches / diff / stage / unstage / commit / push /
// local-file. These run git against a managed clone on the user's own machine,
// so they are EMBEDDED-ONLY (desktop app); on a hosted/shared BE they must
// never touch a filesystem.
//
// Wave 2 (BE-B) — real git-CLI implementations. Auth for clone/pull/push never
// puts the token at rest: a STATIC GIT_ASKPASS helper reads the token from the
// process environment (PATCHMAN_GIT_TOKEN), so the secret lives in memory/env
// only and never lands in a file, the remote URL, or .git/config. See
// GIT_V1_CONTRACT.md §2c/§4.
type GitOpsHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	embedded bool
	gitBin   string // resolved once via exec.LookPath; "" => git absent
}

// NewGitOpsHandler wires the local-ops handler. embedded is cfg.IsEmbedded():
// when false, every method short-circuits with a 403 (desktop-only). git is
// detected once here; when absent, every method degrades to 503.
func NewGitOpsHandler(db *gorm.DB, cipher *services.TokenCipher, embedded bool) *GitOpsHandler {
	bin, _ := exec.LookPath("git")
	return &GitOpsHandler{db: db, cipher: cipher, embedded: embedded, gitBin: bin}
}

// ── DTOs (local-ops wire shapes — see GIT_V1_CONTRACT.md §3) ─────────
//
// GitCommit ([]) reuses the package's gitRemoteCommit (identical JSON shape).
// GitBranch needs current/ahead/behind that the remote gitBranch lacks, so it
// gets a NEW local type.

type gitLocalBranch struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	Current   bool   `json:"current"`
	CommitSha string `json:"commitSha"`
	Ahead     int    `json:"ahead,omitempty"`
	Behind    int    `json:"behind,omitempty"`
}

type gitStatusEntry struct {
	Path    string `json:"path"`
	X       string `json:"x"`
	Y       string `json:"y"`
	Status  string `json:"status"`
	OldPath string `json:"oldPath,omitempty"`
}

type gitStatusDTO struct {
	Branch     string           `json:"branch"`
	Upstream   string           `json:"upstream,omitempty"`
	Ahead      int              `json:"ahead"`
	Behind     int              `json:"behind"`
	Staged     []gitStatusEntry `json:"staged"`
	Unstaged   []gitStatusEntry `json:"unstaged"`
	Untracked  []string         `json:"untracked"`
	Conflicted []string         `json:"conflicted"`
	Clean      bool             `json:"clean"`
}

// ── request bodies ──────────────────────────────────────────────────

type gitCloneInput struct {
	RepoID        string `json:"repoId"`
	ProjectRef    string `json:"projectRef"`
	ProjectName   string `json:"projectName"`
	RemoteURL     string `json:"remoteUrl"`
	DefaultBranch string `json:"defaultBranch"`
	AccessToken   string `json:"accessToken"`
}

type gitStageInput struct {
	Path  string `json:"path"`
	Patch string `json:"patch"`
}

type gitCommitInput struct {
	Message string `json:"message"`
	Amend   bool   `json:"amend"`
}

type gitPushInput struct {
	AccessToken   string `json:"accessToken"`
	Mode          string `json:"mode"` // 'branch-mr' (default) | 'direct'
	TargetBranch  string `json:"targetBranch"`
	MRTitle       string `json:"mrTitle"`
	MRDescription string `json:"mrDescription"`
}

type gitPullInput struct {
	AccessToken string `json:"accessToken"`
}

// repoIDPattern guards filesystem paths derived from the FE-minted repo id
// (used as a managed-clone subdir name). It is a UUID in practice; rejecting
// anything else blocks path traversal ("../..") through the clone dir.
var repoIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ── gates / resolution ──────────────────────────────────────────────

// gitGate enforces the two preconditions every local op shares: embedded mode
// (desktop-only, 403) and git being present (503). Returns false with the
// response already written on failure.
func (h *GitOpsHandler) gitGate(c *gin.Context) bool {
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "Git working-tree operations are only available on the desktop app"})
		return false
	}
	if h.gitBin == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git not found on this machine"})
		return false
	}
	return true
}

// resolveRepo gates + loads the target GitRepo, owner-scoped to the caller
// (embedded => the local user). Returns false with the response written on any
// failure.
func (h *GitOpsHandler) resolveRepo(c *gin.Context) (*models.GitRepo, bool) {
	if !h.gitGate(c) {
		return nil, false
	}
	userID := c.GetString("userID")
	var repo models.GitRepo
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("repoId"), userID).First(&repo).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return nil, false
	}
	if strings.TrimSpace(repo.LocalPath) == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "Repository has no local clone yet"})
		return nil, false
	}
	return &repo, true
}

// opsToken picks the token for a clone/pull/push. A PAT connection uses its
// own stored token: it is the credential for that GitLab, which may not be
// the one the caller's linked account (the FE-minted token) belongs to.
// Otherwise the FE-supplied token is used as before.
func (h *GitOpsHandler) opsToken(userID, connID, supplied string) string {
	if h.cipher != nil && connID != "" {
		var conn models.GitConnection
		if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err == nil &&
			strings.EqualFold(strings.TrimSpace(conn.AuthKind), "pat") && conn.CredentialEnc != "" {
			if pat, err := h.cipher.Decrypt(conn.CredentialEnc); err == nil && strings.TrimSpace(pat) != "" {
				return strings.TrimSpace(pat)
			}
		}
	}
	return strings.TrimSpace(supplied)
}

// ── git exec helpers ────────────────────────────────────────────────

// runGit runs the resolved git binary with the client's request context so a
// client abort cancels the child. env, when non-nil, replaces the child
// environment (callers build it from os.Environ()).
func (h *GitOpsHandler) runGit(ctx context.Context, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, h.gitBin, args...)
	if env != nil {
		cmd.Env = env
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// gitErrMsg folds git's stderr into a single message; falls back to the exec
// error when stderr is empty.
func gitErrMsg(stderr string, err error) string {
	s := strings.TrimSpace(stderr)
	if s != "" {
		return s
	}
	if err != nil {
		return err.Error()
	}
	return "git command failed"
}

// writeGitAskpass writes the STATIC askpass helper — the token is NEVER written
// into the script; it is read from $PATCHMAN_GIT_TOKEN at call time. The file is
// 0700 because git execs it (a non-executable helper fails with EACCES); 0700
// keeps it owner-only. Returns the path + a cleanup that removes it.
func writeGitAskpass() (string, func(), error) {
	f, err := os.CreateTemp("", "patchman-git-askpass-*.sh")
	if err != nil {
		return "", func() {}, err
	}
	const script = "#!/bin/sh\ncase \"$1\" in\nUsername*) echo oauth2 ;;\n*) echo \"$PATCHMAN_GIT_TOKEN\" ;;\nesac\n"
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	_ = f.Chmod(0o700)
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	path := f.Name()
	return path, func() { _ = os.Remove(path) }, nil
}

// authEnv builds the child environment for token-authenticated git (clone /
// push): the static askpass script + the token via env only + terminal prompt
// disabled. The caller must invoke cleanup to remove the script.
func authEnv(token string) (env []string, cleanup func(), err error) {
	script, cleanup, err := writeGitAskpass()
	if err != nil {
		return nil, func() {}, err
	}
	env = append(os.Environ(),
		"GIT_ASKPASS="+script,
		"PATCHMAN_GIT_TOKEN="+token,
		"GIT_TERMINAL_PROMPT=0",
	)
	return env, cleanup, nil
}

// managedGitBase derives a stable app-data base for managed clones. The frozen
// constructor carries no cfg, so we anchor to the user's config dir (home as a
// fallback): <configDir>/patchman/git-repos, 0700.
func managedGitBase() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", herr
		}
		base = home
	}
	dir := filepath.Join(base, "patchman", "git-repos")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// safeArg rejects option-injection: a user-supplied ref / branch / path that
// begins with '-' would be parsed by git as a flag. Callers still pass '--'
// before path operands; this guards refs used positionally (log <ref>,
// show <ref>:<path>).
func safeArg(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-")
}

// ── Clone ───────────────────────────────────────────────────────────

// Clone — POST /git/connections/:id/clone. Blobless clone into the managed dir
// + upsert git_repos. Idempotent: an existing, healthy clone is reused.
func (h *GitOpsHandler) Clone(c *gin.Context) {
	if !h.gitGate(c) {
		return
	}
	userID := c.GetString("userID")
	connID := c.Param("id")

	// The connection must exist and be owned by the caller (embedded => local
	// user). This ties the new repo to a real connection.
	var conn models.GitConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	var in gitCloneInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.RepoID = strings.TrimSpace(in.RepoID)
	in.RemoteURL = strings.TrimSpace(in.RemoteURL)
	if in.RepoID == "" || in.RemoteURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repoId and remoteUrl are required"})
		return
	}
	if !repoIDPattern.MatchString(in.RepoID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repoId must be a UUID-like token"})
		return
	}

	base, err := managedGitBase()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare clone dir: " + err.Error()})
		return
	}
	localPath := filepath.Join(base, in.RepoID)

	ctx := c.Request.Context()

	// Idempotency: a healthy existing clone answers `rev-parse HEAD`. A partial
	// dir (SIGKILL'd mid-clone still leaves a .git) does NOT, so we wipe and
	// re-clone on anything that isn't a clean HEAD.
	healthy := false
	if _, statErr := os.Stat(localPath); statErr == nil {
		if _, _, rpErr := h.runGit(ctx, nil, "-C", localPath, "rev-parse", "HEAD"); rpErr == nil {
			healthy = true
		}
	}
	if !healthy {
		_ = os.RemoveAll(localPath)
		env, cleanup, aerr := authEnv(h.opsToken(userID, conn.ID, in.AccessToken))
		if aerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare auth: " + aerr.Error()})
			return
		}
		_, stderr, cerr := h.runGit(ctx, env, "clone", "--filter=blob:none", in.RemoteURL, localPath)
		cleanup()
		if cerr != nil {
			_ = os.RemoveAll(localPath)
			auditGit(h.db, userID, connID, "clone", false, gin.H{"repoId": in.RepoID, "error": gitErrMsg(stderr, cerr)})
			c.JSON(http.StatusBadGateway, gin.H{"error": "clone failed: " + gitErrMsg(stderr, cerr)})
			return
		}
	}

	defaultBranch := strings.TrimSpace(in.DefaultBranch)
	if defaultBranch == "" {
		if out, _, berr := h.runGit(ctx, nil, "-C", localPath, "rev-parse", "--abbrev-ref", "HEAD"); berr == nil {
			defaultBranch = strings.TrimSpace(out)
		}
	}

	now := time.Now()
	repo := models.GitRepo{
		ID:            in.RepoID,
		ConnectionID:  connID,
		UserID:        userID,
		ProjectRef:    in.ProjectRef,
		ProjectName:   in.ProjectName,
		RemoteURL:     in.RemoteURL,
		LocalPath:     localPath,
		DefaultBranch: defaultBranch,
		CloneKind:     "blobless",
		LastFetchedAt: &now,
		CreatedAt:     now,
	}
	// Upsert on the FE-minted PK — re-cloning the same repoId refreshes metadata.
	if err := h.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"connection_id", "user_id", "project_ref", "project_name",
			"remote_url", "local_path", "default_branch", "clone_kind", "last_fetched_at",
		}),
	}).Create(&repo).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save repo: " + err.Error()})
		return
	}

	auditGit(h.db, userID, connID, "clone", true, gin.H{"repoId": in.RepoID, "projectRef": in.ProjectRef})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"repo": repo}})
}

// ── ListRepos ───────────────────────────────────────────────────────

// ListRepos — GET /git/repos. Local repos for the caller.
func (h *GitOpsHandler) ListRepos(c *gin.Context) {
	if !h.gitGate(c) {
		return
	}
	userID := c.GetString("userID")
	var repos []models.GitRepo
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&repos).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list repos"})
		return
	}
	if repos == nil {
		repos = []models.GitRepo{}
	}
	auditGit(h.db, userID, "", "repos_local", true, gin.H{"count": len(repos)})
	c.JSON(http.StatusOK, gin.H{"data": repos})
}

// ── Status ──────────────────────────────────────────────────────────

// Status — GET /git/repos/:repoId/status. git status --porcelain=v2 --branch.
func (h *GitOpsHandler) Status(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	// core.quotepath=false keeps non-ASCII paths (e.g. Turkish filenames) raw
	// instead of octal-backslash-quoted, so the path we return round-trips back
	// into Stage/Diff/`git add -- <path>` unchanged. Tab/newline still quote, so
	// the rename `\t`-split stays safe.
	out, stderr, err := h.runGit(c.Request.Context(), nil,
		"-C", repo.LocalPath, "-c", "core.quotepath=false", "status", "--porcelain=v2", "--branch")
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "status", false, gin.H{"repoId": repo.ID})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git status: " + gitErrMsg(stderr, err)})
		return
	}
	st := parsePorcelainV2(out)
	auditGit(h.db, userID, repo.ConnectionID, "status", true, gin.H{"repoId": repo.ID})
	c.JSON(http.StatusOK, gin.H{"data": st})
}

// parsePorcelainV2 turns `git status --porcelain=v2 --branch` output into the
// GitStatus DTO. See git-status(1) "Porcelain Format Version 2".
func parsePorcelainV2(out string) gitStatusDTO {
	st := gitStatusDTO{
		Staged:     []gitStatusEntry{},
		Unstaged:   []gitStatusEntry{},
		Untracked:  []string{},
		Conflicted: []string{},
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case '#': // branch header: "# branch.<k> <v>"
			fields := strings.SplitN(line, " ", 3)
			if len(fields) < 3 {
				continue
			}
			switch fields[1] {
			case "branch.head":
				st.Branch = fields[2]
			case "branch.upstream":
				st.Upstream = fields[2]
			case "branch.ab":
				// "+<ahead> -<behind>"
				for _, tok := range strings.Fields(fields[2]) {
					if len(tok) < 2 {
						continue
					}
					n, _ := strconv.Atoi(tok[1:])
					if tok[0] == '+' {
						st.Ahead = n
					} else if tok[0] == '-' {
						st.Behind = n
					}
				}
			}
		case '1': // ordinary: "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>"
			parts := strings.SplitN(line, " ", 9)
			if len(parts) < 9 {
				continue
			}
			addXYEntry(&st, parts[1], parts[8], "")
		case '2': // renamed/copied: "2 <XY> ... <Xscore> <path>\t<origPath>"
			parts := strings.SplitN(line, " ", 10)
			if len(parts) < 10 {
				continue
			}
			path, oldPath := parts[9], ""
			if tab := strings.IndexByte(parts[9], '\t'); tab >= 0 {
				path = parts[9][:tab]
				oldPath = parts[9][tab+1:]
			}
			addXYEntry(&st, parts[1], path, oldPath)
		case 'u': // unmerged: "u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>"
			parts := strings.SplitN(line, " ", 11)
			if len(parts) < 11 {
				continue
			}
			st.Conflicted = append(st.Conflicted, parts[10])
		case '?': // untracked: "? <path>"
			st.Untracked = append(st.Untracked, strings.TrimSpace(line[1:]))
		case '!': // ignored — skip
		}
	}
	st.Clean = len(st.Staged) == 0 && len(st.Unstaged) == 0 &&
		len(st.Untracked) == 0 && len(st.Conflicted) == 0
	return st
}

// addXYEntry splits a porcelain-v2 XY code into a staged entry (from X, the
// index column) and/or an unstaged entry (from Y, the worktree column). '.'
// means "unchanged in that column".
func addXYEntry(st *gitStatusDTO, xy, path, oldPath string) {
	if len(xy) < 2 {
		return
	}
	x, y := xy[0], xy[1]
	if x != '.' {
		st.Staged = append(st.Staged, gitStatusEntry{
			Path: path, X: string(x), Y: string(y),
			Status: statusCodeToName(x), OldPath: oldPath,
		})
	}
	if y != '.' {
		st.Unstaged = append(st.Unstaged, gitStatusEntry{
			Path: path, X: string(x), Y: string(y),
			Status: statusCodeToName(y), OldPath: oldPath,
		})
	}
}

// statusCodeToName maps a porcelain XY letter to the GitStatusEntry.status
// union.
func statusCodeToName(code byte) string {
	switch code {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "renamed" // copied — folded into renamed for the union
	default: // 'M', 'T', 'U', ...
		return "modified"
	}
}

// ── Log ─────────────────────────────────────────────────────────────

// Log — GET /git/repos/:repoId/log?ref=&limit=. git log with parents for the DAG.
func (h *GitOpsHandler) Log(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")

	limit := 100
	if l := strings.TrimSpace(c.Query("limit")); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	// Field sep 0x1f, record sep 0x1e — survives multi-line commit bodies.
	const fmtStr = "--pretty=format:%H%x1f%h%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%B%x1e"
	args := []string{"-C", repo.LocalPath, "log", fmtStr, "-n", strconv.Itoa(limit)}
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		if !safeArg(ref) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
			return
		}
		args = append(args, ref)
	}

	out, stderr, err := h.runGit(c.Request.Context(), nil, args...)
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "log", false, gin.H{"repoId": repo.ID})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git log: " + gitErrMsg(stderr, err)})
		return
	}

	commits := make([]gitRemoteCommit, 0)
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) < 8 {
			continue
		}
		parents := []string{}
		parents = append(parents, strings.Fields(f[2])...)
		commits = append(commits, gitRemoteCommit{
			Sha: f[0], ShortSha: f[1], Parents: parents,
			AuthorName: f[3], AuthorEmail: f[4], AuthoredAt: f[5],
			Title: f[6], Message: f[7],
		})
	}
	auditGit(h.db, userID, repo.ConnectionID, "log", true, gin.H{"repoId": repo.ID, "count": len(commits)})
	c.JSON(http.StatusOK, gin.H{"data": commits})
}

// ── Branches ────────────────────────────────────────────────────────

// Branches — GET /git/repos/:repoId/branches. Local branches, current flagged.
func (h *GitOpsHandler) Branches(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")

	// %(HEAD) = "*" for the checked-out branch; upstream:track,nobracket gives
	// "ahead N, behind M" (or "" / "gone").
	const fmtStr = "--format=%(refname:short)%1f%(objectname)%1f%(HEAD)%1f%(upstream:track,nobracket)"
	out, stderr, err := h.runGit(c.Request.Context(), nil,
		"-C", repo.LocalPath, "for-each-ref", fmtStr, "refs/heads")
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "branches", false, gin.H{"repoId": repo.ID})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git for-each-ref: " + gitErrMsg(stderr, err)})
		return
	}

	branches := make([]gitLocalBranch, 0)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 3 {
			continue
		}
		b := gitLocalBranch{
			Name:      f[0],
			CommitSha: f[1],
			Current:   strings.TrimSpace(f[2]) == "*",
			IsDefault: f[0] == repo.DefaultBranch,
		}
		if len(f) >= 4 {
			b.Ahead, b.Behind = parseTrack(f[3])
		}
		branches = append(branches, b)
	}
	auditGit(h.db, userID, repo.ConnectionID, "branches", true, gin.H{"repoId": repo.ID, "count": len(branches)})
	c.JSON(http.StatusOK, gin.H{"data": branches})
}

// parseTrack reads "ahead N, behind M" (upstream:track,nobracket) into counts.
func parseTrack(s string) (ahead, behind int) {
	for _, part := range strings.Split(s, ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		}
	}
	return ahead, behind
}

// ── Diff ────────────────────────────────────────────────────────────

// Diff — GET /git/repos/:repoId/diff?path=&staged=. git diff [--cached] -- path.
func (h *GitOpsHandler) Diff(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	path := strings.TrimSpace(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	// Match Status: keep non-ASCII paths raw in the patch headers.
	args := []string{"-C", repo.LocalPath, "-c", "core.quotepath=false", "diff"}
	if isTrue(c.Query("staged")) {
		args = append(args, "--cached")
	}
	args = append(args, "--", path)

	out, stderr, err := h.runGit(c.Request.Context(), nil, args...)
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "diff", false, gin.H{"repoId": repo.ID, "path": path})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git diff: " + gitErrMsg(stderr, err)})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "diff", true, gin.H{"repoId": repo.ID, "path": path})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"path": path, "patch": out}})
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ── Stage / Unstage ─────────────────────────────────────────────────

// Stage — POST /git/repos/:repoId/stage. Whole file: git add -- path. Hunk:
// git apply --cached of the supplied patch.
func (h *GitOpsHandler) Stage(c *gin.Context) {
	h.stageOrUnstage(c, false)
}

// Unstage — POST /git/repos/:repoId/unstage. Whole file: git restore --staged
// -- path. Hunk: reverse-apply the patch to the index.
func (h *GitOpsHandler) Unstage(c *gin.Context) {
	h.stageOrUnstage(c, true)
}

func (h *GitOpsHandler) stageOrUnstage(c *gin.Context, unstage bool) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	op := "stage"
	if unstage {
		op = "unstage"
	}

	var in gitStageInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.Path = strings.TrimSpace(in.Path)
	ctx := c.Request.Context()

	var stderr string
	var err error
	if strings.TrimSpace(in.Patch) != "" {
		// Hunk mode: write the patch to a 0600 temp (read by git, never
		// executed) and apply it to the index (reverse for unstage).
		tmp, terr := os.CreateTemp("", "patchman-git-hunk-*.patch")
		if terr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create temp patch: " + terr.Error()})
			return
		}
		tmpPath := tmp.Name()
		_ = tmp.Chmod(0o600)
		if _, werr := tmp.WriteString(in.Patch); werr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write temp patch: " + werr.Error()})
			return
		}
		_ = tmp.Close()
		defer func() { _ = os.Remove(tmpPath) }()

		args := []string{"-C", repo.LocalPath, "apply", "--cached"}
		if unstage {
			args = append(args, "-R")
		}
		args = append(args, tmpPath)
		_, stderr, err = h.runGit(ctx, nil, args...)
	} else {
		if in.Path == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "path or patch is required"})
			return
		}
		if unstage {
			_, stderr, err = h.runGit(ctx, nil, "-C", repo.LocalPath, "restore", "--staged", "--", in.Path)
		} else {
			_, stderr, err = h.runGit(ctx, nil, "-C", repo.LocalPath, "add", "--", in.Path)
		}
	}

	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, op, false, gin.H{"repoId": repo.ID, "path": in.Path})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git " + op + ": " + gitErrMsg(stderr, err)})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, op, true, gin.H{"repoId": repo.ID, "path": in.Path})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ── Commit ──────────────────────────────────────────────────────────

// Commit — POST /git/repos/:repoId/commit. git commit -m msg [--amend].
func (h *GitOpsHandler) Commit(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")

	var in gitCommitInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Message) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}
	ctx := c.Request.Context()

	args := []string{"-C", repo.LocalPath, "commit", "-m", in.Message}
	if in.Amend {
		args = append(args, "--amend")
	}
	if _, stderr, err := h.runGit(ctx, nil, args...); err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "commit", false, gin.H{"repoId": repo.ID})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git commit: " + gitErrMsg(stderr, err)})
		return
	}
	sha := ""
	if out, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "HEAD"); err == nil {
		sha = strings.TrimSpace(out)
	}
	auditGit(h.db, userID, repo.ConnectionID, "commit", true, gin.H{"repoId": repo.ID, "sha": sha, "amend": in.Amend})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"sha": sha}})
}

// ── Push ────────────────────────────────────────────────────────────

// Push — POST /git/repos/:repoId/push. NON-MUTATING: never checkout -b. Default
// 'branch-mr' pushes HEAD to a fresh patchman/<slug>-<unix> ref and opens a
// GitLab MR; 'direct' pushes HEAD straight to the target/current branch.
func (h *GitOpsHandler) Push(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")

	var in gitPushInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.AccessToken = h.opsToken(userID, repo.ConnectionID, in.AccessToken)
	if in.AccessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accessToken is required to push"})
		return
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "branch-mr"
	}
	ctx := c.Request.Context()

	env, cleanup, aerr := authEnv(in.AccessToken)
	if aerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare auth: " + aerr.Error()})
		return
	}
	defer cleanup()

	switch mode {
	case "direct":
		target := strings.TrimSpace(in.TargetBranch)
		if target == "" {
			// Fall back to the current branch name.
			if out, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				target = strings.TrimSpace(out)
			}
		}
		if !safeArg(target) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid targetBranch"})
			return
		}
		if _, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, "push", "origin", "HEAD:refs/heads/"+target); err != nil {
			auditGit(h.db, userID, repo.ConnectionID, "push", false, gin.H{"repoId": repo.ID, "mode": mode, "branch": target})
			c.JSON(http.StatusBadGateway, gin.H{"error": "git push: " + gitErrMsg(stderr, err)})
			return
		}
		auditGit(h.db, userID, repo.ConnectionID, "push", true, gin.H{"repoId": repo.ID, "mode": mode, "branch": target})
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"branch": target}})

	default: // branch-mr
		slugSrc := in.MRTitle
		if strings.TrimSpace(slugSrc) == "" {
			if out, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				slugSrc = strings.TrimSpace(out)
			}
		}
		slug := slugify(slugSrc)
		if slug == "" {
			slug = "change"
		}
		branch := fmt.Sprintf("patchman/%s-%d", slug, time.Now().Unix())

		if _, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
			auditGit(h.db, userID, repo.ConnectionID, "push", false, gin.H{"repoId": repo.ID, "mode": mode, "branch": branch})
			c.JSON(http.StatusBadGateway, gin.H{"error": "git push: " + gitErrMsg(stderr, err)})
			return
		}

		target := strings.TrimSpace(in.TargetBranch)
		if target == "" {
			target = repo.DefaultBranch
		}
		title := strings.TrimSpace(in.MRTitle)
		if title == "" {
			title = branch
		}
		mrURL, mrErr := h.openMergeRequest(ctx, repo, in.AccessToken, branch, target, title, in.MRDescription)
		if mrErr != nil {
			// The branch is already on the remote; surface it in the error so the
			// user can retry the MR without re-pushing.
			auditGit(h.db, userID, repo.ConnectionID, "push", false, gin.H{"repoId": repo.ID, "mode": mode, "branch": branch, "mrError": mrErr.Error()})
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("pushed branch %s, but opening the merge request failed: %s", branch, mrErr.Error())})
			return
		}
		auditGit(h.db, userID, repo.ConnectionID, "push", true, gin.H{"repoId": repo.ID, "mode": mode, "branch": branch, "mrUrl": mrURL})
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"branch": branch, "mrUrl": mrURL}})
	}
}

// Pull fast-forwards the local clone from origin so History/Browse reflect the
// latest remote state (a clone made days ago otherwise never advances). It is
// non-destructive: fetch + merge --ff-only, so local edits or diverged history
// make it fail loudly rather than create a merge commit. Mirrors Push's auth
// (short-lived token via the askpass helper). POST .../repos/:repoId/pull
func (h *GitOpsHandler) Pull(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")

	var in gitPullInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.AccessToken = h.opsToken(userID, repo.ConnectionID, in.AccessToken)
	if in.AccessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accessToken is required to pull"})
		return
	}
	ctx := c.Request.Context()

	env, cleanup, aerr := authEnv(in.AccessToken)
	if aerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "prepare auth: " + aerr.Error()})
		return
	}
	defer cleanup()

	branch := ""
	if out, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch = strings.TrimSpace(out)
	}

	if _, stderr, err := h.runGit(ctx, env, "-C", repo.LocalPath, "pull", "--ff-only"); err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "pull", false, gin.H{"repoId": repo.ID, "branch": branch})
		c.JSON(http.StatusBadGateway, gin.H{"error": "git pull (fast-forward only): " + gitErrMsg(stderr, err)})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "pull", true, gin.H{"repoId": repo.ID, "branch": branch})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"branch": branch, "pulled": true}})
}

// openMergeRequest opens a GitLab MR via a self-contained REST POST to the
// connection's instance, authenticated with the passed short-lived token. The
// API base is the connection BaseURL, falling back to the clone URL's origin
// when the connection carries none (e.g. oauth connections).
func (h *GitOpsHandler) openMergeRequest(ctx context.Context, repo *models.GitRepo, token, source, target, title, desc string) (string, error) {
	base := h.gitAPIBase(repo)
	if base == "" {
		return "", fmt.Errorf("no GitLab base URL available for this connection")
	}
	if strings.TrimSpace(repo.ProjectRef) == "" {
		return "", fmt.Errorf("repo has no project reference")
	}
	endpoint := base + projectPath(repo.ProjectRef) + "/merge_requests"

	body, _ := json.Marshal(map[string]string{
		"source_branch": source,
		"target_branch": target,
		"title":         title,
		"description":   desc,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("gitlab %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var mr struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", fmt.Errorf("decode merge request response: %w", err)
	}
	return mr.WebURL, nil
}

// gitAPIBase resolves the "<scheme>://<host>/api/v4" prefix for the repo's
// GitLab instance: the connection's BaseURL when set, else the clone URL's
// origin. The repo is already owner-scoped by the caller, so loading the
// connection by id (for its BaseURL only) needs no extra access check.
func (h *GitOpsHandler) gitAPIBase(repo *models.GitRepo) string {
	base := ""
	if repo.ConnectionID != "" {
		var conn models.GitConnection
		if err := h.db.First(&conn, "id = ?", repo.ConnectionID).Error; err == nil {
			base = strings.TrimSpace(conn.BaseURL)
		}
	}
	if base == "" {
		if u, err := url.Parse(strings.TrimSpace(repo.RemoteURL)); err == nil && u.Scheme != "" && u.Host != "" {
			base = u.Scheme + "://" + u.Host
		}
	}
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/api/v4"
}

// ── LocalFile ───────────────────────────────────────────────────────

// LocalFile — GET /git/repos/:repoId/file?path=&ref=. With a ref: git show
// ref:path (committed blob, e.g. the Monaco diff base). Without a ref: the
// working-tree file, read from disk inside the clone (traversal-guarded).
func (h *GitOpsHandler) LocalFile(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	path := strings.TrimSpace(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	ref := strings.TrimSpace(c.Query("ref"))

	var content string
	if ref != "" {
		if !safeArg(ref) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref"})
			return
		}
		out, stderr, err := h.runGit(c.Request.Context(), nil, "-C", repo.LocalPath, "show", ref+":"+path)
		if err != nil {
			auditGit(h.db, userID, repo.ConnectionID, "file", false, gin.H{"repoId": repo.ID, "path": path, "ref": ref})
			c.JSON(http.StatusBadGateway, gin.H{"error": "git show: " + gitErrMsg(stderr, err)})
			return
		}
		content = out
	} else {
		// Worktree read — confine the resolved path to the clone dir.
		full := filepath.Clean(filepath.Join(repo.LocalPath, path))
		if full != repo.LocalPath && !strings.HasPrefix(full, repo.LocalPath+string(os.PathSeparator)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "path escapes the repository"})
			return
		}
		b, err := os.ReadFile(full)
		if err != nil {
			auditGit(h.db, userID, repo.ConnectionID, "file", false, gin.H{"repoId": repo.ID, "path": path})
			c.JSON(http.StatusNotFound, gin.H{"error": "read file: " + err.Error()})
			return
		}
		content = string(b)
	}
	auditGit(h.db, userID, repo.ConnectionID, "file", true, gin.H{"repoId": repo.ID, "path": path, "ref": ref})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"path": path, "content": content}})
}

// WriteFile — POST /git/repos/:repoId/file  { path, content }. Writes content
// to a working-tree file inside the clone so the in-app editor / conflict
// resolver can save edits before stage/commit. Same gate as the other local
// ops (embedded-only, owner-scoped via resolveRepo). Traversal-guarded exactly
// like LocalFile, and refuses to write anywhere under .git. Preserves an
// existing file's permission bits (so an executable script stays executable).
func (h *GitOpsHandler) WriteFile(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	full := filepath.Clean(filepath.Join(repo.LocalPath, path))
	if full == repo.LocalPath || !strings.HasPrefix(full, repo.LocalPath+string(os.PathSeparator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path escapes the repository"})
		return
	}
	// Never let a write land inside the .git dir.
	gitDir := filepath.Join(repo.LocalPath, ".git")
	if full == gitDir || strings.HasPrefix(full, gitDir+string(os.PathSeparator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot write inside .git"})
		return
	}
	// Preserve the existing file mode; default to 0644 for a new file.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(full); statErr == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mkdir: " + err.Error()})
		return
	}
	if err := os.WriteFile(full, []byte(in.Content), mode); err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "write_file", false, gin.H{"repoId": repo.ID, "path": path})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write file: " + err.Error()})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "write_file", true, gin.H{"repoId": repo.ID, "path": path})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "path": path}})
}

// RevealPath — GET /git/repos/:repoId/local-path. Returns the clone's absolute
// working-tree path so the desktop app can open it in the OS file manager
// ("Show in Finder"). Embedded/owner-scoped via resolveRepo.
func (h *GitOpsHandler) RevealPath(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"path": repo.LocalPath}})
}
