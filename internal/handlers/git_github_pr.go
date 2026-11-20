package handlers

// GitHub pull requests (provider phase 3): every merge-request route of the
// Git tab answered from GitHub in the GitLab shapes the FE already renders.
//
// Vocabulary mapping (GitHub → what the FE expects):
//   - state: open → opened; closed + merged_at → merged; closed → closed.
//   - mergeStatus: mergeable_state behind → need_rebase, clean → can_be_merged,
//     mergeable null → checking (GitHub is still computing; not a conflict).
//   - pipeline: check runs (+ legacy commit statuses) folded into one entry
//     with GitLab's status words (success / failed / running / pending / canceled).
//   - discussions: review threads (GraphQL, for the thread id + resolved state)
//     + PR conversation comments and review summaries as unresolvable threads.
//   - approvals: the latest review per user; GitHub has no "approvals left".
//   - reviewer / assignee ids: GitHub user ids from the members list → logins.
//
// Things REST can't do (draft toggle, auto-merge, resolve, rebase update) go
// through GraphQL: github.com → https://api.github.com/graphql, Enterprise
// Server → <base>/api/graphql.

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

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// ── transport ───────────────────────────────────────────────────────────────

// ghGraphQLURL maps a connection to its GraphQL endpoint.
func (h *GitRemoteHandler) ghGraphQLURL(conn *models.GitConnection) string {
	base := h.ghBase(conn)
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return base + "/graphql"
}

// ghDo sends a JSON body (nil = none) to a GitHub REST path.
func (h *GitRemoteHandler) ghDo(conn *models.GitConnection, token, method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ghBase(conn)+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return raw, resp.StatusCode, nil
}

func ghMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		parts := []string{e.Message}
		for _, x := range e.Errors {
			switch {
			case x.Message != "":
				parts = append(parts, x.Message)
			case x.Field != "":
				parts = append(parts, x.Field+" "+x.Code)
			}
		}
		return strings.Join(parts, ": ")
	}
	return strings.TrimSpace(string(raw))
}

// ghWrite mirrors gitlabWrite: one write, audited on success and failure.
func (h *GitRemoteHandler) ghWrite(c *gin.Context, conn *models.GitConnection, token, userID, op, method, path string, body any, detail gin.H) (map[string]any, bool) {
	raw, status, err := h.ghDo(conn, token, method, path, body)
	if err != nil {
		h.conns.audit(userID, conn.ID, op, false, detail)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	if status/100 != 2 {
		detail["status"] = status
		h.conns.audit(userID, conn.ID, op, false, detail)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("github %d: %s", status, ghMessage(raw))})
		return nil, false
	}
	h.conns.audit(userID, conn.ID, op, true, detail)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out, true
}

