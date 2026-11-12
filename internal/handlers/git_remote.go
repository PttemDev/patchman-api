package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// gitLinkGitLabMsg is the exact contract string every remote-read handler
// returns (with 412) when the caller has no usable GitLab token. Remote reads
// authenticate as the CALLER's own linked account, so a grantee who never
// linked GitLab gets this instead of a raw 500.
const gitLinkGitLabMsg = "Link your GitLab account in Settings to browse repositories"

// GitRemoteHandler serves the remote-read surface for the Git tool — a thin
// proxy over GitLab REST v4. Every call authenticates as the CALLER's linked
// GitLab account (GitIntegrationHandler.validAccessToken), NOT the connection
// owner, so delegated grantees browse with their own token. The connection
// :id is access-checked (owner or grant) before any GitLab call.
//
// Calls go to the GitLab the token belongs to (apiBase): a PAT connection's
// own Base URL, otherwise the configured instance (cfg.GitLabBaseURL), whose
// OAuth token must never be sent anywhere else. Without a PAT and with GitLab
// not configured, integ is nil and every route returns the 412 link message.
type GitRemoteHandler struct {
	db    *gorm.DB
	cfg   *config.Config
	conns *GitConnectionHandler
	integ *GitIntegrationHandler // may be nil when GitLab is disabled
	http  *http.Client
}

func NewGitRemoteHandler(db *gorm.DB, cfg *config.Config, conns *GitConnectionHandler, integ *GitIntegrationHandler) *GitRemoteHandler {
	return &GitRemoteHandler{
		db:    db,
		cfg:   cfg,
		conns: conns,
		integ: integ,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// ── DTOs (exact wire shapes — see GIT_V1_CONTRACT.md §3) ─────────────

type gitRepoSummary struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Path           string `json:"path"`
	FullPath       string `json:"fullPath"`
	DefaultBranch  string `json:"defaultBranch"`
	HTTPURL        string `json:"httpUrl"`
	LastActivityAt string `json:"lastActivityAt"`
	Visibility     string `json:"visibility"`
}

// branchPageLimit caps the branch list at 2000 branches (100 per page).
const branchPageLimit = 20

type gitBranch struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	CommitSha string `json:"commitSha"`
}

type gitRemoteCommit struct {
	Sha         string   `json:"sha"`
	ShortSha    string   `json:"shortSha"`
	Message     string   `json:"message"`
	Title       string   `json:"title"`
	AuthorName  string   `json:"authorName"`
	AuthorEmail string   `json:"authorEmail"`
	AuthoredAt  string   `json:"authoredAt"`
	Parents     []string `json:"parents"`
	WebURL      string   `json:"webUrl,omitempty"`
}

type gitTreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode,omitempty"`
	Sha  string `json:"sha,omitempty"`
}

