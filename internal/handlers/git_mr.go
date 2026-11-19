package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Merge request workflow (GIT_IDE_PLAN §I, GitLab): create, edit, merge,
// rebase, threaded inline review (discussions with diff positions),
// approvals, pipelines, and member lookup for reviewers / assignees. Reads
// need access to the connection; writes a write grant (authWrite). Merge is
// a sensitive route (step-up).

func mrPath(project, iid string) string { return projectPath(project) + "/merge_requests/" + iid }

// gitlabWrite runs one write call, audits it as op and decodes the answer.
// ok=false means the error response is already written.
func (h *GitRemoteHandler) gitlabWrite(c *gin.Context, token, userID, connID, op, method, path string, body any, detail gin.H) (map[string]any, bool) {
	raw, status, err := h.gitlabDo(c, token, method, path, body)
	if err != nil {
		h.conns.audit(userID, connID, op, false, detail)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	if status/100 != 2 {
		detail["status"] = status
		h.conns.audit(userID, connID, op, false, detail)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("gitlab %d: %s", status, gitlabMessage(raw))})
		return nil, false
	}
	h.conns.audit(userID, connID, op, true, detail)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out, true
}

// gitlabMessage pulls GitLab's "message"/"error" out of an error body.
func gitlabMessage(raw []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if m.Error != "" {
			return m.Error
		}
		if m.Message != nil {
			if s, ok := m.Message.(string); ok {
				return s
			}
			b, _ := json.Marshal(m.Message)
			return string(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

func mrSummary(m map[string]any) gin.H {
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) any { return m[k] }
	return gin.H{"iid": num("iid"), "webUrl": str("web_url"), "state": str("state"), "title": str("title"), "mergeStatus": str("detailed_merge_status")}
}

type mrCreateInput struct {
	SourceBranch       string  `json:"sourceBranch"`
	TargetBranch       string  `json:"targetBranch"`
	Title              string  `json:"title"`
	Description        string  `json:"description"`
	Draft              bool    `json:"draft"`
	RemoveSourceBranch bool    `json:"removeSourceBranch"`
	Squash             bool    `json:"squash"`
	AssigneeIDs        []int64 `json:"assigneeIds"`
	ReviewerIDs        []int64 `json:"reviewerIds"`
	Labels             string  `json:"labels"`
}

// CreateMR — POST /git/connections/:id/merge_request_create?project=
func (h *GitRemoteHandler) CreateMR(c *gin.Context) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	var in mrCreateInput
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.SourceBranch) == "" || strings.TrimSpace(in.TargetBranch) == "" || strings.TrimSpace(in.Title) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sourceBranch, targetBranch and title are required"})
		return
	}
	if isGitHub(conn) {
		h.ghCreateMR(c, userID, conn, token, project, in)
		return
	}
	title := strings.TrimSpace(in.Title)
	if in.Draft && !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	body := gin.H{
		"source_branch": in.SourceBranch, "target_branch": in.TargetBranch, "title": title,
		"description": in.Description, "remove_source_branch": in.RemoveSourceBranch, "squash": in.Squash,
	}
	if len(in.AssigneeIDs) > 0 {
		body["assignee_ids"] = in.AssigneeIDs
	}
	if len(in.ReviewerIDs) > 0 {
		body["reviewer_ids"] = in.ReviewerIDs
	}
	if strings.TrimSpace(in.Labels) != "" {
		body["labels"] = in.Labels
	}
	out, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_create", http.MethodPost, projectPath(project)+"/merge_requests", body,
		gin.H{"project": project, "source": in.SourceBranch, "target": in.TargetBranch})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrSummary(out)})
}

type mrUpdateInput struct {
	Title        *string `json:"title"`
	Description  *string `json:"description"`
	TargetBranch *string `json:"targetBranch"`
	Draft        *bool   `json:"draft"`
	ReviewerIDs  []int64 `json:"reviewerIds"`
	AssigneeIDs  []int64 `json:"assigneeIds"`
	StateEvent   string  `json:"stateEvent"` // close | reopen
}

