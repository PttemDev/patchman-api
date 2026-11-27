package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// Merge request parity with GitLab's MR page: the richer MR detail (people,
// labels, milestone, auto-merge state, merge settings, reference), editing and
// deleting comments, the commits tab, cancelling auto-merge, emoji awards on
// the MR, and the label / milestone pickers. GitHub is mapped where it has an
// equivalent; where it has none the route answers 400 and the FE hides the
// control.
//
// Upstream paths and fields that weren't checked against a live server are
// marked "doğrulanmadı" (rules.md: a fake server only proves what we assumed).

// ── who am I (for "your comment" / "your award") ──────────────────────────

// glUsername is the GitLab username of the token, "" when unknown. Never
// fails the request: it only decides whether edit buttons show. Cached for a
// while per instance + token hash (the raw token is never a map key), since
// the MR detail, discussions and awards each need it on every reload.
func (h *GitRemoteHandler) glUsername(c *gin.Context, token string) string {
	sum := sha256.Sum256([]byte(token))
	key := "gl-user|" + h.apiBase(c) + "|" + hex.EncodeToString(sum[:])
	if v, ok := ghRepoCache.Load(key); ok {
		if e := v.(ghCached); time.Since(e.at) < 10*time.Minute {
			return e.info.FullName
		}
	}
	raw, _, status, err := h.gitlabGET(c, token, "/user", nil)
	if err != nil || status/100 != 2 {
		return ""
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(raw, &me)
	if me.Username != "" {
		ghRepoCache.Store(key, ghCached{info: ghRepoInfo{FullName: me.Username}, at: time.Now()})
	}
	return me.Username
}

// ghLoginSoft is ghLogin without writing an error response.
func (h *GitRemoteHandler) ghLoginSoft(conn *models.GitConnection, token string) string {
	key := "login|" + conn.ID
	if v, ok := ghRepoCache.Load(key); ok {
		if e := v.(ghCached); time.Since(e.at) < 10*time.Minute {
			return e.info.FullName
		}
	}
	raw, _, status, err := h.ghGET(conn, token, "/user", nil)
	if err != nil || status/100 != 2 {
		return ""
	}
	var me struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(raw, &me)
	if me.Login != "" {
		ghRepoCache.Store(key, ghCached{info: ghRepoInfo{FullName: me.Login}, at: time.Now()})
	}
	return me.Login
}

// ── MR detail ──────────────────────────────────────────────────────────────

type glMRUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

func (u glMRUser) dto() gin.H {
	return gin.H{"id": u.ID, "name": u.Name, "username": u.Username, "avatarUrl": u.AvatarURL}
}

func glUsers(us []glMRUser) []gin.H {
	out := make([]gin.H, 0, len(us))
	for _, u := range us {
		out = append(out, u.dto())
	}
	return out
}

type glMRDetail struct {
	IID                 int64      `json:"iid"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	State               string     `json:"state"`
	SourceBranch        string     `json:"source_branch"`
	TargetBranch        string     `json:"target_branch"`
	SourceProjectID     int64      `json:"source_project_id"`
	TargetProjectID     int64      `json:"target_project_id"`
	WebURL              string     `json:"web_url"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
	MergedAt            *string    `json:"merged_at"`
	ClosedAt            *string    `json:"closed_at"`
	Draft               bool       `json:"draft"`
	MergeStatus         string     `json:"merge_status"`
	DetailedMergeStatus string     `json:"detailed_merge_status"`
	HasConflicts        bool       `json:"has_conflicts"`
	SHA                 string     `json:"sha"`
	MergeCommitSHA      *string    `json:"merge_commit_sha"`
	SquashCommitSHA     *string    `json:"squash_commit_sha"`
	Author              glMRUser   `json:"author"`
	Assignees           []glMRUser `json:"assignees"`
	Reviewers           []glMRUser `json:"reviewers"`
	Labels              []string   `json:"labels"`
	MergedBy            *glMRUser  `json:"merged_by"`
	DiffRefs            glDiffRefs `json:"diff_refs"`
	Milestone           *struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"milestone"`
	// merge_when_pipeline_succeeds, force_remove_source_branch, user.can_merge
	// and changes_count follow the GitLab MR API docs (doğrulanmadı live).
	MergeWhenPipelineSucceeds bool   `json:"merge_when_pipeline_succeeds"`
	Squash                    bool   `json:"squash"`
	ForceRemoveSourceBranch   *bool  `json:"force_remove_source_branch"`
	UserNotesCount            int    `json:"user_notes_count"`
	ChangesCount              string `json:"changes_count"` // a string ("12", "1000+")
	References                struct {
		Full string `json:"full"`
	} `json:"references"`
	User *struct {
		CanMerge bool `json:"can_merge"`
	} `json:"user"`
}

func (r glMRDetail) dto(me string) gin.H {
	labels := make([]string, 0, len(r.Labels))
	labels = append(labels, r.Labels...)
	var milestone any
	if r.Milestone != nil {
		milestone = gin.H{"id": r.Milestone.ID, "title": r.Milestone.Title}
	}
	var mergedBy any
	if r.MergedBy != nil {
		mergedBy = r.MergedBy.dto()
	}
	var canMerge any // null = unknown
	if r.User != nil {
		canMerge = r.User.CanMerge
	}
	removeSource := r.ForceRemoveSourceBranch != nil && *r.ForceRemoveSourceBranch
	return gin.H{
		"iid": r.IID, "title": r.Title, "description": r.Description, "state": r.State,
		"sourceBranch": r.SourceBranch, "targetBranch": r.TargetBranch, "author": r.Author.Name,
		"authorUsername": r.Author.Username,
		"webUrl":         r.WebURL, "createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt,
		"mergedAt": r.MergedAt, "closedAt": r.ClosedAt,
		"draft": r.Draft, "mergeStatus": r.MergeStatus, "detailedMergeStatus": r.DetailedMergeStatus,
		"hasConflicts": r.HasConflicts, "sha": r.SHA, "diffRefs": r.DiffRefs.toDTO(),
		"mergeCommitSha": r.MergeCommitSHA, "squashCommitSha": r.SquashCommitSHA,
		"assignees": glUsers(r.Assignees), "reviewers": glUsers(r.Reviewers), "labels": labels,
		"milestone": milestone, "mergedBy": mergedBy,
		"autoMerge": r.MergeWhenPipelineSucceeds, "squash": r.Squash, "removeSourceBranch": removeSource,
		"notesCount": r.UserNotesCount, "changesCount": r.ChangesCount,
		"reference": r.References.Full, "fork": r.SourceProjectID != 0 && r.SourceProjectID != r.TargetProjectID,
		"canMerge": canMerge, "me": me, "provider": "gitlab",
	}
}

// ── comments: edit / delete ───────────────────────────────────────────────

// noteRef validates ?note= (numeric) and ?discussion= (optional for GitLab,
// needed on GitHub to pick the API: the id alone doesn't say which kind).
func noteRef(c *gin.Context) (note, disc string, ok bool) {
	note = strings.TrimSpace(c.Query("note"))
	if _, err := strconv.ParseInt(note, 10, 64); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "note must be a numeric id"})
		return "", "", false
	}
	disc = strings.TrimSpace(c.Query("discussion"))
	if strings.ContainsAny(disc, "/?#") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad discussion id"})
		return "", "", false
	}
	return note, disc, true
}

// UpdateMRNote — POST .../merge_request_note_update?project=&iid=&note=&discussion=  {body}
func (h *GitRemoteHandler) UpdateMRNote(c *gin.Context) { h.noteOp(c, false) }

// DeleteMRNote — POST .../merge_request_note_delete?project=&iid=&note=&discussion=
func (h *GitRemoteHandler) DeleteMRNote(c *gin.Context) { h.noteOp(c, true) }

func (h *GitRemoteHandler) noteOp(c *gin.Context, del bool) {
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
	note, disc, ok := noteRef(c)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	_ = c.ShouldBindJSON(&in)
	if !del && strings.TrimSpace(in.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body is required"})
		return
	}
	op := "merge_request_note_update"
	if del {
		op = "merge_request_note_delete"
	}
	// Audit names the note, never its text.
	detail := gin.H{"project": project, "iid": iid, "note": note}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		var path string
		switch kind, _, _ := strings.Cut(disc, "-"); {
		case kind == "ic":
			path = ghRepoPath(info) + "/issues/comments/" + note
		case kind == "rv":
			c.JSON(http.StatusBadRequest, gin.H{"error": "A submitted review's summary can't be edited or deleted from here."})
			return
		case disc == "":
			c.JSON(http.StatusBadRequest, gin.H{"error": "discussion is required"})
			return
		default: // rc-<id> or a GraphQL review thread: line comments
			path = ghRepoPath(info) + "/pulls/comments/" + note
		}
		if del {
			if _, ok := h.ghWrite(c, conn, token, userID, op, http.MethodDelete, path, nil, detail); !ok {
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
			return
		}
		if _, ok := h.ghWrite(c, conn, token, userID, op, http.MethodPatch, path, gin.H{"body": in.Body}, detail); !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": note, "updated": true}})
		return
	}
	// GitLab: PUT / DELETE /merge_requests/:iid/notes/:note_id (doğrulanmadı
	// against a live instance; documented in the Notes API).
	path := mrPath(project, iid) + "/notes/" + note
	if del {
		if _, ok := h.gitlabWrite(c, token, userID, conn.ID, op, http.MethodDelete, path, nil, detail); !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
		return
	}
	if _, ok := h.gitlabWrite(c, token, userID, conn.ID, op, http.MethodPut, path, gin.H{"body": in.Body}, detail); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": note, "updated": true}})
}

// ── commits tab ───────────────────────────────────────────────────────────

// MRCommits — GET .../merge_request_commits?project=&iid= (newest first, ≤100).
func (h *GitRemoteHandler) MRCommits(c *gin.Context) {
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
	out := []gitRemoteCommit{}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		var raw []ghCommit
		if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/pulls/"+iid+"/commits", url.Values{"per_page": {"100"}}, &raw); !ok {
			return
		}
		// GitHub lists oldest first; GitLab's MR page shows newest first.
		for i := len(raw) - 1; i >= 0; i-- {
			out = append(out, raw[i].toDTO())
		}
	} else {
		var raw []glCommit
		if _, ok := h.getJSON(c, token, mrPath(project, iid)+"/commits", url.Values{"per_page": {"100"}}, &raw); !ok {
			return
		}
		for _, cm := range raw {
			out = append(out, cm.toDTO())
		}
	}
	h.conns.audit(userID, conn.ID, "merge_request_commits", true, gin.H{"project": project, "iid": iid})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ── auto-merge ────────────────────────────────────────────────────────────

// CancelAutoMergeMR — POST .../merge_request_cancel_auto_merge?project=&iid=
func (h *GitRemoteHandler) CancelAutoMergeMR(c *gin.Context) {
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
	detail := gin.H{"project": project, "iid": iid}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		p, ok := h.ghPull(c, conn, token, info, iid)
		if !ok {
			return
		}
		m := `mutation($id:ID!){disablePullRequestAutoMerge(input:{pullRequestId:$id}){pullRequest{number}}}`
		if !h.ghGraphQLWrite(c, conn, token, userID, "merge_request_cancel_auto_merge", m, map[string]any{"id": p.NodeID}, nil, detail) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"autoMerge": false}})
		return
	}
	// POST .../cancel_merge_when_pipeline_succeeds (GitLab docs; doğrulanmadı live).
	if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_cancel_auto_merge", http.MethodPost,
		mrPath(project, iid)+"/cancel_merge_when_pipeline_succeeds", nil, detail); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"autoMerge": false}})
}

// ── emoji awards on the MR ────────────────────────────────────────────────

// mrAwardNames are the awards offered (GitLab names); GitHub reactions are
// a fixed set, so only these map both ways.
var mrAwardNames = []struct{ gl, gh string }{
	{"thumbsup", "+1"}, {"thumbsdown", "-1"}, {"smile", "laugh"}, {"tada", "hooray"},
	{"confused", "confused"}, {"heart", "heart"}, {"rocket", "rocket"}, {"eyes", "eyes"},
}

func awardGH(name string) string {
	for _, a := range mrAwardNames {
		if a.gl == name {
			return a.gh
		}
	}
	return ""
}

func awardGL(content string) string {
	for _, a := range mrAwardNames {
		if a.gh == content {
			return a.gl
		}
	}
	return ""
}

type mrAward struct {
	ID       int64
	Name     string // GitLab name
	Username string
	UserName string
}

func (h *GitRemoteHandler) listAwards(c *gin.Context, conn *models.GitConnection, token, project, iid string) ([]mrAward, string, bool) {
	out := []mrAward{}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return nil, "", false
		}
		var raw []struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
			User    ghUser `json:"user"`
		}
		if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/issues/"+iid+"/reactions", url.Values{"per_page": {"100"}}, &raw); !ok {
			return nil, "", false
		}
		for _, r := range raw {
			if n := awardGL(r.Content); n != "" {
				out = append(out, mrAward{ID: r.ID, Name: n, Username: r.User.Login, UserName: r.User.Login})
			}
		}
		return out, h.ghLoginSoft(conn, token), true
	}
	var raw []struct {
		ID   int64    `json:"id"`
		Name string   `json:"name"`
		User glMRUser `json:"user"`
	}
	if _, ok := h.getJSON(c, token, mrPath(project, iid)+"/award_emoji", url.Values{"per_page": {"100"}}, &raw); !ok {
		return nil, "", false
	}
	for _, r := range raw {
		out = append(out, mrAward{ID: r.ID, Name: r.Name, Username: r.User.Username, UserName: r.User.Name})
	}
	return out, h.glUsername(c, token), true
}

// awardSummary groups awards by name: count, whether "me" gave one, and who.
func awardSummary(awards []mrAward, me string) []gin.H {
	type agg struct {
		count int
		mine  bool
		users []string
	}
	order := []string{}
	m := map[string]*agg{}
	for _, a := range awards {
		g, ok := m[a.Name]
		if !ok {
			g = &agg{users: []string{}}
			m[a.Name] = g
			order = append(order, a.Name)
		}
		g.count++
		g.users = append(g.users, a.UserName)
		if me != "" && strings.EqualFold(a.Username, me) {
			g.mine = true
		}
	}
	out := make([]gin.H, 0, len(order))
	for _, n := range order {
		g := m[n]
		out = append(out, gin.H{"name": n, "count": g.count, "mine": g.mine, "users": g.users})
	}
	return out
}

// MRAwards — GET .../merge_request_awards?project=&iid=
func (h *GitRemoteHandler) MRAwards(c *gin.Context) {
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
	awards, me, ok := h.listAwards(c, conn, token, project, iid)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": awardSummary(awards, me)})
}

// ToggleMRAward — POST .../merge_request_award?project=&iid=  {name, on}
// on=true adds the caller's award, on=false removes it.
func (h *GitRemoteHandler) ToggleMRAward(c *gin.Context) {
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
		Name string `json:"name"`
		On   bool   `json:"on"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || awardGH(in.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be one of thumbsup, thumbsdown, smile, tada, confused, heart, rocket, eyes"})
		return
	}
	detail := gin.H{"project": project, "iid": iid, "award": in.Name, "on": in.On}
	if in.On {
		if isGitHub(conn) {
			info, ok := h.ghRepo(c, conn, token, project)
			if !ok {
				return
			}
			if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_award", http.MethodPost, ghRepoPath(info)+"/issues/"+iid+"/reactions", gin.H{"content": awardGH(in.Name)}, detail); !ok {
				return
			}
		} else if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_award", http.MethodPost, mrPath(project, iid)+"/award_emoji", gin.H{"name": in.Name}, detail); !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": in.Name, "on": true}})
		return
	}
	awards, me, ok := h.listAwards(c, conn, token, project, iid)
	if !ok {
		return
	}
	var id int64
	for _, a := range awards {
		if a.Name == in.Name && me != "" && strings.EqualFold(a.Username, me) {
			id = a.ID
			break
		}
	}
	if id == 0 {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": in.Name, "on": false}}) // nothing to remove
		return
	}
	sid := strconv.FormatInt(id, 10)
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		if _, ok := h.ghWrite(c, conn, token, userID, "merge_request_award", http.MethodDelete, ghRepoPath(info)+"/issues/"+iid+"/reactions/"+sid, nil, detail); !ok {
			return
		}
	} else if _, ok := h.gitlabWrite(c, token, userID, conn.ID, "merge_request_award", http.MethodDelete, mrPath(project, iid)+"/award_emoji/"+sid, nil, detail); !ok { // doğrulanmadı live
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": in.Name, "on": false}})
}