type gitFileDiff struct {
	OldPath   string `json:"oldPath"`
	NewPath   string `json:"newPath"`
	Status    string `json:"status"`
	Renamed   bool   `json:"renamed"`
	Binary    bool   `json:"binary"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type gitContributor struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	Commits   int    `json:"commits"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type gitMergeRequest struct {
	IID          int64  `json:"iid"`
	Title        string `json:"title"`
	State        string `json:"state"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	Author       string `json:"author"`
	WebURL       string `json:"webUrl"`
	CreatedAt    string `json:"createdAt"`
}

// glDiffRefs are the base/head/start SHAs GitLab attaches to an MR — needed to
// anchor inline review comments and to pin the exact diff an LLM reviewed.
type glDiffRefs struct {
	BaseSha  string `json:"base_sha"`
	HeadSha  string `json:"head_sha"`
	StartSha string `json:"start_sha"`
}

func (r glDiffRefs) toDTO() gin.H {
	return gin.H{"baseSha": r.BaseSha, "headSha": r.HeadSha, "startSha": r.StartSha}
}

// ── GitLab raw shapes ───────────────────────────────────────────────

type glCommit struct {
	ID           string   `json:"id"`
	ShortID      string   `json:"short_id"`
	Title        string   `json:"title"`
	Message      string   `json:"message"`
	AuthorName   string   `json:"author_name"`
	AuthorEmail  string   `json:"author_email"`
	AuthoredDate string   `json:"authored_date"`
	ParentIDs    []string `json:"parent_ids"`
	WebURL       string   `json:"web_url"`
}

func (g glCommit) toDTO() gitRemoteCommit {
	parents := make([]string, 0, len(g.ParentIDs))
	parents = append(parents, g.ParentIDs...)
	return gitRemoteCommit{
		Sha: g.ID, ShortSha: g.ShortID, Message: g.Message, Title: g.Title,
		AuthorName: g.AuthorName, AuthorEmail: g.AuthorEmail, AuthoredAt: g.AuthoredDate,
		Parents: parents, WebURL: g.WebURL,
	}
}

type glDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
}

func mapGitLabDiffs(raw []glDiff) []gitFileDiff {
	out := make([]gitFileDiff, 0, len(raw))
	for _, d := range raw {
		status := "modified"
		switch {
		case d.NewFile:
			status = "added"
		case d.DeletedFile:
			status = "deleted"
		case d.RenamedFile:
			status = "renamed"
		}
		add, del := countDiffStats(d.Diff)
		out = append(out, gitFileDiff{
			OldPath: d.OldPath, NewPath: d.NewPath, Status: status,
			Renamed: d.RenamedFile,
			Binary:  d.Diff == "" && !d.NewFile && !d.DeletedFile && !d.RenamedFile,
			Patch:   d.Diff, Additions: add, Deletions: del,
		})
	}
	return out
}

// countDiffStats tallies +/- lines in a unified diff, ignoring the +++/---
// file headers.
func countDiffStats(patch string) (int, int) {
	if patch == "" {
		return 0, 0
	}
	add, del := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			add++
		case strings.HasPrefix(line, "-"):
			del++
		}
	}
	return add, del
}

// ── handlers ────────────────────────────────────────────────────────

// Repos lists projects the caller is a member of. GET .../repos?search=
func (h *GitRemoteHandler) Repos(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("membership", "true")
	q.Set("per_page", "50")
	q.Set("order_by", "last_activity_at")
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		q.Set("search", s)
	}
	var raw []struct {
		ID                int64  `json:"id"`
		Name              string `json:"name"`
		Path              string `json:"path"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		HTTPURLToRepo     string `json:"http_url_to_repo"`
		LastActivityAt    string `json:"last_activity_at"`
		Visibility        string `json:"visibility"`
	}
	if _, ok := h.getJSON(c, token, "/projects", q, &raw); !ok {
		return
	}
	out := make([]gitRepoSummary, 0, len(raw))
	for _, p := range raw {
		out = append(out, gitRepoSummary{
			ID: p.ID, Name: p.Name, Path: p.Path, FullPath: p.PathWithNamespace,
			DefaultBranch: p.DefaultBranch, HTTPURL: p.HTTPURLToRepo,
			LastActivityAt: p.LastActivityAt, Visibility: p.Visibility,
		})
	}
	h.conns.audit(userID, conn.ID, "repos", true, gin.H{"count": len(out)})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// Branches lists a project's branches. GET .../branches?project=
func (h *GitRemoteHandler) Branches(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	// Every page (up to branchPageLimit × 100): GitLab sorts by name with
	// upper case first, so a single page of a repo full of feature branches
	// can leave out main. The default branch leads the list.
	q := url.Values{}
	q.Set("per_page", "100")
	out := []gitBranch{}
	for page := 1; page <= branchPageLimit; page++ {
		q.Set("page", strconv.Itoa(page))
		var raw []struct {
			Name    string `json:"name"`
			Default bool   `json:"default"`
			Commit  struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		hdr, ok := h.getJSON(c, token, projectPath(project)+"/repository/branches", q, &raw)
		if !ok {
			return
		}
		for _, b := range raw {
			out = append(out, gitBranch{Name: b.Name, IsDefault: b.Default, CommitSha: b.Commit.ID})
		}
		if headerInt(hdr, "X-Next-Page") == 0 {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IsDefault && !out[j].IsDefault })
	h.conns.audit(userID, conn.ID, "branches", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// Commits pages a project's commit list (parents included for the DAG).
// GET .../commits?project=&ref=&page=
func (h *GitRemoteHandler) Commits(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "30")
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("ref_name", ref)
	}
	page := strings.TrimSpace(c.Query("page"))
	if page == "" {
		page = "1"
	}
	q.Set("page", page)
	var raw []glCommit
	hdr, ok := h.getJSON(c, token, projectPath(project)+"/repository/commits", q, &raw)
	if !ok {
		return
	}
	commits := make([]gitRemoteCommit, 0, len(raw))
	for _, g := range raw {
		commits = append(commits, g.toDTO())
	}
	h.conns.audit(userID, conn.ID, "commits", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"commits":  commits,
		"nextPage": headerInt(hdr, "X-Next-Page"),
	}})
}