// UpdateMR — POST /git/connections/:id/merge_request_update?project=&iid=
func (h *GitRemoteHandler) UpdateMR(c *gin.Context) {
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
	var in mrUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if isGitHub(conn) {
		h.ghUpdateMR(c, userID, conn, token, project, iid, in)
		return
	}
	body := gin.H{}
	if in.Title != nil {
		body["title"] = *in.Title
	}
	if in.Description != nil {
		body["description"] = *in.Description
	}
	if in.TargetBranch != nil {
		body["target_branch"] = *in.TargetBranch
	}
	if in.Draft != nil {
		// GitLab toggles draft through the title prefix.
		var cur struct {
			Title string `json:"title"`
		}
		if _, ok := h.getJSON(c, token, mrPath(project, iid), nil, &cur); !ok {
			return
		}
		t := cur.Title
		if in.Title != nil {
			t = *in.Title
		}
		t = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "Draft:"), "draft:"))
		if *in.Draft {
			t = "Draft: " + t
		}
		body["title"] = t
	}
	if in.ReviewerIDs != nil {
		body["reviewer_ids"] = in.ReviewerIDs
	}
	if in.AssigneeIDs != nil {
		body["assignee_ids"] = in.AssigneeIDs
	}
	switch in.StateEvent {
	case "":
	case "close", "reopen":
		body["state_event"] = in.StateEvent
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "stateEvent must be close or reopen"})
		return
	}
	out, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_update", http.MethodPut, mrPath(project, iid), body, gin.H{"project": project, "iid": iid})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrSummary(out)})
}

type mrMergeInput struct {
	Squash                    bool   `json:"squash"`
	RemoveSourceBranch        bool   `json:"removeSourceBranch"`
	MergeWhenPipelineSucceeds bool   `json:"mergeWhenPipelineSucceeds"`
	SHA                       string `json:"sha"`
	SquashCommitMessage       string `json:"squashCommitMessage"`
}

// MergeMR — POST /git/connections/:id/merge_request_merge?project=&iid=
// (sensitive: step-up). sha pins the head the user reviewed: GitLab refuses
// the merge when the branch moved since.
func (h *GitRemoteHandler) MergeMR(c *gin.Context) {
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
	var in mrMergeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if isGitHub(conn) {
		h.ghMergeMR(c, userID, conn, token, project, iid, in)
		return
	}
	body := gin.H{"squash": in.Squash, "should_remove_source_branch": in.RemoveSourceBranch}
	if in.MergeWhenPipelineSucceeds {
		body["merge_when_pipeline_succeeds"] = true
	}
	if s := strings.TrimSpace(in.SHA); s != "" {
		body["sha"] = s
	}
	if m := strings.TrimSpace(in.SquashCommitMessage); m != "" && in.Squash {
		body["squash_commit_message"] = m
	}
	out, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_merge", http.MethodPut, mrPath(project, iid)+"/merge", body,
		gin.H{"project": project, "iid": iid, "squash": in.Squash, "auto": in.MergeWhenPipelineSucceeds})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrSummary(out)})
}

// RebaseMR — POST /git/connections/:id/merge_request_rebase?project=&iid=
// GitLab rebases asynchronously; the MR's state shows when it is done.
func (h *GitRemoteHandler) RebaseMR(c *gin.Context) {
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
	if isGitHub(conn) {
		h.ghRebaseMR(c, userID, conn, token, project, iid)
		return
	}
	if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_rebase", http.MethodPut, mrPath(project, iid)+"/rebase", nil, gin.H{"project": project, "iid": iid}); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rebaseInProgress": true}})
}

// UnapproveMR — POST /git/connections/:id/merge_request_unapprove?project=&iid=
func (h *GitRemoteHandler) UnapproveMR(c *gin.Context) {
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
	if isGitHub(conn) {
		h.ghUnapproveMR(c, userID, conn, token, project, iid)
		return
	}
	if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_unapprove", http.MethodPost, mrPath(project, iid)+"/unapprove", nil, gin.H{"project": project, "iid": iid}); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"approved": false}})
}

// ── discussions (threaded, optionally on a diff line) ──────────────────

