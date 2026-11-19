package handlers

// GitHub as a git provider (phase 2): the remote reads behind the Git tab —
// repos, branches, commits, a commit's diff, the file tree, files, compare,
// contributors — answered from GitHub's REST API in the same shapes the
// GitLab handlers return, so the FE doesn't care which provider it talks to.
//
// Auth: a personal access token stored on the connection (authKind "pat").
// github.com → https://api.github.com; GitHub Enterprise Server → <base>/api/v3.
// Pull requests: git_github_pr.go. Actions: git_github_actions.go.
//
// The FE addresses a repo by String(repo.id) like on GitLab; a numeric id is
// resolved to owner/name once (GET /repositories/:id) and cached.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

func isGitHub(conn *models.GitConnection) bool {
	return conn != nil && strings.EqualFold(strings.TrimSpace(conn.Provider), "github")
}

// gitHubAPIBase maps a connection's base URL to its REST root.
func gitHubAPIBase(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b == "" || strings.EqualFold(b, "https://github.com") || strings.EqualFold(b, "http://github.com") || strings.EqualFold(b, "https://www.github.com") {
		return "https://api.github.com"
	}
	if strings.HasSuffix(b, "/api/v3") {
		return b
	}
	return b + "/api/v3"
}

// gitHubAPIBaseOverride points every GitHub connection at a fake / emulator
// (tests; GITHUB_API_BASE for local runs). Empty in real use.
var gitHubAPIBaseOverride = ""

// SetGitHubAPIBase sets gitHubAPIBaseOverride.
func SetGitHubAPIBase(base string) {
	gitHubAPIBaseOverride = strings.TrimRight(strings.TrimSpace(base), "/")
}

func (h *GitRemoteHandler) ghBase(conn *models.GitConnection) string {
	if gitHubAPIBaseOverride != "" {
		return gitHubAPIBaseOverride
	}
	return gitHubAPIBase(conn.BaseURL)
}

func ghRequest(client *http.Client, method, endpoint, token, accept string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return body, resp.Header, resp.StatusCode, nil
}

func (h *GitRemoteHandler) ghGET(conn *models.GitConnection, token, path string, q url.Values) ([]byte, http.Header, int, error) {
	endpoint := h.ghBase(conn) + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	return ghRequest(h.http, http.MethodGet, endpoint, token, "")
}

// ghJSON decodes a 2xx answer into out; otherwise writes a 502 (404 → 404)
// with GitHub's message and returns ok=false.
func (h *GitRemoteHandler) ghJSON(c *gin.Context, conn *models.GitConnection, token, path string, q url.Values, out any) (http.Header, bool) {
	body, hdr, status, err := h.ghGET(conn, token, path, q)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	if status/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		code := http.StatusBadGateway
		if status == http.StatusNotFound {
			code = http.StatusNotFound
		}
		c.JSON(code, gin.H{"error": fmt.Sprintf("github %d: %s", status, msg)})
		return nil, false
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "decode github response: " + err.Error()})
			return nil, false
		}
	}
	return hdr, true
}

var linkNext = regexp.MustCompile(`<[^>]*[?&]page=(\d+)[^>]*>;\s*rel="next"`)

// ghNextPage reads the next page number from a Link header (0 = last page).
func ghNextPage(h http.Header) int {
	if h == nil {
		return 0
	}
	m := linkNext.FindStringSubmatch(h.Get("Link"))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ── repo resolution ──────────────────────────────────────────────────────────

type ghRepoInfo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

var ghRepoCache sync.Map // connID|project → ghRepoInfo (+ time)

type ghCached struct {
	info ghRepoInfo
	at   time.Time
}

// ghRepo resolves the FE's project (numeric id or owner/name) to owner/name
// and the default branch.
func (h *GitRemoteHandler) ghRepo(c *gin.Context, conn *models.GitConnection, token, project string) (ghRepoInfo, bool) {
	key := conn.ID + "|" + project
	if v, ok := ghRepoCache.Load(key); ok {
		if e := v.(ghCached); time.Since(e.at) < 10*time.Minute {
			return e.info, true
		}
	}
	path := "/repos/" + project
	if _, err := strconv.ParseInt(project, 10, 64); err == nil {
		path = "/repositories/" + project
	} else if strings.Count(project, "/") != 1 || strings.ContainsAny(project, " ?#") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project must be a GitHub repository id or owner/name"})
		return ghRepoInfo{}, false
	}
	var info ghRepoInfo
	if _, ok := h.ghJSON(c, conn, token, path, nil, &info); !ok {
		return ghRepoInfo{}, false
	}
	ghRepoCache.Store(key, ghCached{info: info, at: time.Now()})
	return info, true
}

func ghRepoPath(info ghRepoInfo) string { return "/repos/" + info.FullName }