// Commit returns a single commit + its file diffs.
// GET .../commit?project=&sha=
func (h *GitRemoteHandler) Commit(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	sha := strings.TrimSpace(c.Query("sha"))
	if sha == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sha is required"})
		return
	}
	var meta glCommit
	if _, ok := h.getJSON(c, token, projectPath(project)+"/repository/commits/"+url.PathEscape(sha), nil, &meta); !ok {
		return
	}
	var rawDiff []glDiff
	if _, ok := h.getJSON(c, token, projectPath(project)+"/repository/commits/"+url.PathEscape(sha)+"/diff", nil, &rawDiff); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "commit", true, gin.H{"project": project, "sha": sha})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"commit": meta.toDTO(),
		"diff":   mapGitLabDiffs(rawDiff),
	}})
}

// Tree lists a directory at a ref. GET .../tree?project=&ref=&path=
func (h *GitRemoteHandler) Tree(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "100")
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("ref", ref)
	}
	if path := c.Query("path"); path != "" {
		q.Set("path", path)
	}
	var raw []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/repository/tree", q, &raw); !ok {
		return
	}
	out := make([]gitTreeEntry, 0, len(raw))
	for _, e := range raw {
		out = append(out, gitTreeEntry{Name: e.Name, Path: e.Path, Type: e.Type, Mode: e.Mode, Sha: e.ID})
	}
	h.conns.audit(userID, conn.ID, "tree", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// File returns a single blob (base64) at a ref. GET .../file?project=&ref=&path=
func (h *GitRemoteHandler) File(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	path := strings.TrimSpace(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	q := url.Values{}
	if ref := strings.TrimSpace(c.Query("ref")); ref != "" {
		q.Set("ref", ref)
	}
	var raw struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
		BlobID   string `json:"blob_id"`
	}
	endpoint := projectPath(project) + "/repository/files/" + url.PathEscape(path)
	if _, ok := h.getJSON(c, token, endpoint, q, &raw); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "file", true, gin.H{"project": project, "path": path})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"path": raw.FilePath, "content": raw.Content, "encoding": raw.Encoding,
		"size": raw.Size, "blobSha": raw.BlobID,
	}})
}

