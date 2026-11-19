package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// JetBrains-style Push dialog backend (GIT_IDE_PLAN §D): the commits that
// would go out, what each one changes, and a push with an explicit remote,
// target branch and options. Force push (with lease) is its own sensitive
// route so the step-up gate can tell it apart.

const commitLogFmt = "--pretty=format:%H%x1f%h%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%B%x1e"

func parseCommitLog(out string) []gitRemoteCommit {
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
		commits = append(commits, gitRemoteCommit{
			Sha: f[0], ShortSha: f[1], Parents: strings.Fields(f[2]),
			AuthorName: f[3], AuthorEmail: f[4], AuthoredAt: f[5],
			Title: f[6], Message: strings.TrimSpace(f[7]),
		})
	}
	return commits
}

// Outgoing — GET /git/repos/:repoId/outgoing?remote=origin&branch=x.
// Commits on HEAD that <remote>/<branch> lacks; when that branch does not
// exist yet, the commits no branch of <remote> has (a new branch).
func (h *GitOpsHandler) Outgoing(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	remote := strings.TrimSpace(c.DefaultQuery("remote", "origin"))
	branch := strings.TrimSpace(c.Query("branch"))
	if !validRemoteName(remote) || (branch != "" && !safeArg(branch)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote or branch"})
		return
	}
	ctx := c.Request.Context()
	dir := repo.LocalPath
	exists := false
	if branch != "" {
		_, _, err := h.runGit(ctx, nil, "-C", dir, "show-ref", "--verify", "--quiet", "refs/remotes/"+remote+"/"+branch)
		exists = err == nil
	}
	args := []string{"-C", dir, "log", commitLogFmt, "-n", "300"}
	if exists {
		args = append(args, remote+"/"+branch+"..HEAD")
	} else {
		args = append(args, "HEAD", "--not", "--remotes="+remote)
	}
	out, stderr, err := h.runGit(ctx, nil, args...)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git log: " + gitErrMsg(stderr, err)})
		return
	}
	upstream := ""
	if u, _, err := h.runGit(ctx, nil, "-C", dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		upstream = strings.TrimSpace(u)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"commits":      parseCommitLog(out),
		"remoteExists": exists,
		"upstream":     upstream,
	}})
}

type gitCommitFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"` // added | modified | deleted | renamed | copied | typechange
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