// ghGraphQL runs one query / mutation; GraphQL errors come back as an error.
func (h *GitRemoteHandler) ghGraphQL(conn *models.GitConnection, token, query string, vars map[string]any, out any) error {
	b, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequest(http.MethodPost, h.ghGraphQLURL(conn), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github graphql %d: %s", resp.StatusCode, ghMessage(raw))
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode github graphql: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("github: %s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// ghGraphQLWrite runs a mutation, audited, writing the error response itself.
func (h *GitRemoteHandler) ghGraphQLWrite(c *gin.Context, conn *models.GitConnection, token, userID, op, query string, vars map[string]any, out any, detail gin.H) bool {
	if err := h.ghGraphQL(conn, token, query, vars, out); err != nil {
		h.conns.audit(userID, conn.ID, op, false, detail)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return false
	}
	h.conns.audit(userID, conn.ID, op, true, detail)
	return true
}

func ghOwnerName(info ghRepoInfo) (string, string) {
	owner, name, _ := strings.Cut(info.FullName, "/")
	return owner, name
}

// ghLogin is the token owner's login (cached per token hash for a while).
func (h *GitRemoteHandler) ghLogin(c *gin.Context, conn *models.GitConnection, token string) (string, bool) {
	key := "login|" + conn.ID // a GitHub connection carries one token
	if v, ok := ghRepoCache.Load(key); ok {
		if e := v.(ghCached); time.Since(e.at) < 10*time.Minute {
			return e.info.FullName, true
		}
	}
	var me struct {
		Login string `json:"login"`
	}
	if _, ok := h.ghJSON(c, conn, token, "/user", nil, &me); !ok {
		return "", false
	}
	ghRepoCache.Store(key, ghCached{info: ghRepoInfo{FullName: me.Login}, at: time.Now()})
	return me.Login, true
}

// ── shapes ──────────────────────────────────────────────────────────────────

type ghUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

type ghPull struct {
	ID        int64   `json:"id"`
	NodeID    string  `json:"node_id"`
	Number    int64   `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Draft     bool    `json:"draft"`
	Merged    bool    `json:"merged"`
	MergedAt  *string `json:"merged_at"`
	HTMLURL   string  `json:"html_url"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	Mergeable *bool   `json:"mergeable"`
	// clean | dirty | behind | blocked | unstable | has_hooks | draft | unknown
	MergeableState string   `json:"mergeable_state"`
	Comments       int      `json:"comments"`
	ReviewComments int      `json:"review_comments"`
	User           ghUser   `json:"user"`
	Assignees      []ghUser `json:"assignees"`
	Reviewers      []ghUser `json:"requested_reviewers"`
	Head           struct {
		Ref  string `json:"ref"`
		Sha  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Sha  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (p ghPull) glState() string {
	switch {
	case p.State == "open":
		return "opened"
	case p.Merged || p.MergedAt != nil:
		return "merged"
	default:
		return "closed"
	}
}

func (p ghPull) mergeStatus() string {
	if p.Mergeable == nil && p.State == "open" {
		return "checking"
	}
	switch p.MergeableState {
	case "behind":
		return "need_rebase"
	case "clean", "has_hooks", "unstable":
		return "can_be_merged"
	case "dirty":
		return "cannot_be_merged"
	case "":
		return "unchecked"
	}
	return p.MergeableState // blocked | draft | unknown
}

func (p ghPull) hasConflicts() bool {
	return p.State == "open" && ((p.Mergeable != nil && !*p.Mergeable) || p.MergeableState == "dirty")
}

func (p ghPull) listDTO() gitMergeRequest {
	return gitMergeRequest{
		IID: p.Number, Title: p.Title, State: p.glState(), SourceBranch: p.Head.Ref, TargetBranch: p.Base.Ref,
		Author: p.User.Login, WebURL: p.HTMLURL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		Draft: p.Draft, HasConflicts: p.hasConflicts(), Notes: p.Comments + p.ReviewComments,
	}
}

func ghPullSummary(m map[string]any) gin.H {
	str := func(k string) string { s, _ := m[k].(string); return s }
	state := str("state")
	if state == "open" {
		state = "opened"
	} else if merged, _ := m["merged"].(bool); merged || m["merged_at"] != nil {
		state = "merged"
	}
	return gin.H{"iid": m["number"], "webUrl": str("html_url"), "state": state, "title": str("title"), "mergeStatus": str("mergeable_state")}
}

// ── reads ───────────────────────────────────────────────────────────────────

func (h *GitRemoteHandler) ghPull(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo, iid string) (ghPull, bool) {
	var p ghPull
	_, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid, nil, &p)
	return p, ok
}

// ghMergeRequests: GitHub's pulls list has no search, "mine" or "merged"
// filter, so the last 100 pulls in the right state are filtered here.
func (h *GitRemoteHandler) ghMergeRequests(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	state := strings.TrimSpace(c.Query("state"))
	q := url.Values{"per_page": {"100"}, "sort": {"updated"}, "direction": {"desc"}}
	switch state {
	case "", "opened":
		q.Set("state", "open")
	case "closed", "merged":
		q.Set("state", "closed")
	default:
		q.Set("state", "all")
	}
	var raw []ghPull
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls", q, &raw); !ok {
		return
	}
	mine := c.Query("mine")
	me := ""
	if mine != "" {
		if me, ok = h.ghLogin(c, conn, token); !ok {
			return
		}
	}
	has := func(us []ghUser) bool {
		for _, u := range us {
			if strings.EqualFold(u.Login, me) {
				return true
			}
		}
		return false
	}
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	out := make([]gitMergeRequest, 0, len(raw))
	for _, p := range raw {
		st := p.glState()
		if (state == "merged" && st != "merged") || (state == "closed" && st != "closed") {
			continue
		}
		switch mine {
		case "created":
			if !strings.EqualFold(p.User.Login, me) {
				continue
			}
		case "assigned":
			if !has(p.Assignees) {
				continue
			}
		case "review":
			if !has(p.Reviewers) {
				continue
			}
		}
		if search != "" && !strings.Contains(strings.ToLower(p.Title), search) && strconv.FormatInt(p.Number, 10) != strings.TrimPrefix(search, "#") {
			continue
		}
		out = append(out, p.listDTO())
		if len(out) == 50 {
			break
		}
	}
	h.conns.audit(userID, conn.ID, "merge_requests", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GitRemoteHandler) ghMergeRequest(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "merge_request", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"iid": p.Number, "title": p.Title, "description": p.Body, "state": p.glState(),
		"sourceBranch": p.Head.Ref, "targetBranch": p.Base.Ref, "author": p.User.Login,
		"webUrl": p.HTMLURL, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
		"draft": p.Draft, "mergeStatus": p.mergeStatus(), "hasConflicts": p.hasConflicts(),
		"sha":      p.Head.Sha,
		"diffRefs": gin.H{"baseSha": p.Base.Sha, "headSha": p.Head.Sha, "startSha": p.Base.Sha},
		"provider": "github",
	}})
}

func (h *GitRemoteHandler) ghMergeRequestChanges(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	// GitHub lists at most 3000 files, 100 per page; 10 pages is plenty here.
	files := []ghFile{}
	for page := 1; page <= 10; page++ {
		var batch []ghFile
		hdr, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid+"/files", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, &batch)
		if !ok {
			return
		}
		files = append(files, batch...)
		if ghNextPage(hdr) == 0 {
			break
		}
	}
	h.conns.audit(userID, conn.ID, "merge_request_changes", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"diffs":    mapGitHubFiles(files),
		"diffRefs": gin.H{"baseSha": p.Base.Sha, "headSha": p.Head.Sha, "startSha": p.Base.Sha},
	}})
}

type ghIssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      ghUser `json:"user"`
}

func (h *GitRemoteHandler) ghIssueComments(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo, iid string) ([]ghIssueComment, bool) {
	var raw []ghIssueComment
	_, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/issues/"+iid+"/comments", url.Values{"per_page": {"100"}}, &raw)
	return raw, ok
}

func (h *GitRemoteHandler) ghMergeRequestNotes(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	raw, ok := h.ghIssueComments(c, conn, token, info, iid)
	if !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, n := range raw {
		out = append(out, gin.H{"id": n.ID, "body": n.Body, "system": false, "author": n.User.Login, "username": n.User.Login, "createdAt": n.CreatedAt})
	}
	h.conns.audit(userID, conn.ID, "merge_request_notes", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ── discussions ─────────────────────────────────────────────────────────────

const ghThreadsQuery = `query($owner:String!,$name:String!,$n:Int!){repository(owner:$owner,name:$name){pullRequest(number:$n){
reviewThreads(first:100){nodes{id isResolved path line originalLine diffSide
comments(first:100){nodes{databaseId body createdAt author{login avatarUrl}}}}}}}}`

type ghThread struct {
	ID           string `json:"id"`
	IsResolved   bool   `json:"isResolved"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"originalLine"`
	DiffSide     string `json:"diffSide"`
	Comments     struct {
		Nodes []struct {
			DatabaseID int64  `json:"databaseId"`
			Body       string `json:"body"`
			CreatedAt  string `json:"createdAt"`
			Author     *struct {
				Login     string `json:"login"`
				AvatarURL string `json:"avatarUrl"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

func (t ghThread) dto() gin.H {
	notes := make([]gin.H, 0, len(t.Comments.Nodes))
	for _, n := range t.Comments.Nodes {
		login, avatar := "ghost", ""
		if n.Author != nil {
			login, avatar = n.Author.Login, n.Author.AvatarURL
		}
		notes = append(notes, gin.H{"id": n.DatabaseID, "body": n.Body, "author": login, "username": login, "avatarUrl": avatar, "createdAt": n.CreatedAt})
	}
	line := t.Line
	if line == nil {
		line = t.OriginalLine // outdated thread: keep it on the line it was written on
	}
	pos := gin.H{"newPath": t.Path, "oldPath": t.Path, "newLine": nil, "oldLine": nil}
	if t.DiffSide == "LEFT" {
		pos["oldLine"] = line
	} else {
		pos["newLine"] = line
	}
	return gin.H{"id": t.ID, "notes": notes, "resolvable": true, "resolved": t.IsResolved, "position": pos}
}

// ghReviewComment is a REST review comment — the fallback when GraphQL is
// unavailable (then threads can't be resolved from here).
type ghReviewComment struct {
	ID           int64  `json:"id"`
	InReplyTo    int64  `json:"in_reply_to_id"`
	Body         string `json:"body"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"original_line"`
	Side         string `json:"side"`
	CreatedAt    string `json:"created_at"`
	User         ghUser `json:"user"`
}

func (h *GitRemoteHandler) ghMRDiscussions(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(iid)
	out := []gin.H{}
	// Conversation comments and review summaries: general, not resolvable.
	comments, ok := h.ghIssueComments(c, conn, token, info, iid)
	if !ok {
		return
	}
	type general struct {
		at string
		d  gin.H
	}
	gen := []general{}
	for _, cm := range comments {
		gen = append(gen, general{cm.CreatedAt, gin.H{"id": "ic-" + strconv.FormatInt(cm.ID, 10), "resolvable": false, "resolved": false,
			"notes": []gin.H{{"id": cm.ID, "body": cm.Body, "author": cm.User.Login, "username": cm.User.Login, "avatarUrl": cm.User.AvatarURL, "createdAt": cm.CreatedAt}}}})
	}
	var reviews []struct {
		ID          int64  `json:"id"`
		Body        string `json:"body"`
		State       string `json:"state"`
		SubmittedAt string `json:"submitted_at"`
		User        ghUser `json:"user"`
	}
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid+"/reviews", url.Values{"per_page": {"100"}}, &reviews); !ok {
		return
	}
	for _, r := range reviews {
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		body := r.Body
		switch r.State {
		case "APPROVED":
			body = "✅ " + body
		case "CHANGES_REQUESTED":
			body = "✋ " + body
		}
		gen = append(gen, general{r.SubmittedAt, gin.H{"id": "rv-" + strconv.FormatInt(r.ID, 10), "resolvable": false, "resolved": false,
			"notes": []gin.H{{"id": r.ID, "body": body, "author": r.User.Login, "username": r.User.Login, "avatarUrl": r.User.AvatarURL, "createdAt": r.SubmittedAt}}}})
	}
	sort.SliceStable(gen, func(i, j int) bool { return gen[i].at < gen[j].at })
	for _, g := range gen {
		out = append(out, g.d)
	}
	// Line threads.
	owner, name := ghOwnerName(info)
	var gq struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []ghThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := h.ghGraphQL(conn, token, ghThreadsQuery, map[string]any{"owner": owner, "name": name, "n": n}, &gq); err == nil {
		for _, t := range gq.Repository.PullRequest.ReviewThreads.Nodes {
			if len(t.Comments.Nodes) > 0 {
				out = append(out, t.dto())
			}
		}
	} else {
		var rc []ghReviewComment
		if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid+"/comments", url.Values{"per_page": {"100"}}, &rc); !ok {
			return
		}
		out = append(out, groupReviewComments(rc)...)
	}
	h.conns.audit(userID, conn.ID, "merge_request_discussions", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// groupReviewComments threads REST review comments by their root comment.
func groupReviewComments(rc []ghReviewComment) []gin.H {
	order := []int64{}
	threads := map[int64][]ghReviewComment{}
	for _, cm := range rc {
		root := cm.ID
		if cm.InReplyTo != 0 {
			root = cm.InReplyTo
		}
		if _, seen := threads[root]; !seen {
			order = append(order, root)
		}
		threads[root] = append(threads[root], cm)
	}
	out := make([]gin.H, 0, len(order))
	for _, root := range order {
		cs := threads[root]
		first := cs[0]
		notes := make([]gin.H, 0, len(cs))
		for _, cm := range cs {
			notes = append(notes, gin.H{"id": cm.ID, "body": cm.Body, "author": cm.User.Login, "username": cm.User.Login, "avatarUrl": cm.User.AvatarURL, "createdAt": cm.CreatedAt})
		}
		line := first.Line
		if line == nil {
			line = first.OriginalLine
		}
		pos := gin.H{"newPath": first.Path, "oldPath": first.Path, "newLine": nil, "oldLine": nil}
		if first.Side == "LEFT" {
			pos["oldLine"] = line
		} else {
			pos["newLine"] = line
		}
		out = append(out, gin.H{"id": "rc-" + strconv.FormatInt(root, 10), "notes": notes, "resolvable": false, "resolved": false, "position": pos})
	}
	return out
}

// ghCreateMRDiscussion: no position → a conversation comment; with one → a
// line comment on the head commit (newLine → RIGHT side, oldLine → LEFT).
func (h *GitRemoteHandler) ghCreateMRDiscussion(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string, in mrDiscussionInput) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	detail := gin.H{"project": project, "iid": iid, "inline": in.Position != nil}
	if in.Position == nil {
		out, ok := h.ghWrite(c, conn, token, userID, "merge_request_discussion", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/comments", gin.H{"body": in.Body}, detail)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": fmt.Sprintf("ic-%v", jsonInt(out["id"]))}})
		return
	}
	p := in.Position
	if p.HeadSha == "" || (p.NewLine == nil && p.OldLine == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "position needs the head sha and a line"})
		return
	}
	body := gin.H{"body": in.Body, "commit_id": p.HeadSha}
	if p.NewLine != nil {
		body["path"], body["line"], body["side"] = p.NewPath, *p.NewLine, "RIGHT"
	} else {
		path := p.OldPath
		if path == "" {
			path = p.NewPath
		}
		body["path"], body["line"], body["side"] = path, *p.OldLine, "LEFT"
	}
	out, ok := h.ghWrite(c, conn, token, userID, "merge_request_discussion", http.MethodPost, ghRepoPath(info)+"/pulls/"+iid+"/comments", body, detail)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": fmt.Sprintf("rc-%v", jsonInt(out["id"]))}})
}

func jsonInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

func (h *GitRemoteHandler) ghDiscussionOp(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid, disc string, resolve bool, body string, resolved bool) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	detail := gin.H{"project": project, "iid": iid, "discussion": disc}
	kind, id, _ := strings.Cut(disc, "-")
	restID := ""
	if kind == "ic" || kind == "rv" || kind == "rc" {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad discussion id"})
			return
		}
		restID = id
	}
	if resolve {
		if restID != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Only review threads on lines can be resolved on GitHub."})
			return
		}
		m := `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`
		if !resolved {
			m = `mutation($id:ID!){unresolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`
		}
		if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_discussion_resolve", m, map[string]any{"id": disc}, nil, detail) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"resolved": resolved}})
		return
	}
	if strings.TrimSpace(body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is required"})
		return
	}
	switch kind {
	case "ic", "rv":
		// Conversation comments have no replies on GitHub: answer in the conversation.
		out, ok := h.ghWrite(c, conn, token, userID, "merge_request_discussion_reply", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/comments", gin.H{"body": body}, detail)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": jsonInt(out["id"])}})
	case "rc":
		out, ok := h.ghWrite(c, conn, token, userID, "merge_request_discussion_reply", http.MethodPost, ghRepoPath(info)+"/pulls/"+iid+"/comments/"+restID+"/replies", gin.H{"body": body}, detail)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": jsonInt(out["id"])}})
	default:
		var res struct {
			Add struct {
				Comment struct {
					DatabaseID int64 `json:"databaseId"`
				} `json:"comment"`
			} `json:"addPullRequestReviewThreadReply"`
		}
		m := `mutation($id:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$id,body:$body}){comment{databaseId}}}`
		if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_discussion_reply", m, map[string]any{"id": disc, "body": body}, &res, detail) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": res.Add.Comment.DatabaseID}})
	}
}

// ── reviews / checks / members ──────────────────────────────────────────────

type ghReview struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	User  ghUser `json:"user"`
}

// ghLatestReviews keeps each user's latest deciding review (COMMENTED doesn't
// change an earlier approval).
func ghLatestReviews(rs []ghReview) map[string]ghReview {
	latest := map[string]ghReview{}
	for _, r := range rs {
		if r.State == "COMMENTED" || r.State == "PENDING" {
			continue
		}
		latest[strings.ToLower(r.User.Login)] = r
	}
	return latest
}

func (h *GitRemoteHandler) ghReviews(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo, iid string) ([]ghReview, bool) {
	var rs []ghReview
	_, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid+"/reviews", url.Values{"per_page": {"100"}}, &rs)
	return rs, ok
}

func (h *GitRemoteHandler) ghMRApprovals(c *gin.Context, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	rs, ok := h.ghReviews(c, conn, token, info, iid)
	if !ok {
		return
	}
	me, ok := h.ghLogin(c, conn, token)
	if !ok {
		return
	}
	by := []string{}
	changes := []string{}
	mine := false
	latest := ghLatestReviews(rs)
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := latest[k]
		switch r.State {
		case "APPROVED":
			by = append(by, r.User.Login)
			if strings.EqualFold(r.User.Login, me) {
				mine = true
			}
		case "CHANGES_REQUESTED":
			changes = append(changes, r.User.Login)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"approved": len(by) > 0 && len(changes) == 0, "required": 0, "left": 0,
		"userHasApproved": mine, "userCanApprove": !strings.EqualFold(p.User.Login, me) && !mine,
		"approvedBy": by, "changesRequestedBy": changes,
	}})
}

// ghCheckStatus folds check runs + legacy statuses into GitLab's words.
func ghCheckStatus(runs []struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}, combined string) string {
	failed, running, pending, canceled, success := false, false, false, false, 0
	for _, r := range runs {
		switch r.Status {
		case "queued", "waiting", "requested", "pending":
			pending = true
			continue
		case "in_progress":
			running = true
			continue
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
			success++
		case "cancelled", "stale":
			canceled = true
		default: // failure, timed_out, action_required, startup_failure
			failed = true
		}
	}
	switch combined {
	case "failure", "error":
		failed = true
	case "pending":
		pending = true
	case "success":
		success++
	}
	switch {
	case failed:
		return "failed"
	case running:
		return "running"
	case pending:
		return "pending"
	case canceled:
		return "canceled"
	case success > 0:
		return "success"
	}
	return ""
}

func (h *GitRemoteHandler) ghMRPipelines(c *gin.Context, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	var cr struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/commits/"+p.Head.Sha+"/check-runs", url.Values{"per_page": {"100"}}, &cr); !ok {
		return
	}
	var st struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	combined := ""
	if body, _, status, err := h.ghGET(conn, token, ghRepoPath(info)+"/commits/"+p.Head.Sha+"/status", nil); err == nil && status == 200 {
		if json.Unmarshal(body, &st) == nil && st.TotalCount > 0 {
			combined = st.State
		}
	}
	out := []gin.H{}
	if s := ghCheckStatus(cr.CheckRuns, combined); s != "" {
		out = append(out, gin.H{"id": 0, "status": s, "ref": p.Head.Ref, "sha": p.Head.Sha, "webUrl": p.HTMLURL + "/checks", "createdAt": p.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ghMembers lists who can be asked to review: collaborators (needs push
// access), else the repo's assignable users.
func (h *GitRemoteHandler) ghMembers(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo) ([]ghUser, bool) {
	var users []ghUser
	body, _, status, err := h.ghGET(conn, token, ghRepoPath(info)+"/collaborators", url.Values{"per_page": {"100"}})
	if err == nil && status == 200 && json.Unmarshal(body, &users) == nil {
		return users, true
	}
	users = nil
	_, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/assignees", url.Values{"per_page": {"100"}}, &users)
	return users, ok
}

func (h *GitRemoteHandler) ghProjectMembers(c *gin.Context, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	users, ok := h.ghMembers(c, conn, token, info)
	if !ok {
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("query")))
	out := make([]gin.H, 0, 20)
	for _, u := range users {
		if q != "" && !strings.Contains(strings.ToLower(u.Login), q) {
			continue
		}
		out = append(out, gin.H{"id": u.ID, "name": u.Login, "username": u.Login, "avatarUrl": u.AvatarURL})
		if len(out) == 20 {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ghLoginsFor turns the FE's user ids into logins (members list, then /user/:id).
func (h *GitRemoteHandler) ghLoginsFor(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo, ids []int64) ([]string, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	users, _ := h.ghMembers(c, conn, token, info)
	if c.IsAborted() || c.Writer.Written() {
		return nil, false
	}
	byID := map[int64]string{}
	for _, u := range users {
		byID[u.ID] = u.Login
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if l, ok := byID[id]; ok {
			out = append(out, l)
			continue
		}
		var u ghUser
		if _, ok := h.ghJSON(c, conn, token, "/user/"+strconv.FormatInt(id, 10), nil, &u); !ok {
			return nil, false
		}
		out = append(out, u.Login)
	}
	return out, true
}

// ── writes ──────────────────────────────────────────────────────────────────

func (h *GitRemoteHandler) ghCreateMRNote(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid, body string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	out, ok := h.ghWrite(c, conn, token, userID, "merge_request_note_create", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/comments", gin.H{"body": body}, gin.H{"project": project, "iid": iid})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": jsonInt(out["id"]), "iid": iid, "posted": true}})
}

func (h *GitRemoteHandler) ghApproveMR(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_approve", http.MethodPost, ghRepoPath(info)+"/pulls/"+iid+"/reviews", gin.H{"event": "APPROVE"}, gin.H{"project": project, "iid": iid}); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"iid": iid, "approved": true}})
}

// ghUnapproveMR dismisses the caller's latest approval. GitHub lets only
// people with admin/maintain rights dismiss reviews on protected branches.
func (h *GitRemoteHandler) ghUnapproveMR(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	rs, ok := h.ghReviews(c, conn, token, info, iid)
	if !ok {
		return
	}
	me, ok := h.ghLogin(c, conn, token)
	if !ok {
		return
	}
	r, found := ghLatestReviews(rs)[strings.ToLower(me)]
	if !found || r.State != "APPROVED" {
		c.JSON(http.StatusConflict, gin.H{"error": "You haven't approved this pull request."})
		return
	}
	if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_unapprove", http.MethodPut,
		ghRepoPath(info)+"/pulls/"+iid+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/dismissals",
		gin.H{"message": "Approval withdrawn from PatchMan", "event": "DISMISS"}, gin.H{"project": project, "iid": iid}); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approved": false}})
}

func (h *GitRemoteHandler) ghRequestPeople(c *gin.Context, conn *models.GitConnection, token, userID string, info ghRepoInfo, iid string, reviewerIDs, assigneeIDs []int64, detail gin.H) bool {
	if len(reviewerIDs) > 0 {
		logins, ok := h.ghLoginsFor(c, conn, token, info, reviewerIDs)
		if !ok {
			return false
		}
		if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_reviewers", http.MethodPost, ghRepoPath(info)+"/pulls/"+iid+"/requested_reviewers", gin.H{"reviewers": logins}, detail); !ok {
			return false
		}
	}
	if len(assigneeIDs) > 0 {
		logins, ok := h.ghLoginsFor(c, conn, token, info, assigneeIDs)
		if !ok {
			return false
		}
		if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_assignees", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/assignees", gin.H{"assignees": logins}, detail); !ok {
			return false
		}
	}
	return true
}

func (h *GitRemoteHandler) ghCreateMR(c *gin.Context, userID string, conn *models.GitConnection, token, project string, in mrCreateInput) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	title := strings.TrimSpace(in.Title)
	if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(title, "Draft:"), "draft:")); t != "" {
		title = t
	}
	detail := gin.H{"project": project, "source": in.SourceBranch, "target": in.TargetBranch}
	out, ok := h.ghWrite(c, conn, token, userID, "merge_request_create", http.MethodPost, ghRepoPath(info)+"/pulls",
		gin.H{"head": in.SourceBranch, "base": in.TargetBranch, "title": title, "body": in.Description, "draft": in.Draft}, detail)
	if !ok {
		return
	}
	iid := strconv.FormatInt(jsonInt(out["number"]), 10)
	if !h.ghRequestPeople(c, conn, token, userID, info, iid, in.ReviewerIDs, in.AssigneeIDs, detail) {
		return
	}
	if strings.TrimSpace(in.Labels) != "" {
		labels := []string{}
		for _, l := range strings.Split(in.Labels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_labels", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/labels", gin.H{"labels": labels}, detail); !ok {
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": ghPullSummary(out)})
}

func (h *GitRemoteHandler) ghUpdateMR(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string, in mrUpdateInput) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	detail := gin.H{"project": project, "iid": iid}
	body := gin.H{}
	if in.Title != nil {
		body["title"] = *in.Title
	}
	if in.Description != nil {
		body["body"] = *in.Description
	}
	if in.TargetBranch != nil {
		body["base"] = *in.TargetBranch
	}
	switch in.StateEvent {
	case "":
	case "close":
		body["state"] = "closed"
	case "reopen":
		body["state"] = "open"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "stateEvent must be close or reopen"})
		return
	}
	var out map[string]any
	if len(body) > 0 {
		if out, ok = h.ghWrite(c, conn, token, userID, "merge_request_update", http.MethodPatch, ghRepoPath(info)+"/pulls/"+iid, body, detail); !ok {
			return
		}
	}
	if in.Draft != nil {
		p, ok := h.ghPull(c, conn, token, info, iid)
		if !ok {
			return
		}
		if p.Draft != *in.Draft {
			m := `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}`
			if *in.Draft {
				m = `mutation($id:ID!){convertPullRequestToDraft(input:{pullRequestId:$id}){pullRequest{isDraft}}}`
			}
			if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_draft", m, map[string]any{"id": p.NodeID}, nil, detail) {
				return
			}
		}
	}
	if !h.ghRequestPeople(c, conn, token, userID, info, iid, in.ReviewerIDs, in.AssigneeIDs, detail) {
		return
	}
	if out == nil {
		p, ok := h.ghPull(c, conn, token, info, iid)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"iid": p.Number, "webUrl": p.HTMLURL, "state": p.glState(), "title": p.Title, "mergeStatus": p.MergeableState}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ghPullSummary(out)})
}

// ghMergeMR merges (merge commit or squash). "Merge when the pipeline
// succeeds" → GitHub auto-merge (the repo must allow it). Deleting the source
// branch only happens for branches in the same repo, never a fork's.
func (h *GitRemoteHandler) ghMergeMR(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string, in mrMergeInput) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	method := "merge"
	if in.Squash {
		method = "squash"
	}
	detail := gin.H{"project": project, "iid": iid, "squash": in.Squash, "auto": in.MergeWhenPipelineSucceeds}
	title, msg := "", ""
	if m := in.commitMessage(); m != "" {
		title, msg, _ = strings.Cut(m, "\n")
		title, msg = strings.TrimSpace(title), strings.TrimSpace(msg)
	}
	if in.MergeWhenPipelineSucceeds {
		vars := map[string]any{"id": p.NodeID, "method": strings.ToUpper(method)}
		m := `mutation($id:ID!,$method:PullRequestMergeMethod!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:$method}){pullRequest{number}}}`
		if title != "" {
			vars["title"], vars["body"] = title, msg
			m = `mutation($id:ID!,$method:PullRequestMergeMethod!,$title:String,$body:String){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:$method,commitHeadline:$title,commitBody:$body}){pullRequest{number}}}`
		}
		if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_merge", m, vars, nil, detail) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"iid": p.Number, "webUrl": p.HTMLURL, "state": "opened", "title": p.Title, "mergeStatus": "auto_merge", "autoMerge": true}})
		return
	}
	body := gin.H{"merge_method": method}
	if s := strings.TrimSpace(in.SHA); s != "" {
		body["sha"] = s
	}
	if title != "" {
		body["commit_title"] = title
		if msg != "" {
			body["commit_message"] = msg
		}
	}
	if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_merge", http.MethodPut, ghRepoPath(info)+"/pulls/"+iid+"/merge", body, detail); !ok {
		return
	}
	branchDeleted := false
	if in.RemoveSourceBranch && p.Head.Repo != nil && p.Base.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName) && p.Head.Ref != info.DefaultBranch {
		raw, status, err := h.ghDo(conn, token, http.MethodDelete, ghRepoPath(info)+"/git/refs/heads/"+ghRefEscape(p.Head.Ref), nil)
		branchDeleted = err == nil && status/100 == 2
		h.conns.audit(userID, conn.ID, "remote_branch_delete", branchDeleted, gin.H{"project": project, "branch": p.Head.Ref, "status": status, "error": ghErrText(err, raw, status)})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"iid": p.Number, "webUrl": p.HTMLURL, "state": "merged", "title": p.Title, "mergeStatus": "merged", "sourceBranchDeleted": branchDeleted}})
}

func ghRefEscape(ref string) string {
	parts := strings.Split(ref, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func ghErrText(err error, raw []byte, status int) string {
	if err != nil {
		return err.Error()
	}
	if status/100 == 2 {
		return ""
	}
	return ghMessage(raw)
}

// ghRebaseMR brings the PR branch up to date by rebasing it on the base
// (GraphQL updatePullRequestBranch, updateMethod REBASE).
func (h *GitRemoteHandler) ghRebaseMR(c *gin.Context, userID string, conn *models.GitConnection, token, project, iid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return
	}
	m := `mutation($id:ID!,$sha:GitObjectID){updatePullRequestBranch(input:{pullRequestId:$id,expectedHeadOid:$sha,updateMethod:REBASE}){pullRequest{number}}}`
	if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_rebase", m, map[string]any{"id": p.NodeID, "sha": p.Head.Sha}, nil, gin.H{"project": project, "iid": iid}) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rebaseInProgress": true}})
}