// Compare returns ahead/behind counts + diffs between two refs.
// GET .../compare?project=&from=&to=
func (h *GitRemoteHandler) Compare(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	from := strings.TrimSpace(c.Query("from"))
	to := strings.TrimSpace(c.Query("to"))
	if from == "" || to == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from and to are required"})
		return
	}
	fq := url.Values{}
	fq.Set("from", from)
	fq.Set("to", to)
	var fwd struct {
		Commits []glCommit `json:"commits"`
		Diffs   []glDiff   `json:"diffs"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/repository/compare", fq, &fwd); !ok {
		return
	}
	// behind = commits reachable from `from` but not `to`. Best-effort reverse
	// compare; a failure just yields 0 rather than failing the whole call.
	behind := 0
	rq := url.Values{}
	rq.Set("from", to)
	rq.Set("to", from)
	if body, _, status, err := h.gitlabGET(c, token, projectPath(project)+"/repository/compare", rq); err == nil && status/100 == 2 {
		var rev struct {
			Commits []glCommit `json:"commits"`
		}
		if json.Unmarshal(body, &rev) == nil {
			behind = len(rev.Commits)
		}
	}
	h.conns.audit(userID, conn.ID, "compare", true, gin.H{"project": project, "from": from, "to": to})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ahead":  len(fwd.Commits),
		"behind": behind,
		"diffs":  mapGitLabDiffs(fwd.Diffs),
	}})
}

// Contributors returns an insight-lite contributor list.
// GET .../contributors?project=
func (h *GitRemoteHandler) Contributors(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	var raw []struct {
		Name      string `json:"name"`
		Email     string `json:"email"`
		Commits   int    `json:"commits"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/repository/contributors", nil, &raw); !ok {
		return
	}
	out := make([]gitContributor, 0, len(raw))
	for _, r := range raw {
		out = append(out, gitContributor{
			Name: r.Name, Email: r.Email, Commits: r.Commits,
			Additions: r.Additions, Deletions: r.Deletions,
		})
	}
	h.conns.audit(userID, conn.ID, "contributors", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// MergeRequests lists a project's MRs (read). GET .../merge_requests?project=&state=
func (h *GitRemoteHandler) MergeRequests(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "50")
	if state := strings.TrimSpace(c.Query("state")); state != "" {
		q.Set("state", state)
	}
	var raw []struct {
		IID          int64  `json:"iid"`
		Title        string `json:"title"`
		State        string `json:"state"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		WebURL       string `json:"web_url"`
		CreatedAt    string `json:"created_at"`
		Author       struct {
			Name string `json:"name"`
		} `json:"author"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/merge_requests", q, &raw); !ok {
		return
	}
	out := make([]gitMergeRequest, 0, len(raw))
	for _, m := range raw {
		out = append(out, gitMergeRequest{
			IID: m.IID, Title: m.Title, State: m.State, SourceBranch: m.SourceBranch,
			TargetBranch: m.TargetBranch, Author: m.Author.Name, WebURL: m.WebURL,
			CreatedAt: m.CreatedAt,
		})
	}
	h.conns.audit(userID, conn.ID, "merge_requests", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// MergeRequest returns a single MR with metadata + diff_refs (read).
// GET .../merge_request?project=&iid=
func (h *GitRemoteHandler) MergeRequest(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	iid, ok := requireIID(c)
	if !ok {
		return
	}
	var raw struct {
		IID          int64      `json:"iid"`
		Title        string     `json:"title"`
		Description  string     `json:"description"`
		State        string     `json:"state"`
		SourceBranch string     `json:"source_branch"`
		TargetBranch string     `json:"target_branch"`
		WebURL       string     `json:"web_url"`
		CreatedAt    string     `json:"created_at"`
		UpdatedAt    string     `json:"updated_at"`
		Draft        bool       `json:"draft"`
		MergeStatus  string     `json:"merge_status"`
		HasConflicts bool       `json:"has_conflicts"`
		SHA          string     `json:"sha"`
		Author       struct{ Name string `json:"name"` } `json:"author"`
		DiffRefs     glDiffRefs `json:"diff_refs"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/merge_requests/"+iid, nil, &raw); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "merge_request", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"iid": raw.IID, "title": raw.Title, "description": raw.Description, "state": raw.State,
		"sourceBranch": raw.SourceBranch, "targetBranch": raw.TargetBranch, "author": raw.Author.Name,
		"webUrl": raw.WebURL, "createdAt": raw.CreatedAt, "updatedAt": raw.UpdatedAt,
		"draft": raw.Draft, "mergeStatus": raw.MergeStatus, "hasConflicts": raw.HasConflicts,
		"sha": raw.SHA, "diffRefs": raw.DiffRefs.toDTO(),
	}})
}

// MergeRequestChanges returns an MR's per-file diffs + diff_refs (read). This is
// the surface an LLM reviews. GET .../merge_request_changes?project=&iid=
func (h *GitRemoteHandler) MergeRequestChanges(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	iid, ok := requireIID(c)
	if !ok {
		return
	}
	var raw struct {
		Changes  []glDiff   `json:"changes"`
		DiffRefs glDiffRefs `json:"diff_refs"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/merge_requests/"+iid+"/changes", nil, &raw); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "merge_request_changes", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"diffs":    mapGitLabDiffs(raw.Changes),
		"diffRefs": raw.DiffRefs.toDTO(),
	}})
}