// CommitFiles — GET /git/repos/:repoId/commit-files?sha=. The files one
// commit changes (against its first parent; the root commit against empty).
func (h *GitOpsHandler) CommitFiles(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	sha := strings.TrimSpace(c.Query("sha"))
	if !safeArg(sha) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sha"})
		return
	}
	ctx := c.Request.Context()
	base := []string{"-C", repo.LocalPath, "-c", "core.quotepath=false", "diff-tree", "--root", "-r", "-M", "--no-commit-id"}
	ns, stderr, err := h.runGit(ctx, nil, append(base, "--name-status", "-z", sha)...)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git diff-tree: " + gitErrMsg(stderr, err)})
		return
	}
	files := []gitCommitFile{}
	idx := map[string]int{}
	parts := strings.Split(ns, "\x00")
	for i := 0; i < len(parts); {
		code := parts[i]
		if code == "" {
			i++
			continue
		}
		f := gitCommitFile{Status: map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'R': "renamed", 'C': "copied", 'T': "typechange"}[code[0]]}
		if f.Status == "" {
			f.Status = "modified"
		}
		if (code[0] == 'R' || code[0] == 'C') && i+2 < len(parts) {
			f.OldPath, f.Path = parts[i+1], parts[i+2]
			i += 3
		} else if i+1 < len(parts) {
			f.Path = parts[i+1]
			i += 2
		} else {
			break
		}
		idx[f.Path] = len(files)
		files = append(files, f)
	}
	if nm, _, err := h.runGit(ctx, nil, append(base, "--numstat", "-z", sha)...); err == nil {
		// -z numstat: "add\tdel\tpath\0" or, for renames, "add\tdel\t\0old\0new\0".
		p := strings.Split(nm, "\x00")
		for i := 0; i < len(p); i++ {
			cols := strings.SplitN(p[i], "\t", 3)
			if len(cols) < 3 {
				continue
			}
			path := cols[2]
			if path == "" && i+2 < len(p) {
				path = p[i+2]
				i += 2
			}
			if j, ok := idx[path]; ok {
				if cols[0] == "-" {
					files[j].Binary = true
				} else {
					files[j].Additions, _ = strconv.Atoi(cols[0])
					files[j].Deletions, _ = strconv.Atoi(cols[1])
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": files})
}

type gitPushToInput struct {
	AccessToken string `json:"accessToken"`
	// Source (optional): a local branch to push instead of HEAD — pushing a
	// branch that is not checked out. Target defaults to its upstream branch.
	Source       string `json:"source"`
	Remote       string `json:"remote"`
	TargetBranch string `json:"targetBranch"`
	SetUpstream  bool   `json:"setUpstream"`
	Tags         bool   `json:"tags"`
	// Optional merge request once pushed (origin only).
	OpenMR        bool   `json:"openMr"`
	MRTarget      string `json:"mrTarget"`
	MRTitle       string `json:"mrTitle"`
	MRDescription string `json:"mrDescription"`
}

// PushTo — POST /git/repos/:repoId/push-to: push HEAD to
// <remote>:<targetBranch>. A rejected (non-fast-forward) push answers 409
// with rejected=true so the dialog can offer Update.
func (h *GitOpsHandler) PushTo(c *gin.Context) { h.pushTo(c, false) }

// ForcePushTo — POST /git/repos/:repoId/push-to/force: same, with
// --force-with-lease (never a bare --force). Sensitive route: step-up.
func (h *GitOpsHandler) ForcePushTo(c *gin.Context) { h.pushTo(c, true) }

func (h *GitOpsHandler) pushTo(c *gin.Context, force bool) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	var in gitPushToInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	in.Remote, in.TargetBranch = strings.TrimSpace(in.Remote), strings.TrimSpace(in.TargetBranch)
	if in.Remote == "" {
		in.Remote = "origin"
	}
	srcRef := "HEAD"
	if src := strings.TrimSpace(in.Source); src != "" {
		if !h.validBranchName(ctx, src) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source branch"})
			return
		}
		if _, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "show-ref", "--verify", "--quiet", "refs/heads/"+src); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no local branch " + src})
			return
		}
		srcRef = "refs/heads/" + src
		if in.TargetBranch == "" {
			if r, rb := h.upstreamOf(c, repo, src); r == in.Remote && rb != "" {
				in.TargetBranch = rb
			} else {
				in.TargetBranch = src
			}
		}
	}
	if in.TargetBranch == "" {
		if out, _, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
			in.TargetBranch = strings.TrimSpace(out)
		}
	}
	if !validRemoteName(in.Remote) || !h.validBranchName(ctx, in.TargetBranch) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote or target branch"})
		return
	}
	if in.OpenMR && h.gitUser(repo.ConnectionID) == "x-access-token" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Opening a pull request from PatchMan isn't available for GitHub yet — push without it and open the pull request on GitHub."})
		return
	}
	if in.OpenMR && in.Remote != "origin" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a merge request can only be opened for a push to origin"})
		return
	}
	token := h.opsToken(c.GetString("userID"), repo.ConnectionID, in.AccessToken)
	env, cleanup, ok := h.withAuth(c, repo, token)
	if !ok {
		return
	}
	defer cleanup()

	args := []string{"-C", repo.LocalPath, "push", "--porcelain"}
	if force {
		args = append(args, "--force-with-lease")
	}
	if in.SetUpstream {
		args = append(args, "--set-upstream")
	}
	if in.Tags {
		args = append(args, "--follow-tags")
	}
	args = append(args, in.Remote, srcRef+":refs/heads/"+in.TargetBranch)
	userID := c.GetString("userID")
	detail := gin.H{"repoId": repo.ID, "remote": in.Remote, "branch": in.TargetBranch, "force": force, "tags": in.Tags}
	out, stderr, err := h.runGit(ctx, env, args...)
	if err != nil {
		auditGit(h.db, userID, repo.ConnectionID, "push", false, detail)
		msg := gitErrMsg(stderr, err)
		rejected := strings.Contains(out+stderr, "[rejected]") || strings.Contains(out+stderr, "non-fast-forward") || strings.Contains(out+stderr, "stale info")
		c.JSON(http.StatusConflict, gin.H{"error": "git push: " + msg, "rejected": rejected})
		return
	}
	resp := gin.H{"remote": in.Remote, "branch": in.TargetBranch}
	if in.OpenMR {
		target := strings.TrimSpace(in.MRTarget)
		if target == "" {
			target = repo.DefaultBranch
		}
		title := strings.TrimSpace(in.MRTitle)
		if title == "" {
			title = in.TargetBranch
		}
		mrURL, mrErr := h.openMergeRequest(ctx, repo, token, in.TargetBranch, target, title, in.MRDescription)
		if mrErr != nil {
			detail["mrError"] = mrErr.Error()
			resp["mrError"] = mrErr.Error()
		} else {
			detail["mrUrl"] = mrURL
			resp["mrUrl"] = mrURL
		}
	}
	auditGit(h.db, userID, repo.ConnectionID, "push", true, detail)
	c.JSON(http.StatusOK, gin.H{"data": resp})
}