type glNote struct {
	ID         int64  `json:"id"`
	Body       string `json:"body"`
	System     bool   `json:"system"`
	CreatedAt  string `json:"created_at"`
	Resolvable bool   `json:"resolvable"`
	Resolved   bool   `json:"resolved"`
	Author     struct {
		Name      string `json:"name"`
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine *int   `json:"new_line"`
		OldLine *int   `json:"old_line"`
	} `json:"position"`
}

// MRDiscussions — GET /git/connections/:id/merge_request_discussions?project=&iid=
func (h *GitRemoteHandler) MRDiscussions(c *gin.Context) {
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
	if isGitHub(conn) {
		h.ghMRDiscussions(c, userID, conn, token, project, iid)
		return
	}
	var raw []struct {
		ID    string   `json:"id"`
		Notes []glNote `json:"notes"`
	}
	q := url.Values{"per_page": {"100"}}
	if _, ok := h.getJSON(c, token, mrPath(project, iid)+"/discussions", q, &raw); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, d := range raw {
		if len(d.Notes) == 0 || d.Notes[0].System {
			continue // "added 1 commit" etc.
		}
		notes := make([]gin.H, 0, len(d.Notes))
		for _, n := range d.Notes {
			notes = append(notes, gin.H{"id": n.ID, "body": n.Body, "author": n.Author.Name, "username": n.Author.Username,
				"avatarUrl": n.Author.AvatarURL, "createdAt": n.CreatedAt})
		}
		first := d.Notes[0]
		item := gin.H{"id": d.ID, "notes": notes, "resolvable": first.Resolvable, "resolved": first.Resolved}
		if p := first.Position; p != nil {
			item["position"] = gin.H{"newPath": p.NewPath, "oldPath": p.OldPath, "newLine": p.NewLine, "oldLine": p.OldLine}
		}
		out = append(out, item)
	}
	h.conns.audit(userID, conn.ID, "merge_request_discussions", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type mrDiscussionInput struct {
	Body     string `json:"body"`
	Position *struct {
		BaseSha  string `json:"baseSha"`
		StartSha string `json:"startSha"`
		HeadSha  string `json:"headSha"`
		NewPath  string `json:"newPath"`
		OldPath  string `json:"oldPath"`
		NewLine  *int   `json:"newLine"`
		OldLine  *int   `json:"oldLine"`
	} `json:"position"`
}

// CreateMRDiscussion — POST /git/connections/:id/merge_request_discussion?project=&iid=
// A new thread; with position it is anchored to a diff line (new_line for
// added/context lines, old_line for removed ones).
func (h *GitRemoteHandler) CreateMRDiscussion(c *gin.Context) {
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
	var in mrDiscussionInput
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is required"})
		return
	}
	if isGitHub(conn) {
		h.ghCreateMRDiscussion(c, userID, conn, token, project, iid, in)
		return
	}
	body := gin.H{"body": in.Body}
	if p := in.Position; p != nil {
		if p.BaseSha == "" || p.HeadSha == "" || p.StartSha == "" || (p.NewLine == nil && p.OldLine == nil) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "position needs the diff refs and a line"})
			return
		}
		pos := gin.H{"position_type": "text", "base_sha": p.BaseSha, "start_sha": p.StartSha, "head_sha": p.HeadSha,
			"new_path": p.NewPath, "old_path": p.OldPath}
		if p.NewLine != nil {
			pos["new_line"] = *p.NewLine
		}
		if p.OldLine != nil {
			pos["old_line"] = *p.OldLine
		}
		body["position"] = pos
	}
	out, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_discussion", http.MethodPost, mrPath(project, iid)+"/discussions", body,
		gin.H{"project": project, "iid": iid, "inline": in.Position != nil})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": out["id"]}})
}

// ReplyMRDiscussion — POST .../merge_request_discussion_reply?project=&iid=&discussion=
func (h *GitRemoteHandler) ReplyMRDiscussion(c *gin.Context) { h.discussionOp(c, false) }

// ResolveMRDiscussion — POST .../merge_request_discussion_resolve?project=&iid=&discussion=  {resolved}
func (h *GitRemoteHandler) ResolveMRDiscussion(c *gin.Context) { h.discussionOp(c, true) }