// MergeRequestNotes lists an MR's comments/notes (read).
// GET .../merge_request_notes?project=&iid=
func (h *GitRemoteHandler) MergeRequestNotes(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	iid, ok := requireIID(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("sort", "asc")
	q.Set("order_by", "created_at")
	var raw []struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		System    bool   `json:"system"`
		CreatedAt string `json:"created_at"`
		Author    struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"author"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/merge_requests/"+iid+"/notes", q, &raw); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, n := range raw {
		out = append(out, gin.H{
			"id": n.ID, "body": n.Body, "system": n.System,
			"author": n.Author.Name, "username": n.Author.Username, "createdAt": n.CreatedAt,
		})
	}
	h.conns.audit(userID, conn.ID, "merge_request_notes", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// CreateMRNote posts a review comment on an MR (WRITE — needs a write grant on
// the connection). POST .../merge_request_notes?project=&iid=  body {body}
func (h *GitRemoteHandler) CreateMRNote(c *gin.Context) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	iid, ok := requireIID(c)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is required"})
		return
	}
	body, status, err := h.gitlabDo(c, token, http.MethodPost, projectPath(project)+"/merge_requests/"+iid+"/notes", gin.H{"body": in.Body})
	if err != nil {
		h.conns.audit(userID, conn.ID, "merge_request_note_create", false, gin.H{"project": project, "iid": iid})
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if status/100 != 2 {
		h.conns.audit(userID, conn.ID, "merge_request_note_create", false, gin.H{"project": project, "iid": iid, "status": status})
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("gitlab %d: %s", status, strings.TrimSpace(string(body)))})
		return
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &created)
	h.conns.audit(userID, conn.ID, "merge_request_note_create", true, gin.H{"project": project, "iid": iid, "noteId": created.ID})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": created.ID, "iid": iid, "posted": true}})
}

// ApproveMR approves an MR (WRITE — needs a write grant). Requires GitLab's MR
// Approvals feature; on editions without it GitLab returns 404, surfaced as-is.
// POST .../merge_request_approve?project=&iid=
func (h *GitRemoteHandler) ApproveMR(c *gin.Context) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	iid, ok := requireIID(c)
	if !ok {
		return
	}
	body, status, err := h.gitlabDo(c, token, http.MethodPost, projectPath(project)+"/merge_requests/"+iid+"/approve", nil)
	if err != nil {
		h.conns.audit(userID, conn.ID, "merge_request_approve", false, gin.H{"project": project, "iid": iid})
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if status/100 != 2 {
		h.conns.audit(userID, conn.ID, "merge_request_approve", false, gin.H{"project": project, "iid": iid, "status": status})
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("gitlab %d: %s", status, strings.TrimSpace(string(body)))})
		return
	}
	h.conns.audit(userID, conn.ID, "merge_request_approve", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"iid": iid, "approved": true}})
}

// ── internals ───────────────────────────────────────────────────────

// auth resolves the connection (access-checked) + the caller's GitLab token.
// Returns ok=false with the response already written on any failure — a 404 for
// an inaccessible connection, or the 412 link message when the caller has no
// usable token.
func (h *GitRemoteHandler) auth(c *gin.Context) (userID string, conn *models.GitConnection, token string, ok bool) {
	userID = c.GetString("userID")
	cn, _, err := h.conns.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return "", nil, "", false
	}
	tok, tokOK := h.connToken(c, cn, userID)
	if !tokOK {
		return "", nil, "", false
	}
	return userID, cn, tok, true
}

// connToken resolves the bearer token for a remote call. A PAT connection uses
// its OWN stored personal access token — which needs no OAuth flow, so offline /
// local-mode users (whose embedded backend has no GitLab OAuth config, and no
// browser round-trip) can still browse repos and review MRs. Otherwise it falls
// back to the caller's linked GitLab OAuth account. h.conns is in the same
// package, so its cipher decrypts the stored PAT here.
func (h *GitRemoteHandler) connToken(c *gin.Context, conn *models.GitConnection, userID string) (string, bool) {
	if strings.EqualFold(strings.TrimSpace(conn.AuthKind), "pat") && conn.CredentialEnc != "" {
		if pat, err := h.conns.cipher.Decrypt(conn.CredentialEnc); err == nil && strings.TrimSpace(pat) != "" {
			// The PAT belongs to the connection's GitLab, which need not be
			// the configured one (another company's instance, say).
			if base := strings.TrimRight(strings.TrimSpace(conn.BaseURL), "/"); base != "" {
				c.Set(gitlabAPIBaseKey, base+"/api/v4")
			}
			return strings.TrimSpace(pat), true
		}
	}
	return h.tokenOr412(c, userID)
}