// ── reads ────────────────────────────────────────────────────────────────────

func (h *GitRemoteHandler) ghRepos(c *gin.Context, userID string, conn *models.GitConnection, token string) {
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	var all []struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		PushedAt      string `json:"pushed_at"`
		Private       bool   `json:"private"`
		Visibility    string `json:"visibility"`
	}
	q := url.Values{"per_page": {"100"}, "sort": {"pushed"}, "affiliation": {"owner,collaborator,organization_member"}}
	pages := 1
	if search != "" {
		pages = 5 // search the user's repos by name across up to 500
	}
	for page := 1; page <= pages; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []struct {
			ID            int64  `json:"id"`
			Name          string `json:"name"`
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			CloneURL      string `json:"clone_url"`
			PushedAt      string `json:"pushed_at"`
			Private       bool   `json:"private"`
			Visibility    string `json:"visibility"`
		}
		hdr, ok := h.ghJSON(c, conn, token, "/user/repos", q, &batch)
		if !ok {
			return
		}
		all = append(all, batch...)
		if ghNextPage(hdr) == 0 {
			break
		}
	}
	out := make([]gitRepoSummary, 0, len(all))
	for _, r := range all {
		if search != "" && !strings.Contains(strings.ToLower(r.FullName), search) {
			continue
		}
		vis := r.Visibility
		if vis == "" {
			vis = map[bool]string{true: "private", false: "public"}[r.Private]
		}
		out = append(out, gitRepoSummary{
			ID: r.ID, Name: r.Name, Path: r.Name, FullPath: r.FullName,
			DefaultBranch: r.DefaultBranch, HTTPURL: r.CloneURL, LastActivityAt: r.PushedAt, Visibility: vis,
		})
		if len(out) >= 50 {
			break
		}
	}
	h.conns.audit(userID, conn.ID, "repos", true, gin.H{"count": len(out), "provider": "github"})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GitRemoteHandler) ghBranches(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	out := []gitBranch{}
	q := url.Values{"per_page": {"100"}}
	for page := 1; page <= branchPageLimit; page++ {
		q.Set("page", strconv.Itoa(page))
		var raw []struct {
			Name   string `json:"name"`
			Commit struct {
				Sha string `json:"sha"`
			} `json:"commit"`
		}
		hdr, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/branches", q, &raw)
		if !ok {
			return
		}
		for _, b := range raw {
			out = append(out, gitBranch{Name: b.Name, IsDefault: b.Name == info.DefaultBranch, CommitSha: b.Commit.Sha})
		}
		if ghNextPage(hdr) == 0 {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IsDefault && !out[j].IsDefault })
	h.conns.audit(userID, conn.ID, "branches", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type ghCommit struct {
	Sha     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Parents []struct {
		Sha string `json:"sha"`
	} `json:"parents"`
	Files []ghFile `json:"files"`
}

type ghFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Patch            string `json:"patch"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

func (g ghCommit) toDTO() gitRemoteCommit {
	title := g.Commit.Message
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = title[:i]
	}
	short := g.Sha
	if len(short) > 8 {
		short = short[:8]
	}
	parents := make([]string, 0, len(g.Parents))
	for _, p := range g.Parents {
		parents = append(parents, p.Sha)
	}
	return gitRemoteCommit{
		Sha: g.Sha, ShortSha: short, Message: g.Commit.Message, Title: strings.TrimSpace(title),
		AuthorName: g.Commit.Author.Name, AuthorEmail: g.Commit.Author.Email, AuthoredAt: g.Commit.Author.Date,
		Parents: parents, WebURL: g.HTMLURL,
	}
}

func mapGitHubFiles(files []ghFile) []gitFileDiff {
	out := make([]gitFileDiff, 0, len(files))
	for _, f := range files {
		old := f.Filename
		if f.PreviousFilename != "" {
			old = f.PreviousFilename
		}
		status := map[string]string{"added": "added", "removed": "deleted", "renamed": "renamed", "copied": "added"}[f.Status]
		if status == "" {
			status = "modified"
		}
		out = append(out, gitFileDiff{
			OldPath: old, NewPath: f.Filename, Status: status, Renamed: f.Status == "renamed",
			Binary: f.Patch == "" && f.Additions+f.Deletions > 0, Patch: f.Patch,
			Additions: f.Additions, Deletions: f.Deletions,
		})
	}
	return out
}

func (h *GitRemoteHandler) ghCommits(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	page := strings.TrimSpace(c.Query("page"))
	if page == "" {
		page = "1"
	}
	q := url.Values{"per_page": {"30"}, "page": {page}}
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("sha", ref)
	}
	var raw []ghCommit
	hdr, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/commits", q, &raw)
	if !ok {
		return
	}
	commits := make([]gitRemoteCommit, 0, len(raw))
	for _, g := range raw {
		commits = append(commits, g.toDTO())
	}
	h.conns.audit(userID, conn.ID, "commits", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"commits": commits, "nextPage": ghNextPage(hdr)}})
}

func (h *GitRemoteHandler) ghCommitDetail(c *gin.Context, userID string, conn *models.GitConnection, token, project, sha string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var g ghCommit
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/commits/"+url.PathEscape(sha), nil, &g); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "commit", true, gin.H{"project": project, "sha": sha})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"commit": g.toDTO(), "diff": mapGitHubFiles(g.Files)}})
}

// ghContentsPath escapes each path segment (keeping the slashes).
func ghContentsPath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (h *GitRemoteHandler) ghTree(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	q := url.Values{}
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("ref", ref)
	}
	path := strings.Trim(c.Query("path"), "/")
	endpoint := ghRepoPath(info) + "/contents"
	if path != "" {
		endpoint += "/" + ghContentsPath(path)
	}
	var raw []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Sha  string `json:"sha"`
	}
	if _, ok := h.ghJSON(c, conn, token, endpoint, q, &raw); !ok {
		return
	}
	out := make([]gitTreeEntry, 0, len(raw))
	for _, e := range raw {
		t := "blob"
		switch e.Type {
		case "dir":
			t = "tree"
		case "submodule":
			t = "commit"
		}
		out = append(out, gitTreeEntry{Name: e.Name, Path: e.Path, Type: t, Sha: e.Sha})
	}
	// GitLab lists folders first; keep the same order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Type == "tree" && out[j].Type != "tree" })
	h.conns.audit(userID, conn.ID, "tree", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GitRemoteHandler) ghFileContent(c *gin.Context, userID string, conn *models.GitConnection, token, project, path string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	q := url.Values{}
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("ref", ref)
	}
	endpoint := ghRepoPath(info) + "/contents/" + ghContentsPath(path)
	var raw struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
		Sha      string `json:"sha"`
		Type     string `json:"type"`
	}
	if _, ok := h.ghJSON(c, conn, token, endpoint, q, &raw); !ok {
		return
	}
	content := strings.ReplaceAll(raw.Content, "\n", "")
	if content == "" && raw.Size > 0 {
		// Over 1 MB GitHub leaves content empty: fetch the raw bytes.
		u := h.ghBase(conn) + endpoint
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		body, _, status, err := ghRequest(h.http, http.MethodGet, u, token, "application/vnd.github.raw+json")
		if err != nil || status/100 != 2 {
			c.JSON(http.StatusBadGateway, gin.H{"error": "github: could not read the file"})
			return
		}
		content = base64.StdEncoding.EncodeToString(body)
	}
	h.conns.audit(userID, conn.ID, "file", true, gin.H{"project": project, "path": path})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"path": raw.Path, "content": content, "encoding": "base64", "size": raw.Size, "blobSha": raw.Sha,
	}})
}

func (h *GitRemoteHandler) ghCompare(c *gin.Context, userID string, conn *models.GitConnection, token, project, from, to string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var cmp struct {
		AheadBy  int      `json:"ahead_by"`
		BehindBy int      `json:"behind_by"`
		Files    []ghFile `json:"files"`
	}
	path := ghRepoPath(info) + "/compare/" + url.PathEscape(from) + "..." + url.PathEscape(to)
	if _, ok := h.ghJSON(c, conn, token, path, nil, &cmp); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "compare", true, gin.H{"project": project, "from": from, "to": to})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ahead": cmp.AheadBy, "behind": cmp.BehindBy, "diffs": mapGitHubFiles(cmp.Files)}})
}

func (h *GitRemoteHandler) ghContributors(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var raw []struct {
		Login         string `json:"login"`
		Contributions int    `json:"contributions"`
	}
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/contributors", url.Values{"per_page": {"100"}}, &raw); !ok {
		return
	}
	out := make([]gitContributor, 0, len(raw))
	for _, r := range raw {
		out = append(out, gitContributor{Name: r.Login, Commits: r.Contributions})
	}
	h.conns.audit(userID, conn.ID, "contributors", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// probeGitHubPAT checks a token against GitHub: GET /user → the login.
func probeGitHubPAT(client *http.Client, baseURL, token string) (string, error) {
	base := gitHubAPIBase(baseURL)
	if gitHubAPIBaseOverride != "" {
		base = gitHubAPIBaseOverride
	}
	body, _, status, err := ghRequest(client, http.MethodGet, base+"/user", token, "")
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", errors.New("GitHub rejected the token (401) — check it hasn't expired")
	}
	if status/100 != 2 {
		return "", fmt.Errorf("github %d", status)
	}
	var u struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(body, &u)
	return u.Login, nil
}
