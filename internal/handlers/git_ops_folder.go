package handlers

// Open folder — registers a git working tree the user already has on disk
// (anywhere, not just the managed clone dir) as a local repo, so every
// /git/repos/:repoId/* op works on it. Desktop-only like the rest of
// GitOpsHandler (gitGate). The folder is never moved, copied or deleted:
// CloneKind "external" marks it, and nothing in the handlers removes an
// external repo's directory.

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

const cloneKindExternal = "external"

type gitOpenFolderInput struct {
	RepoID string `json:"repoId"`
	Path   string `json:"path"`
}

// OpenFolder — POST /git/repos/open {repoId, path}. Resolves path to its
// work-tree root and registers it (idempotent per user + root: reopening a
// folder returns the existing row). When origin's host matches one of the
// caller's git connections the repo is tied to it (project = the GitLab path)
// so MRs / CI / token push work; otherwise it stands alone and remote ops use
// the machine's own git credentials.
func (h *GitOpsHandler) OpenFolder(c *gin.Context) {
	if !h.gitGate(c) {
		return
	}
	userID := c.GetString("userID")
	var in gitOpenFolderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.RepoID = strings.TrimSpace(in.RepoID)
	if !repoIDPattern.MatchString(in.RepoID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repoId must be a UUID-like token"})
		return
	}
	dir := strings.TrimSpace(in.Path)
	if dir == "" || !filepath.IsAbs(dir) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path must be an absolute folder path"})
		return
	}
	dir, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is not a folder"})
		return
	}

	ctx := c.Request.Context()
	out, _, err := h.runGit(ctx, nil, "-C", dir, "rev-parse", "--show-toplevel")
	root := strings.TrimSpace(out)
	if err != nil || root == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "This folder is not inside a git repository"})
		return
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	// Already registered (an earlier open, or a managed clone): reuse it.
	// Stored paths may differ by symlinks (/var vs /private/var), so compare
	// resolved paths.
	var mine []models.GitRepo
	h.db.Where("user_id = ?", userID).Find(&mine)
	for _, r := range mine {
		p := filepath.Clean(r.LocalPath)
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			p = rp
		}
		if p == root {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"repo": r}})
			return
		}
	}

	origin := ""
	if o, _, err := h.runGit(ctx, nil, "-C", root, "remote", "get-url", "origin"); err == nil {
		origin = strings.TrimSpace(o)
	}
	repo := models.GitRepo{
		ID:          in.RepoID,
		UserID:      userID,
		ProjectRef:  "local:" + in.RepoID,
		ProjectName: filepath.Base(root),
		RemoteURL:   redactURL(origin),
		LocalPath:   root,
		CloneKind:   cloneKindExternal,
		CreatedAt:   time.Now(),
	}
	if origin != "" {
		var conns []models.GitConnection
		h.db.Where("user_id = ?", userID).Order("created_at").Find(&conns)
		for _, cn := range conns {
			if project := originProject(origin, connBaseURL(cn)); project != "" {
				repo.ConnectionID = cn.ID
				repo.ProjectRef = project
				repo.ProjectName = project
				break
			}
		}
	}
	if o, _, err := h.runGit(ctx, nil, "-C", root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		repo.DefaultBranch = strings.TrimPrefix(strings.TrimSpace(o), "origin/")
	} else if o, _, err := h.runGit(ctx, nil, "-C", root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		repo.DefaultBranch = strings.TrimSpace(o)
	}

	if err := h.db.Create(&repo).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save repo: " + err.Error()})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "open_folder", true, gin.H{"repoId": repo.ID, "linked": repo.ConnectionID != ""})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"repo": repo}})
}

// remoteHostPath splits a git remote URL into host and project path (no
// leading slash, no .git): https://host/g/p.git, ssh://git@host:22/g/p.git
// and scp-style git@host:g/p.git. Anything else yields "", "".
func remoteHostPath(remote string) (host, project string) {
	r := strings.TrimSpace(remote)
	if r == "" {
		return "", ""
	}
	if strings.Contains(r, "://") {
		u, err := url.Parse(r)
		if err != nil {
			return "", ""
		}
		host, project = u.Hostname(), u.Path
	} else if at := strings.Index(r, "@"); at >= 0 {
		rest := r[at+1:]
		colon := strings.Index(rest, ":")
		if colon <= 0 {
			return "", ""
		}
		host, project = rest[:colon], rest[colon+1:]
	} else {
		return "", ""
	}
	project = strings.TrimSuffix(strings.Trim(project, "/"), ".git")
	if host == "" || project == "" {
		return "", ""
	}
	return host, project
}

// connBaseURL is the GitLab a connection talks to: its BaseURL, else the
// server default (GITLAB_BASE_URL, as config.Load reads it).
func connBaseURL(cn models.GitConnection) string {
	if b := strings.TrimSpace(cn.BaseURL); b != "" {
		return b
	}
	if b := strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")); b != "" {
		return b
	}
	return "https://gitlab.com"
}

// originProject returns the GitLab project path when origin points at the
// GitLab at base, else "". An http(s) origin must match scheme AND host:port
// exactly — the linked connection's token is answered for that origin's
// host, so a look-alike (other port / plain http) must stay unlinked. SSH
// origins match on hostname; no token is ever answered for them (tokenHost).
// A base with a path prefix (https://host/gitlab) is stripped from the path.
func originProject(origin, base string) string {
	bu, err := url.Parse(strings.TrimSpace(base))
	if err != nil || bu.Host == "" {
		return ""
	}
	host, project := remoteHostPath(origin)
	if host == "" {
		return ""
	}
	if ou, err := url.Parse(strings.TrimSpace(origin)); err == nil && (ou.Scheme == "https" || ou.Scheme == "http") {
		if !strings.EqualFold(ou.Scheme, bu.Scheme) || !strings.EqualFold(ou.Host, bu.Host) {
			return ""
		}
		if prefix := strings.Trim(bu.Path, "/"); prefix != "" {
			if !strings.HasPrefix(project, prefix+"/") {
				return ""
			}
			project = strings.TrimPrefix(project, prefix+"/")
		}
		return project
	}
	if !strings.EqualFold(host, bu.Hostname()) {
		return ""
	}
	return project
}

// tokenAllowed reports whether a PatchMan-held token may be offered for this
// repo's remote. A stand-alone external folder has no connection, so its
// origin can be any host (github.com, …): never hand it a GitLab token —
// git falls back to the machine's own credential helper / SSH keys.
func tokenAllowed(repo *models.GitRepo) bool {
	return repo.CloneKind != cloneKindExternal || repo.ConnectionID != ""
}

// markMissing flags rows whose folder is gone (moved / deleted outside the app)
// so the FE can grey them out in the recent list.
func markMissing(repos []models.GitRepo) {
	for i := range repos {
		if fi, err := os.Stat(repos[i].LocalPath); err != nil || !fi.IsDir() {
			repos[i].Missing = true
		}
	}
}