func (h *GitRemoteHandler) discussionOp(c *gin.Context, resolve bool) {
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
	disc := strings.TrimSpace(c.Query("discussion"))
	if disc == "" || strings.ContainsAny(disc, "/?#") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "discussion is required"})
		return
	}
	var in struct {
		Body     string `json:"body"`
		Resolved bool   `json:"resolved"`
	}
	_ = c.ShouldBindJSON(&in)
	if isGitHub(conn) {
		h.ghDiscussionOp(c, userID, conn, token, project, iid, disc, resolve, in.Body, in.Resolved)
		return
	}
	path := mrPath(project, iid) + "/discussions/" + url.PathEscape(disc)
	detail := gin.H{"project": project, "iid": iid, "discussion": disc}
	if resolve {
		if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_discussion_resolve", http.MethodPut, path+"?resolved="+strconv.FormatBool(in.Resolved), nil, detail); !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"resolved": in.Resolved}})
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is required"})
		return
	}
	out, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_discussion_reply", http.MethodPost, path+"/notes", gin.H{"body": in.Body}, detail)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": out["id"]}})
}

// ── approvals, pipelines, members ─────────────────────────────────────

// MRApprovals — GET .../merge_request_approvals?project=&iid=. Who approved
// and how many are still needed (fields an edition lacks come back empty).
func (h *GitRemoteHandler) MRApprovals(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
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
	if isGitHub(conn) {
		h.ghMRApprovals(c, conn, token, project, iid)
		return
	}
	var raw struct {
		Approved          bool `json:"approved"`
		ApprovalsRequired int  `json:"approvals_required"`
		ApprovalsLeft     int  `json:"approvals_left"`
		UserHasApproved   bool `json:"user_has_approved"`
		UserCanApprove    bool `json:"user_can_approve"`
		ApprovedBy        []struct {
			User struct {
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"user"`
		} `json:"approved_by"`
	}
	if _, ok := h.getJSON(c, token, mrPath(project, iid)+"/approvals", nil, &raw); !ok {
		return
	}
	by := make([]string, 0, len(raw.ApprovedBy))
	for _, a := range raw.ApprovedBy {
		by = append(by, a.User.Name)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"approved": raw.Approved, "required": raw.ApprovalsRequired, "left": raw.ApprovalsLeft,
		"userHasApproved": raw.UserHasApproved, "userCanApprove": raw.UserCanApprove, "approvedBy": by,
	}})
}

// MRPipelines — GET .../merge_request_pipelines?project=&iid= (newest first).
func (h *GitRemoteHandler) MRPipelines(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
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
	if isGitHub(conn) {
		h.ghMRPipelines(c, conn, token, project, iid)
		return
	}
	var raw []struct {
		ID        int64  `json:"id"`
		Status    string `json:"status"`
		Ref       string `json:"ref"`
		SHA       string `json:"sha"`
		WebURL    string `json:"web_url"`
		CreatedAt string `json:"created_at"`
	}
	if _, ok := h.getJSON(c, token, mrPath(project, iid)+"/pipelines", url.Values{"per_page": {"5"}}, &raw); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, p := range raw {
		out = append(out, gin.H{"id": p.ID, "status": p.Status, "ref": p.Ref, "sha": p.SHA, "webUrl": p.WebURL, "createdAt": p.CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ProjectMembers — GET .../project_members?project=&query=. For the reviewer
// and assignee pickers.
func (h *GitRemoteHandler) ProjectMembers(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	if isGitHub(conn) {
		h.ghProjectMembers(c, conn, token, project)
		return
	}
	q := url.Values{"per_page": {"20"}}
	if s := strings.TrimSpace(c.Query("query")); s != "" {
		q.Set("query", s)
	}
	var raw []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	}
	if _, ok := h.getJSON(c, token, projectPath(project)+"/members/all", q, &raw); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, m := range raw {
		out = append(out, gin.H{"id": m.ID, "name": m.Name, "username": m.Username, "avatarUrl": m.AvatarURL})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}
