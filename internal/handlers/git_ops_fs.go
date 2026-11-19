package handlers

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/models"
)

// Working-tree surface for the in-app editor (GIT_IDE_PLAN §H): list a
// folder of the clone with git status, blame a file, and create / rename /
// delete files and folders. Every path stays inside the clone, never inside
// .git, and never goes through a symlink that leads out.

// repoPath resolves rel inside the clone. The parent directory is resolved
// through symlinks and must still be inside the clone's real root.
func repoPath(repo *models.GitRepo, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("a relative path is required")
	}
	root, err := filepath.EvalSymlinks(repo.LocalPath)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(root, rel))
	if full == root || !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("path escapes the repository")
	}
	gitDir := filepath.Join(root, ".git")
	if full == gitDir || strings.HasPrefix(full, gitDir+string(os.PathSeparator)) {
		return "", errors.New("paths inside .git are off limits")
	}
	// The deepest existing ancestor must resolve inside the root.
	dir := filepath.Dir(full)
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errors.New("path leads outside the repository")
	}
	return full, nil
}

type gitLsEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"` // dir | file | symlink
	Status  string `json:"status,omitempty"`
	Ignored bool   `json:"ignored,omitempty"`
}

// porcelainStatus maps path → one-word status for the whole clone (dirs get
// "modified" when anything under them changed).
func (h *GitOpsHandler) porcelainStatus(c *gin.Context, dir string) (map[string]string, map[string]bool) {
	st := map[string]string{}
	ign := map[string]bool{}
	out, _, err := h.runGit(c.Request.Context(), nil, "-C", dir, "-c", "core.quotepath=false", "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return st, ign
	}
	parts := strings.Split(out, "\x00")
	for i := 0; i < len(parts); i++ {
		e := parts[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], strings.TrimSuffix(e[3:], "/")
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the old path follows
		}
		var s string
		switch {
		case xy == "!!":
			ign[p] = true
			continue
		case xy == "??":
			s = "untracked"
		case strings.Contains(xy, "U") || xy == "AA" || xy == "DD":
			s = "conflicted"
		case strings.Contains(xy, "A"):
			s = "added"
		case strings.Contains(xy, "D"):
			s = "deleted"
		case strings.Contains(xy, "R"):
			s = "renamed"
		default:
			s = "modified"
		}
		st[p] = s
		for d := filepath.Dir(p); d != "." && d != "/"; d = filepath.Dir(d) {
			if _, ok := st[d]; !ok {
				st[d] = "modified"
			}
		}
	}
	return st, ign
}

// LsDir — GET /git/repos/:repoId/ls?path= (one folder of the working tree).
func (h *GitOpsHandler) LsDir(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	rel := strings.Trim(strings.TrimSpace(c.Query("path")), "/")
	dir := repo.LocalPath
	if rel != "" {
		full, err := repoPath(repo, rel)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		dir = full
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	st, ign := h.porcelainStatus(c, repo.LocalPath)
	out := make([]gitLsEntry, 0, len(items))
	for _, it := range items {
		if rel == "" && it.Name() == ".git" {
			continue
		}
		p := it.Name()
		if rel != "" {
			p = rel + "/" + it.Name()
		}
		e := gitLsEntry{Name: it.Name(), Path: p, Type: "file", Status: st[p]}
		switch {
		case it.Type()&os.ModeSymlink != 0:
			e.Type = "symlink"
		case it.IsDir():
			e.Type = "dir"
		}
		e.Ignored = ign[p]
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "dir") != (out[j].Type == "dir") {
			return out[i].Type == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type gitBlameLine struct {
	Sha     string `json:"sha"`
	Author  string `json:"author"`
	Time    int64  `json:"time"`
	Summary string `json:"summary"`
}

// Blame — GET /git/repos/:repoId/blame?path=. One entry per line of the
// working-tree file (uncommitted lines come back with sha 0000…).
func (h *GitOpsHandler) Blame(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	rel := strings.TrimSpace(c.Query("path"))
	if _, err := repoPath(repo, rel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	env, cleanup, ok := h.withAuth(c, repo, c.Query("accessToken"))
	if !ok {
		return
	}
	defer cleanup()
	out, stderr, err := h.runGit(c.Request.Context(), env, "-C", repo.LocalPath, "blame", "--line-porcelain", "--", rel)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git blame: " + gitErrMsg(stderr, err)})
		return
	}
	lines := []gitBlameLine{}
	var cur gitBlameLine
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "\t"):
			lines = append(lines, cur)
		case strings.HasPrefix(l, "author "):
			cur.Author = strings.TrimPrefix(l, "author ")
		case strings.HasPrefix(l, "author-time "):
			cur.Time, _ = strconv.ParseInt(strings.TrimPrefix(l, "author-time "), 10, 64)
		case strings.HasPrefix(l, "summary "):
			cur.Summary = strings.TrimPrefix(l, "summary ")
		default:
			if f := strings.Fields(l); len(f) >= 3 && len(f[0]) == 40 {
				cur = gitBlameLine{Sha: f[0]}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": lines})
}

type gitFsInput struct {
	Op      string `json:"op"` // create | mkdir | rename | delete
	Path    string `json:"path"`
	NewPath string `json:"newPath"`
	Content string `json:"content"`
}

// FsOp — POST /git/repos/:repoId/fs. Create a file / folder, rename or move
// (git mv for tracked files), delete (git rm for tracked ones, so the change
// shows staged the way an IDE does it).
func (h *GitOpsHandler) FsOp(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitFsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	full, err := repoPath(repo, in.Path)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	tracked := func(rel string) bool {
		_, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "ls-files", "--error-unmatch", "--", rel)
		return err == nil
	}
	userID := c.GetString("userID")
	detail := gin.H{"repoId": repo.ID, "op": in.Op, "path": in.Path, "newPath": in.NewPath}
	fail := func(code int, msg string) {
		auditGit(h.db, userID, repo.ConnectionID, "fs_"+in.Op, false, detail)
		c.JSON(code, gin.H{"error": msg})
	}
	switch in.Op {
	case "create":
		if _, err := os.Lstat(full); err == nil {
			fail(http.StatusConflict, "a file or folder with that name already exists")
			return
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
	case "mkdir":
		if _, err := os.Lstat(full); err == nil {
			fail(http.StatusConflict, "a file or folder with that name already exists")
			return
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
	case "rename":
		dst, err := repoPath(repo, in.NewPath)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if _, err := os.Lstat(dst); err == nil {
			fail(http.StatusConflict, "the target already exists")
			return
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		if tracked(in.Path) {
			if _, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "mv", "--", in.Path, in.NewPath); err != nil {
				fail(http.StatusConflict, "git mv: "+gitErrMsg(stderr, err))
				return
			}
		} else if err := os.Rename(full, dst); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
	case "delete":
		if _, err := os.Lstat(full); err != nil {
			fail(http.StatusNotFound, "no such file or folder")
			return
		}
		if tracked(in.Path) {
			if _, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "rm", "-r", "-f", "--quiet", "--", in.Path); err != nil {
				fail(http.StatusConflict, "git rm: "+gitErrMsg(stderr, err))
				return
			}
		}
		// Untracked leftovers (or an untracked file/folder) go from disk.
		if err := os.RemoveAll(full); err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "op must be create, mkdir, rename or delete"})
		return
	}
	auditGit(h.db, userID, repo.ConnectionID, "fs_"+in.Op, true, detail)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