// ── label / milestone pickers ─────────────────────────────────────────────

// ProjectLabels — GET .../project_labels?project=  [{name, color}]
func (h *GitRemoteHandler) ProjectLabels(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	var raw []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/labels", url.Values{"per_page": {"100"}}, &raw); !ok {
			return
		}
	} else if _, ok := h.getJSON(c, token, projectPath(project)+"/labels", url.Values{"per_page": {"100"}}, &raw); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, l := range raw {
		color := l.Color
		if color != "" && !strings.HasPrefix(color, "#") {
			color = "#" + color // GitHub sends "d73a4a"
		}
		out = append(out, gin.H{"name": l.Name, "color": color})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ProjectMilestones — GET .../project_milestones?project=  [{id, title}] (open ones).
// On GitHub id is the milestone number (what PATCH /issues takes).
func (h *GitRemoteHandler) ProjectMilestones(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	out := []gin.H{}
	if isGitHub(conn) {
		info, ok := h.ghRepo(c, conn, token, project)
		if !ok {
			return
		}
		var raw []struct {
			Number int64  `json:"number"`
			Title  string `json:"title"`
		}
		if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/milestones", url.Values{"per_page": {"100"}, "state": {"open"}}, &raw); !ok {
			return
		}
		for _, m := range raw {
			out = append(out, gin.H{"id": m.Number, "title": m.Title})
		}
	} else {
		var raw []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		}
		if _, ok := h.getJSON(c, token, projectPath(project)+"/milestones", url.Values{"per_page": {"100"}, "state": {"active"}}, &raw); !ok {
			return
		}
		for _, m := range raw {
			out = append(out, gin.H{"id": m.ID, "title": m.Title})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ── GitHub people helpers ─────────────────────────────────────────────────

func ghUserDTO(u ghUser) gin.H {
	return gin.H{"id": u.ID, "name": u.Login, "username": u.Login, "avatarUrl": u.AvatarURL}
}

func ghUsersDTO(us []ghUser) []gin.H {
	out := make([]gin.H, 0, len(us))
	for _, u := range us {
		out = append(out, ghUserDTO(u))
	}
	return out
}

// ghSetPeople makes the PR's requested reviewers / assignees equal the given
// ids (nil = leave as is): GitHub only adds through POST, so the ones no
// longer wanted are removed with DELETE. A reviewer who already reviewed is
// no longer "requested" and is left alone.
func (h *GitRemoteHandler) ghSetPeople(c *gin.Context, conn *models.GitConnection, token, userID string, info ghRepoInfo, iid string, reviewerIDs, assigneeIDs []int64, detail gin.H) bool {
	if reviewerIDs == nil && assigneeIDs == nil {
		return true
	}
	p, ok := h.ghPull(c, conn, token, info, iid)
	if !ok {
		return false
	}
	type set struct {
		ids     []int64
		current []ghUser
		path    string
		key     string
		op      string
	}
	for _, s := range []set{
		{reviewerIDs, p.Reviewers, ghRepoPath(info) + "/pulls/" + iid + "/requested_reviewers", "reviewers", "merge_request_reviewers"},
		{assigneeIDs, p.Assignees, ghRepoPath(info) + "/issues/" + iid + "/assignees", "assignees", "merge_request_assignees"},
	} {
		if s.ids == nil {
			continue
		}
		want := map[int64]bool{}
		for _, id := range s.ids {
			want[id] = true
		}
		have := map[int64]bool{}
		remove := []string{}
		for _, u := range s.current {
			have[u.ID] = true
			if !want[u.ID] {
				remove = append(remove, u.Login)
			}
		}
		add := []int64{}
		for _, id := range s.ids {
			if !have[id] {
				add = append(add, id)
			}
		}
		if len(remove) > 0 {
			if _, ok := h.ghWrite(c, conn, token, userID, s.op, http.MethodDelete, s.path, gin.H{s.key: remove}, detail); !ok {
				return false
			}
		}
		if len(add) > 0 {
			logins, ok := h.ghLoginsFor(c, conn, token, info, add)
			if !ok {
				return false
			}
			if _, ok := h.ghWrite(c, conn, token, userID, s.op, http.MethodPost, s.path, gin.H{s.key: logins}, detail); !ok {
				return false
			}
		}
	}
	return true
}