// tokenOr412 returns the caller's valid GitLab access token, or writes the exact
// 412 contract response and returns ok=false.
func (h *GitRemoteHandler) tokenOr412(c *gin.Context, userID string) (string, bool) {
	if h.integ == nil {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": gitLinkGitLabMsg})
		return "", false
	}
	tok, err := h.integ.validAccessToken(userID)
	if err != nil || strings.TrimSpace(tok) == "" {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": gitLinkGitLabMsg})
		return "", false
	}
	return tok, true
}

// authWrite is auth for mutating remote calls (post note, approve): it requires
// a WRITE grant (owner/write role) on the connection via conns.access, then the
// caller's GitLab token. Writes the 404/403/412 response and returns ok=false on
// any failure. GitLab enforces its own project permissions on top of this.
func (h *GitRemoteHandler) authWrite(c *gin.Context) (userID string, conn *models.GitConnection, token string, ok bool) {
	userID = c.GetString("userID")
	cn, _, aok := h.conns.access(c, true)
	if !aok {
		return "", nil, "", false
	}
	tok, tokOK := h.connToken(c, cn, userID)
	if !tokOK {
		return "", nil, "", false
	}
	return userID, cn, tok, true
}

// gitlabAPIBaseKey carries, per request, the REST root connToken picked.
const gitlabAPIBaseKey = "gitlabAPIBase"

// apiBase is the REST root of the GitLab the request's token belongs to: set
// by connToken for a PAT connection, otherwise the configured instance.
func (h *GitRemoteHandler) apiBase(c *gin.Context) string {
	if v, ok := c.Get(gitlabAPIBaseKey); ok {
		if base, _ := v.(string); base != "" {
			return base
		}
	}
	return strings.TrimRight(h.cfg.GitLabBaseURL, "/") + "/api/v4"
}

// gitlabDo performs an authenticated write (POST/PUT/DELETE) with an optional
// JSON body and returns the raw body + status. Mirrors gitlabGET; used by the
// MR comment/approve handlers (the getJSON path is GET/decode-only).
func (h *GitRemoteHandler) gitlabDo(c *gin.Context, token, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.apiBase(c)+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// gitlabGET performs an authenticated GET and returns the raw body + header +
// status. Path is appended verbatim (callers pre-escape project/file segments,
// as git_push.go does); query is encoded from url.Values.
func (h *GitRemoteHandler) gitlabGET(c *gin.Context, token, path string, q url.Values) ([]byte, http.Header, int, error) {
	endpoint := h.apiBase(c) + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.Header, resp.StatusCode, nil
}

// getJSON runs gitlabGET and decodes a 2xx body into out. Non-2xx / transport /
// decode failures write a 502 and return ok=false.
func (h *GitRemoteHandler) getJSON(c *gin.Context, token, path string, q url.Values, out any) (http.Header, bool) {
	body, hdr, status, err := h.gitlabGET(c, token, path, q)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	if status/100 != 2 {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("gitlab %d: %s", status, strings.TrimSpace(string(body)))})
		return nil, false
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "decode gitlab response: " + err.Error()})
			return nil, false
		}
	}
	return hdr, true
}

// projectPath builds the /projects/<escaped-ref> prefix. The ref may be a
// numeric id or a url-encoded "group/repo" path — PathEscape turns "/" into
// %2F, which GitLab requires and net/http preserves (RawPath).
func projectPath(project string) string {
	return "/projects/" + url.PathEscape(project)
}

func requireProject(c *gin.Context) (string, bool) {
	p := strings.TrimSpace(c.Query("project"))
	if p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project is required"})
		return "", false
	}
	return p, true
}

// requireIID reads the MR iid query param, validates it's a positive integer,
// and returns it as a clean string safe to append to the GitLab path.
func requireIID(c *gin.Context) (string, bool) {
	raw := strings.TrimSpace(c.Query("iid"))
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iid is required"})
		return "", false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iid must be a positive integer"})
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

func headerInt(h http.Header, key string) int {
	if h == nil {
		return 0
	}
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
