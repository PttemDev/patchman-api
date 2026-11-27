package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/jira"
	"patchman-be/internal/models"
	"patchman-be/internal/notecards"
	"patchman-be/internal/qalink"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AI review (CR_QA_RFC P1). A person's own agent (Claude Code etc.) works
// the review queue over MCP: claim an MR at its head SHA, read the fused
// context (diff, commits, Jira acceptance criteria, linked tests, earlier
// reviews — secret-redacted), submit findings. The server never calls an
// LLM. Findings reach GitLab / GitHub only when a person approves and posts
// them with their own credentials.

const (
	reviewClaimTTL     = 30 * time.Minute
	reviewMaxFindings  = 100
	reviewDiffBudget   = 200 << 10 // bytes of diff text per context
	reviewFileBudget   = 40 << 10  // bytes per file
	reviewMaxFiles     = 300
	reviewTextCap      = 8000
	reviewFindingCap   = 6000
	reviewSeverityList = "blocker,major,minor,nit"
)

var reviewSeverities = map[string]int{"blocker": 0, "major": 1, "minor": 2, "nit": 3}

// linkCtx is a QA link resolved for the caller: the effective (mapped)
// link, its Git connection, a fetcher with the caller's credentials and
// the caller's own username there ("" when the token isn't theirs).
type linkCtx struct {
	link     models.QAProjectLink
	ws       *string
	conn     *models.GitConnection
	role     string
	f        *radarFetcher
	me       string
	ownToken bool // the token is the caller's linked account (not a stored one)
}

// Writes go out under the writer's own name: their linked account or a
// connection they own — never someone else's stored token.
const (
	errNotYourIdentity = "This Git connection's token belongs to its owner, so posting would be under their name — use your own connection (Hub → Settings → Your connection) or your linked GitLab account"
	errNotYourJira     = "This Jira connection's token belongs to its owner — add your own Jira connection (Hub → Settings) to write to Jira"
)

// ownIdentity: the remote calls of lc speak as userID.
func (lc *linkCtx) ownIdentity(userID string) bool { return lc.ownToken || lc.conn.UserID == userID }

// linkAccess resolves a link for the caller, writing 404 / 403 / 412.
func (h *QARadarHandler) linkAccess(ctx context.Context, c *gin.Context, userID, linkID string) (*linkCtx, bool) {
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, linkID, &l, ViewerRole, "Link not found") {
		return nil, false
	}
	ws := l.WorkspaceID
	if m, ok := qaLinkMaps(h.db, userID, []models.QAProjectLink{l})[l.ID]; ok {
		if m.GitConnectionID != "" {
			l.GitConnectionID = m.GitConnectionID
		}
		if m.JiraConnectionID != "" && l.JiraProjectKey != "" {
			l.JiraConnectionID = m.JiraConnectionID
		}
	}
	conn, role, err := h.git.conns.loadAccessible(l.GitConnectionID, userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't use this link's Git connection — map your own in Hub → Settings"})
		return nil, false
	}
	token, base, err := h.git.credsFor(conn, userID)
	if err != nil {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": err.Error()})
		return nil, false
	}
	lc := &linkCtx{link: l, ws: ws, conn: conn, role: role, ownToken: !h.git.usesStoredCredential(conn)}
	lc.f = &radarFetcher{h: h.git, conn: conn, token: token, glBase: h.git.glBase(base)}
	if conn.UserID == userID || lc.ownToken {
		lc.me = lc.f.me(ctx)
	}
	return lc, true
}

// send performs a write (POST / PUT) with a JSON body.
func (f *radarFetcher) send(ctx context.Context, method, endpoint string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	if isGitHub(f.conn) {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := f.h.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, nil
}

// ── MR snapshot (detail + files) ─────────────────────────────────────

type mrSnapshot struct {
	Title        string
	Description  string
	State        string
	Draft        bool
	SourceBranch string
	TargetBranch string
	WebURL       string
	HeadSHA      string
	BaseSHA      string
	StartSHA     string
	Author       string
	Reviewers    []string
	Files        []mrFile
	Commits      []string
}

type mrFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"` // added | deleted | renamed | modified
	Diff      string `json:"diff,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (lc *linkCtx) mrBase(iid int64) string {
	if isGitHub(lc.conn) {
		return ""
	}
	return lc.f.glBase + projectPath(lc.link.GitProject) + "/merge_requests/" + strconv.FormatInt(iid, 10)
}

// snapshot reads the MR's metadata, and with files=true its changed files
// (diff text included) and commit messages.
func (lc *linkCtx) snapshot(ctx context.Context, iid int64, files bool) (*mrSnapshot, error) {
	if isGitHub(lc.conn) {
		return lc.ghSnapshot(ctx, iid, files)
	}
	base := lc.mrBase(iid)
	var raw struct {
		Title        string        `json:"title"`
		Description  string        `json:"description"`
		State        string        `json:"state"`
		Draft        bool          `json:"draft"`
		SourceBranch string        `json:"source_branch"`
		TargetBranch string        `json:"target_branch"`
		WebURL       string        `json:"web_url"`
		SHA          string        `json:"sha"`
		Author       glRadarUser   `json:"author"`
		Reviewers    []glRadarUser `json:"reviewers"`
		DiffRefs     struct {
			BaseSHA  string `json:"base_sha"`
			HeadSHA  string `json:"head_sha"`
			StartSHA string `json:"start_sha"`
		} `json:"diff_refs"`
	}
	if _, err := lc.f.get(ctx, base, &raw); err != nil {
		return nil, err
	}
	s := &mrSnapshot{Title: raw.Title, Description: raw.Description, State: raw.State, Draft: raw.Draft,
		SourceBranch: raw.SourceBranch, TargetBranch: raw.TargetBranch, WebURL: raw.WebURL, HeadSHA: raw.SHA,
		BaseSHA: raw.DiffRefs.BaseSHA, StartSHA: raw.DiffRefs.StartSHA, Author: raw.Author.Username}
	if raw.DiffRefs.HeadSHA != "" {
		s.HeadSHA = raw.DiffRefs.HeadSHA
	}
	for _, r := range raw.Reviewers {
		s.Reviewers = append(s.Reviewers, r.Username)
	}
	if !files {
		return s, nil
	}
	type glDiffRow struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		DeletedFile bool   `json:"deleted_file"`
		RenamedFile bool   `json:"renamed_file"`
		Diff        string `json:"diff"`
	}
	add := func(d glDiffRow) {
		st := "modified"
		switch {
		case d.NewFile:
			st = "added"
		case d.DeletedFile:
			st = "deleted"
		case d.RenamedFile:
			st = "renamed"
		}
		f := mrFile{Path: d.NewPath, Status: st, Diff: d.Diff}
		if d.OldPath != d.NewPath {
			f.OldPath = d.OldPath
		}
		s.Files = append(s.Files, f)
	}
	for page := 1; page <= 5 && len(s.Files) < reviewMaxFiles; page++ {
		var rows []glDiffRow
		hdr, err := lc.f.get(ctx, base+"/diffs?per_page=100&page="+strconv.Itoa(page), &rows)
		if err != nil {
			if re, ok := err.(*remoteErr); ok && re.status == http.StatusNotFound && page == 1 {
				var ch struct {
					Changes []glDiffRow `json:"changes"`
				}
				if _, err := lc.f.get(ctx, base+"/changes", &ch); err != nil {
					return nil, err
				}
				for _, d := range ch.Changes {
					add(d)
				}
				break
			}
			return nil, err
		}
		for _, d := range rows {
			add(d)
		}
		if strings.TrimSpace(hdr.Get("X-Next-Page")) == "" {
			break
		}
	}
	var commits []struct {
		Message string `json:"message"`
	}
	if _, err := lc.f.get(ctx, base+"/commits?per_page=50", &commits); err == nil {
		for _, c := range commits {
			s.Commits = append(s.Commits, c.Message)
		}
	}
	return s, nil
}

func (lc *linkCtx) ghRepo(ctx context.Context) (string, error) {
	return lc.f.ghRepoPath(ctx, lc.link.GitProject)
}

func (lc *linkCtx) ghSnapshot(ctx context.Context, iid int64, files bool) (*mrSnapshot, error) {
	repo, err := lc.ghRepo(ctx)
	if err != nil {
		return nil, err
	}
	pr := lc.f.h.ghBase(lc.conn) + repo + "/pulls/" + strconv.FormatInt(iid, 10)
	var p ghPull
	if _, err := lc.f.get(ctx, pr, &p); err != nil {
		return nil, err
	}
	s := &mrSnapshot{Title: p.Title, Description: p.Body, State: p.glState(), Draft: p.Draft, SourceBranch: p.Head.Ref,
		TargetBranch: p.Base.Ref, WebURL: p.HTMLURL, HeadSHA: p.Head.Sha, BaseSHA: p.Base.Sha, Author: p.User.Login}
	for _, r := range p.Reviewers {
		s.Reviewers = append(s.Reviewers, r.Login)
	}
	if !files {
		return s, nil
	}
	for page := 1; page <= 3; page++ {
		var rows []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			Patch            string `json:"patch"`
		}
		hdr, err := lc.f.get(ctx, pr+"/files?per_page=100&page="+strconv.Itoa(page), &rows)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			st := r.Status
			if st == "removed" {
				st = "deleted"
			}
			s.Files = append(s.Files, mrFile{Path: r.Filename, OldPath: r.PreviousFilename, Status: st, Diff: r.Patch})
		}
		if ghNextPage(hdr) == 0 {
			break
		}
	}
	var commits []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if _, err := lc.f.get(ctx, pr+"/commits?per_page=50", &commits); err == nil {
		for _, c := range commits {
			s.Commits = append(s.Commits, c.Commit.Message)
		}
	}
	return s, nil
}

// ── helpers ───────────────────────────────────────────────────────────

func (h *QARadarHandler) audit(userID string, r *models.MRReview, linkID string, iid int64, sha, action, agent, model, detail string) {
	row := models.MRReviewAudit{ID: uuid.NewString(), UserID: userID, LinkID: linkID, IID: iid, HeadSHA: sha,
		Action: action, Agent: agent, Model: model, Detail: detail, At: time.Now()}
	if r != nil {
		row.ReviewID = r.ID
	}
	go func() { _ = h.db.Create(&row).Error }()
}

func reviewFindings(r *models.MRReview) []models.ReviewFinding {
	out := []models.ReviewFinding{}
	if r.FindingsJSON != "" {
		_ = json.Unmarshal([]byte(r.FindingsJSON), &out)
	}
	return out
}

func setReviewFindings(r *models.MRReview, fs []models.ReviewFinding) {
	b, _ := json.Marshal(fs)
	r.FindingsJSON = string(b)
}

func reviewDTO(r *models.MRReview) gin.H {
	fs := reviewFindings(r)
	counts := map[string]int{}
	for _, f := range fs {
		counts[f.Status]++
	}
	reviewers := []string{}
	if r.ReviewerUsernames != "" {
		reviewers = strings.Split(r.ReviewerUsernames, ",")
	}
	return gin.H{
		"id": r.ID, "workspaceId": r.WorkspaceID, "linkId": r.LinkID, "gitConnectionId": r.GitConnectionID, "provider": r.Provider,
		"project": r.Project, "iid": r.IID, "headSha": r.HeadSHA, "title": r.Title, "webUrl": r.WebURL, "status": r.Status,
		"claimedBy": r.ClaimedBy, "agent": r.Agent, "model": r.Model, "summary": r.Summary, "findings": fs, "counts": counts,
		"authorUsername": r.AuthorUsername, "reviewerUsernames": reviewers, "postedAt": r.PostedAt, "postedBy": r.PostedBy,
		"createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt,
	}
}

func capRunes(s string, n int) (string, bool) {
	if r := []rune(s); len(r) > n {
		return string(r[:n]), true
	}
	return s, false
}

func reviewRef(linkID string, iid int64) string { return linkID + ":" + strconv.FormatInt(iid, 10) }

// ── queue ─────────────────────────────────────────────────────────────

type queueItem struct {
	LinkID          string `json:"linkId"`
	GitConnectionID string `json:"gitConnectionId"`
	Project         string `json:"project"`
	Provider        string `json:"provider"`
	IID             int64  `json:"iid"`
	HeadSHA         string `json:"headSha"`
	Title           string `json:"title"`
	Author          string `json:"author"`
	WebURL          string `json:"webUrl"`
	Reason          string `json:"reason"` // reviewer | mine | open
	ClaimBy         string `json:"claimedBy,omitempty"`
	// Kind: review (no review at head) | verify (check fixes of posted findings).
	Kind         string `json:"kind"`
	OpenFindings int    `json:"openFindings,omitempty"`
}

// ReviewQueue — GET /api/qa/review-queue?workspaceId=&mine=1: open, non-
// draft MRs with no review at their head SHA. MRs where I'm a reviewer come
// first, then my own, then the rest (mine=1 keeps only the first two).
func (h *QARadarHandler) ReviewQueue(c *gin.Context) {
	res, ok := h.result(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	onlyMine := c.Query("mine") == "1"
	reviewed := map[string]models.MRReview{}
	var rows []models.MRReview
	linkIDs := []string{}
	for _, l := range res.links {
		linkIDs = append(linkIDs, l.ID)
	}
	if len(linkIDs) > 0 {
		h.db.Where("link_id IN ?", linkIDs).Find(&rows)
	}
	for _, r := range rows {
		reviewed[reviewRef(r.LinkID, r.IID)+"@"+r.HeadSHA] = r
	}
	items := []queueItem{}
	now := h.now()
	for _, m := range res.MRs {
		if m.Draft || m.HeadSHA == "" {
			continue
		}
		// Fix checks: posted findings + new commits, not checked yet.
		if m.OpenFindings > 0 {
			me := strings.ToLower(res.Me[m.GitConnectionID])
			reason := "open"
			switch {
			case containsFold(m.verifyOwners, userID) || (me != "" && hasUser(m.Reviewers, me)):
				reason = "reviewer"
			case me != "" && strings.EqualFold(m.Author.Username, me):
				reason = "mine"
			}
			if !onlyMine || reason != "open" {
				items = append(items, queueItem{LinkID: m.LinkID, GitConnectionID: m.GitConnectionID, Project: m.Project, Provider: m.Provider, IID: m.IID,
					HeadSHA: m.HeadSHA, Title: m.Title, Author: m.Author.Username, WebURL: m.WebURL, Reason: reason, Kind: "verify", OpenFindings: m.OpenFindings})
			}
		}
		claim := ""
		if r, ok := reviewed[reviewRef(m.LinkID, m.IID)+"@"+m.HeadSHA]; ok {
			fresh := r.Status == "claimed" && now.Sub(r.UpdatedAt) < reviewClaimTTL
			if r.Status != "discarded" && !(r.Status == "claimed" && !fresh) {
				if !(fresh && r.ClaimedBy == userID) {
					continue
				}
			}
			if fresh {
				claim = r.ClaimedBy
			}
		}
		me := strings.ToLower(res.Me[m.GitConnectionID])
		reason := "open"
		switch {
		case me != "" && hasUser(m.Reviewers, me):
			reason = "reviewer"
		case me != "" && strings.EqualFold(m.Author.Username, me):
			reason = "mine"
		}
		if onlyMine && reason == "open" {
			continue
		}
		items = append(items, queueItem{LinkID: m.LinkID, GitConnectionID: m.GitConnectionID, Project: m.Project, Provider: m.Provider, IID: m.IID, HeadSHA: m.HeadSHA,
			Title: m.Title, Author: m.Author.Username, WebURL: m.WebURL, Reason: reason, ClaimBy: claim, Kind: "review"})
	}
	rank := map[string]int{"reviewer": 0, "mine": 1, "open": 2}
	sort.SliceStable(items, func(a, b int) bool { return rank[items[a].Reason] < rank[items[b].Reason] })
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items, "generatedAt": res.GeneratedAt}})
}

// ── claim ─────────────────────────────────────────────────────────────

type claimInput struct {
	LinkID  string `json:"linkId" binding:"required"`
	IID     int64  `json:"iid" binding:"required"`
	HeadSHA string `json:"headSha"`
}

// claimReview creates or takes the claim for (link, iid, sha). It returns
// the review, or an HTTP status + message when someone else holds it.
func (h *QARadarHandler) claimReview(userID, agent string, lc *linkCtx, iid int64, snap *mrSnapshot) (*models.MRReview, int, string) {
	var r models.MRReview
	found := false
	if q := h.db.Where("link_id = ? AND iid = ? AND head_sha = ?", lc.link.ID, iid, snap.HeadSHA).Limit(1).Find(&r); q.Error == nil && q.RowsAffected > 0 {
		found = true
	}
	if found {
		switch {
		case r.Status == "claimed" && r.ClaimedBy != userID && h.now().Sub(r.UpdatedAt) < reviewClaimTTL:
			return &r, http.StatusConflict, "Another person's agent is reviewing this MR at this commit"
		case r.Status == "draft" || r.Status == "posted":
			return &r, http.StatusConflict, "This MR already has a review at this commit"
		}
	} else {
		r = models.MRReview{ID: uuid.NewString(), WorkspaceID: lc.ws, LinkID: lc.link.ID, IID: iid, HeadSHA: snap.HeadSHA}
	}
	r.GitConnectionID, r.Provider, r.Project = lc.conn.ID, providerOf(lc.conn), lc.link.GitProject
	r.Title, r.WebURL, r.Status, r.ClaimedBy, r.Agent = snap.Title, snap.WebURL, "claimed", userID, agent
	r.Summary, r.FindingsJSON, r.Model = "", "", ""
	r.AuthorUsername, r.ReviewerUsernames = snap.Author, strings.Join(snap.Reviewers, ",")
	if err := h.db.Save(&r).Error; err != nil {
		return nil, http.StatusInternalServerError, "Failed to claim"
	}
	h.audit(userID, &r, lc.link.ID, iid, snap.HeadSHA, "claim", agent, "", "")
	invalidateQACaches(userID, lc.ws)
	return &r, 0, ""
}

func providerOf(conn *models.GitConnection) string {
	if isGitHub(conn) {
		return "github"
	}
	return "gitlab"
}

// Claim — POST /api/qa/reviews/claim {linkId, iid, headSha}.
func (h *QARadarHandler) Claim(c *gin.Context) {
	userID := c.GetString("userID")
	var in claimInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "linkId and iid are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, in.LinkID)
	if !ok {
		return
	}
	snap, err := lc.snapshot(ctx, in.IID, false)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if in.HeadSHA != "" && in.HeadSHA != snap.HeadSHA {
		c.JSON(http.StatusConflict, gin.H{"error": "The MR has a newer commit; claim its current head", "headSha": snap.HeadSHA})
		return
	}
	r, code, msg := h.claimReview(userID, noteAgent(c), lc, in.IID, snap)
	if code != 0 {
		out := gin.H{"error": msg}
		if r != nil {
			out["reviewId"] = r.ID
		}
		c.JSON(code, out)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": reviewDTO(r)})
}

// ── context ───────────────────────────────────────────────────────────

// MRContext — GET /api/qa/mr-context?linkId=&iid=&claim=1: everything an
// agent needs to review (or draft tests for) an MR, secret-redacted and
// size-capped; files left out are listed, never dropped silently.
func (h *QARadarHandler) MRContext(c *gin.Context) {
	userID := c.GetString("userID")
	iid, err := strconv.ParseInt(c.Query("iid"), 10, 64)
	if err != nil || iid <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iid is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, c.Query("linkId"))
	if !ok {
		return
	}
	snap, err := lc.snapshot(ctx, iid, true)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	agent := noteAgent(c)
	redactions := 0
	red := func(s string) string {
		out, n := notecards.Redact(s)
		redactions += n
		return out
	}

	// Files within the budget; the rest listed as omitted.
	files := []mrFile{}
	omitted := []string{}
	budget := reviewDiffBudget
	// ?file=<path> reads one file in full (up to the per-file cap), for an
	// agent following up on a file the first context left out.
	if one := c.Query("file"); one != "" {
		kept := []mrFile{}
		for _, f := range snap.Files {
			if f.Path == one || f.OldPath == one {
				kept = append(kept, f)
			}
		}
		snap.Files = kept
	}
	for i, f := range snap.Files {
		if i >= reviewMaxFiles {
			omitted = append(omitted, f.Path)
			continue
		}
		d := red(f.Diff)
		if len(d) > reviewFileBudget {
			d, f.Truncated = d[:reviewFileBudget], true
		}
		if budget-len(d) < 0 {
			f.Diff = ""
			omitted = append(omitted, f.Path)
			continue
		}
		budget -= len(d)
		f.Diff = d
		files = append(files, f)
	}
	desc, _ := capRunes(red(snap.Description), reviewTextCap)
	commits := []string{}
	for _, m := range snap.Commits {
		m, _ = capRunes(red(m), 2000)
		commits = append(commits, m)
	}

	out := gin.H{
		"link":         gin.H{"id": lc.link.ID, "project": lc.link.GitProject, "provider": providerOf(lc.conn), "jiraProjectKey": lc.link.JiraProjectKey},
		"mr":           gin.H{"iid": iid, "title": red(snap.Title), "description": desc, "state": snap.State, "draft": snap.Draft, "sourceBranch": snap.SourceBranch, "targetBranch": snap.TargetBranch, "webUrl": snap.WebURL, "headSha": snap.HeadSHA, "author": snap.Author, "reviewers": nonNil(snap.Reviewers)},
		"files":        files,
		"omittedFiles": omitted,
		"commits":      commits,
		"redactions":   0,
		"severities":   strings.Split(reviewSeverityList, ","),
		"teamGuide":    h.spaceGuide(userID, lc.ws),
		"guidance":     "Review for correctness, security and missing tests against the Jira acceptance criteria. Report only real problems; blocker/major first. Each finding: severity, file, line (new side), title, body, optional suggestion. Do not repeat earlier findings that were rejected.",
	}

	// Jira issue with acceptance criteria.
	key := ""
	if lc.link.JiraProjectKey != "" {
		key = qalink.ExtractJiraKey(snap.Title, snap.SourceBranch, snap.Description, []string{lc.link.JiraProjectKey}, lc.link.KeyPattern)
	}
	if key != "" && lc.link.JiraConnectionID != "" {
		if jc, _, err := h.jira.loadAccessible(lc.link.JiraConnectionID, userID); err == nil {
			if cl, err := h.jira.client(*jc); err == nil {
				opt := jira.IssueOptions{AcceptanceFieldID: jiraOpt(*jc, "acFieldId")}
				if is, err := cl.Issue(ctx, key, opt); err == nil {
					ac := ""
					if is.AcceptanceCriteria != nil {
						ac = red(*is.AcceptanceCriteria)
					}
					d, _ := capRunes(red(is.Description), reviewTextCap)
					out["jira"] = gin.H{"key": is.Key, "summary": red(is.Summary), "status": is.Status, "statusCategory": is.StatusCategory, "acceptanceCriteria": ac, "description": d, "url": is.WebURL}
				} else {
					out["jira"] = gin.H{"key": key, "error": err.Error()}
				}
			}
		} else {
			out["jira"] = gin.H{"key": key, "error": "You can't use this link's Jira connection"}
		}
	}

	// Linked test cases (by MR ref or Jira key), visible in the link's space.
	var cases []models.TestCase
	q := h.db.Where("mr_ref = ?", reviewRef(lc.link.ID, iid))
	if key != "" {
		q = h.db.Where("mr_ref = ? OR jira_key = ?", reviewRef(lc.link.ID, iid), key)
	}
	if lc.ws != nil {
		q = q.Where("workspace_id = ?", *lc.ws)
	} else {
		q = q.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", userID)
	}
	q.Limit(50).Find(&cases)
	tc := []gin.H{}
	for _, t := range cases {
		tc = append(tc, gin.H{"id": t.ID, "title": t.Title, "status": t.Status, "jiraKey": t.JiraKey, "mrRef": t.MRRef})
	}
	out["testCases"] = tc

	// Earlier reviews of this MR (other commits).
	var prev []models.MRReview
	h.db.Where("link_id = ? AND iid = ? AND head_sha <> ? AND status IN ?", lc.link.ID, iid, snap.HeadSHA, []string{"draft", "posted", "stale"}).
		Order("created_at DESC").Limit(5).Find(&prev)
	pv := []gin.H{}
	for i := range prev {
		fs := []gin.H{}
		for _, f := range reviewFindings(&prev[i]) {
			fs = append(fs, gin.H{"severity": f.Severity, "file": f.File, "line": f.Line, "title": f.Title, "status": f.Status})
		}
		pv = append(pv, gin.H{"headSha": prev[i].HeadSHA, "status": prev[i].Status, "summary": prev[i].Summary, "findings": fs})
	}
	out["previousReviews"] = pv

	if c.Query("claim") == "1" {
		r, code, msg := h.claimReview(userID, agent, lc, iid, snap)
		switch {
		case code == 0:
			out["review"] = gin.H{"id": r.ID, "status": r.Status, "claimed": true}
		case r != nil:
			out["review"] = gin.H{"id": r.ID, "status": r.Status, "claimed": false, "note": msg}
		default:
			out["review"] = gin.H{"claimed": false, "note": msg}
		}
	}
	out["redactions"] = redactions
	h.audit(userID, nil, lc.link.ID, iid, snap.HeadSHA, "context", agent, "", fmt.Sprintf("%d files, %d omitted, %d redactions", len(files), len(omitted), redactions))
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ── submit ────────────────────────────────────────────────────────────

type submitInput struct {
	Summary  string `json:"summary"`
	Model    string `json:"model"`
	Findings []struct {
		Severity   string `json:"severity"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		Title      string `json:"title"`
		Body       string `json:"body"`
		Suggestion string `json:"suggestion"`
	} `json:"findings"`
}

// Submit — POST /api/qa/reviews/:id/submit (the claimer's agent).
func (h *QARadarHandler) Submit(c *gin.Context) {
	userID := c.GetString("userID")
	var r models.MRReview
	if !qaLoad(h.db, c, userID, c.Param("id"), &r, ViewerRole, "Review not found") {
		return
	}
	if r.ClaimedBy != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the person who claimed this review can submit it"})
		return
	}
	if r.Status != "claimed" && r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "This review is " + r.Status})
		return
	}
	var in submitInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.Findings) > reviewMaxFindings {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d findings", reviewMaxFindings)})
		return
	}
	fs := make([]models.ReviewFinding, 0, len(in.Findings))
	for i, f := range in.Findings {
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if _, ok := reviewSeverities[sev]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("finding %d: severity must be one of %s", i+1, reviewSeverityList)})
			return
		}
		title, _ := capRunes(strings.TrimSpace(f.Title), 300)
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("finding %d: title is required", i+1)})
			return
		}
		body, _ := capRunes(f.Body, reviewFindingCap)
		sug, _ := capRunes(f.Suggestion, reviewFindingCap)
		title, _ = notecards.Redact(title)
		body, _ = notecards.Redact(body)
		sug, _ = notecards.Redact(sug)
		line := f.Line
		if line < 0 {
			line = 0
		}
		fs = append(fs, models.ReviewFinding{ID: strconv.Itoa(i + 1), Severity: sev, File: strings.TrimSpace(f.File), Line: line,
			Title: title, Body: body, Suggestion: sug, Status: "pending"})
	}
	sort.SliceStable(fs, func(a, b int) bool { return reviewSeverities[fs[a].Severity] < reviewSeverities[fs[b].Severity] })
	summary, _ := capRunes(in.Summary, reviewTextCap)
	summary, _ = notecards.Redact(summary)
	model, _ := capRunes(strings.TrimSpace(in.Model), 100)
	r.Summary, r.Model, r.Status = summary, model, "draft"
	if a := noteAgent(c); a != "" {
		r.Agent = a
	}
	setReviewFindings(&r, fs)

	// A head that moved since the claim makes the review stale (kept, shown
	// as such; findings may not match the diff any more).
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	if lc, ok := h.linkAccessQuiet(ctx, userID, r.LinkID); ok {
		if snap, err := lc.snapshot(ctx, r.IID, false); err == nil && snap.HeadSHA != r.HeadSHA {
			r.Status = "stale"
		}
	}
	if err := h.db.Save(&r).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the review"})
		return
	}
	h.audit(userID, &r, r.LinkID, r.IID, r.HeadSHA, "submit", r.Agent, r.Model, fmt.Sprintf("%d findings", len(fs)))
	invalidateQACaches(userID, r.WorkspaceID)
	c.JSON(http.StatusOK, gin.H{"data": reviewDTO(&r)})
}

// linkAccessQuiet is linkAccess without writing a response.
func (h *QARadarHandler) linkAccessQuiet(ctx context.Context, userID, linkID string) (*linkCtx, bool) {
	w := discardWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(&w)
	return h.linkAccess(ctx, c, userID, linkID)
}

type discardWriter struct {
	header http.Header
	status int
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(s int)           { w.status = s }

// ── list / get ────────────────────────────────────────────────────────

// List — GET /api/qa/reviews?workspaceId=&linkId=&iid=&status=.
func (h *QARadarHandler) ListReviews(c *gin.Context) {
	userID := c.GetString("userID")
	scope, ok := qaListScopeAny(h.db, c, userID)
	if !ok {
		return
	}
	q := scope
	if v := c.Query("linkId"); v != "" {
		q = q.Where("link_id = ?", v)
	}
	if v := c.Query("iid"); v != "" {
		q = q.Where("iid = ?", v)
	}
	if v := c.Query("status"); v != "" {
		q = q.Where("status = ?", v)
	}
	var rows []models.MRReview
	q.Order("updated_at DESC").Limit(200).Find(&rows)
	out := make([]gin.H, 0, len(rows))
	memo := map[string]bool{}
	for i := range rows {
		if h.canSeeLink(userID, rows[i].LinkID, memo) {
			out = append(out, reviewDTO(&rows[i]))
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// qaListScopeAny scopes review rows: a workspace's (members), or the
// caller's personal ones (claimed by them, no workspace).
func qaListScopeAny(db *gorm.DB, c *gin.Context, userID string) (*gorm.DB, bool) {
	ws := qaWorkspaceParam(c)
	if ws == "" {
		return db.Where("claimed_by = ? AND (workspace_id IS NULL OR workspace_id = '')", userID), true
	}
	if qaWorkspaceRole(db, ws, userID) == NoRole {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return nil, false
	}
	return db.Where("workspace_id = ?", ws), true
}

// loadReview loads a review the caller may see: a member of its space who
// can also use the link's Git connection (a review holds code and titles
// from that project). Writes 404 / 403.
func (h *QARadarHandler) loadReview(c *gin.Context, userID string, r *models.MRReview) bool {
	if !qaLoad(h.db, c, userID, c.Param("id"), r, ViewerRole, "Review not found") {
		return false
	}
	if !h.canSeeLink(userID, r.LinkID, nil) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't use this project's Git connection — map your own in Hub → Settings"})
		return false
	}
	return true
}

// canSeeLink: the caller can use the link's effective Git connection
// (their mapping, else the link's). memo caches answers per request.
func (h *QARadarHandler) canSeeLink(userID, linkID string, memo map[string]bool) bool {
	if v, ok := memo[linkID]; ok {
		return v
	}
	ok := false
	var l models.QAProjectLink
	if h.db.Where("id = ?", linkID).First(&l).Error == nil {
		connID := l.GitConnectionID
		if m, has := qaLinkMaps(h.db, userID, []models.QAProjectLink{l})[l.ID]; has && m.GitConnectionID != "" {
			connID = m.GitConnectionID
		}
		_, _, err := h.git.conns.loadAccessible(connID, userID)
		ok = err == nil
	}
	if memo != nil {
		memo[linkID] = ok
	}
	return ok
}

// GetReview — GET /api/qa/reviews/:id.
func (h *QARadarHandler) GetReview(c *gin.Context) {
	userID := c.GetString("userID")
	var r models.MRReview
	if !h.loadReview(c, userID, &r) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": reviewDTO(&r)})
}

// ── decide (approve / reject / edit) ──────────────────────────────────

// canApprove (O3): the MR's author, one of its reviewers (by the caller's
// own Git identity) or the person whose agent wrote the review.
func canApprove(r *models.MRReview, userID, me string) bool {
	if r.ClaimedBy == userID {
		return true
	}
	if me == "" {
		return false
	}
	if strings.EqualFold(r.AuthorUsername, me) {
		return true
	}
	for _, u := range strings.Split(r.ReviewerUsernames, ",") {
		if strings.EqualFold(strings.TrimSpace(u), me) {
			return true
		}
	}
	return false
}

type decideInput struct {
	Status     string  `json:"status"` // approved | rejected | pending
	Title      *string `json:"title"`
	Body       *string `json:"body"`
	Severity   *string `json:"severity"`
	Suggestion *string `json:"suggestion"`
}

// Decide — PATCH /api/qa/reviews/:id/findings/:fid.
func (h *QARadarHandler) Decide(c *gin.Context) {
	userID := c.GetString("userID")
	var r models.MRReview
	if !h.loadReview(c, userID, &r) {
		return
	}
	var in decideInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	me := ""
	if lc, ok := h.linkAccessQuiet(ctx, userID, r.LinkID); ok {
		me = lc.me
	}
	if !canApprove(&r, userID, me) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the MR's author, its reviewers or the review's owner can decide findings"})
		return
	}
	fs := reviewFindings(&r)
	idx := -1
	for i := range fs {
		if fs[i].ID == c.Param("fid") {
			idx = i
		}
	}
	if idx < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Finding not found"})
		return
	}
	f := &fs[idx]
	if f.Status == "posted" {
		c.JSON(http.StatusConflict, gin.H{"error": "This finding is already posted"})
		return
	}
	if in.Title != nil {
		t, _ := capRunes(strings.TrimSpace(*in.Title), 300)
		if t != "" {
			f.Title = t
		}
	}
	if in.Body != nil {
		f.Body, _ = capRunes(*in.Body, reviewFindingCap)
	}
	if in.Suggestion != nil {
		f.Suggestion, _ = capRunes(*in.Suggestion, reviewFindingCap)
	}
	if in.Severity != nil {
		if _, ok := reviewSeverities[*in.Severity]; ok {
			f.Severity = *in.Severity
		}
	}
	switch in.Status {
	case "approved", "rejected", "pending":
		f.Status, f.DecidedBy = in.Status, userID
	case "":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be approved, rejected or pending"})
		return
	}
	setReviewFindings(&r, fs)
	if err := h.db.Save(&r).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	invalidateQACaches(userID, r.WorkspaceID)
	h.audit(userID, &r, r.LinkID, r.IID, r.HeadSHA, "decide", "", "", f.ID+" "+f.Status)
	c.JSON(http.StatusOK, gin.H{"data": reviewDTO(&r)})
}

// Discard — POST /api/qa/reviews/:id/discard (claimer or approver).
func (h *QARadarHandler) Discard(c *gin.Context) {
	userID := c.GetString("userID")
	var r models.MRReview
	if !h.loadReview(c, userID, &r) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	me := ""
	if lc, ok := h.linkAccessQuiet(ctx, userID, r.LinkID); ok {
		me = lc.me
	}
	if !canApprove(&r, userID, me) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the MR's author, its reviewers or the review's owner can discard it"})
		return
	}
	r.Status = "discarded"
	h.db.Save(&r)
	invalidateQACaches(userID, r.WorkspaceID)
	h.audit(userID, &r, r.LinkID, r.IID, r.HeadSHA, "discard", "", "", "")
	c.JSON(http.StatusOK, gin.H{"data": reviewDTO(&r)})
}

// ── post ──────────────────────────────────────────────────────────────

// Post — POST /api/qa/reviews/:id/post: posts the approved, not yet posted
// findings (and the summary once) to the MR with the caller's credentials.
// A connection's stored token needs a write grant; the caller's own linked
// account is checked by GitLab / GitHub themselves.
func (h *QARadarHandler) Post(c *gin.Context) {
	userID := c.GetString("userID")
	var r models.MRReview
	if !qaLoad(h.db, c, userID, c.Param("id"), &r, ViewerRole, "Review not found") {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, r.LinkID)
	if !ok {
		return
	}
	if !canApprove(&r, userID, lc.me) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the MR's author, its reviewers or the review's owner can post findings"})
		return
	}
	if !lc.ownIdentity(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errNotYourIdentity})
		return
	}
	if r.Status == "stale" {
		c.JSON(http.StatusConflict, gin.H{"error": "The MR has newer commits than this review; review it again"})
		return
	}
	snap, err := lc.snapshot(ctx, r.IID, false)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if snap.HeadSHA != r.HeadSHA {
		r.Status = "stale"
		h.db.Save(&r)
		c.JSON(http.StatusConflict, gin.H{"error": "The MR has newer commits than this review; review it again"})
		return
	}
	fs := reviewFindings(&r)
	footer := "\n\n---\n_PatchMan AI review"
	if r.Agent != "" || r.Model != "" {
		footer += " (" + strings.Trim(strings.Join([]string{r.Agent, r.Model}, ", "), ", ") + ")"
	}
	footer += ", approved by a person_"
	posted, failed := 0, 0
	if r.SummaryNoteID == "" && strings.TrimSpace(r.Summary) != "" {
		if id, _, err := lc.postNote(ctx, r.IID, "**AI review summary**\n\n"+r.Summary+footer, snap, "", 0); err == nil {
			r.SummaryNoteID = id
		}
	}
	for i := range fs {
		f := &fs[i]
		if f.Status != "approved" {
			continue
		}
		body := fmt.Sprintf("**[%s] %s**\n\n%s", f.Severity, f.Title, f.Body)
		if f.Suggestion != "" {
			body += "\n\n```suggestion\n" + f.Suggestion + "\n```"
		}
		body += footer
		id, inline, err := lc.postNote(ctx, r.IID, body, snap, f.File, f.Line)
		if err != nil {
			f.PostError, _ = capRunes(err.Error(), 300)
			failed++
			continue
		}
		f.Status, f.NoteID, f.Inline, f.PostError = "posted", id, inline, ""
		posted++
	}
	setReviewFindings(&r, fs)
	if posted > 0 || r.SummaryNoteID != "" {
		now := time.Now()
		r.PostedAt, r.PostedBy, r.Status = &now, userID, "posted"
	}
	h.db.Save(&r)
	invalidateQACaches(userID, r.WorkspaceID)
	h.audit(userID, &r, r.LinkID, r.IID, r.HeadSHA, "post", "", "", fmt.Sprintf("%d posted, %d failed", posted, failed))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"review": reviewDTO(&r), "posted": posted, "failed": failed}})
}

// postNote posts one comment: on the diff line when file + line are given
// and the provider accepts the position, else as a general comment with the
// location in the text. Returns the note id and whether it went inline.
func (lc *linkCtx) postNote(ctx context.Context, iid int64, body string, snap *mrSnapshot, file string, line int) (string, bool, error) {
	if isGitHub(lc.conn) {
		repo, err := lc.ghRepo(ctx)
		if err != nil {
			return "", false, err
		}
		base := lc.f.h.ghBase(lc.conn) + repo
		n := strconv.FormatInt(iid, 10)
		if file != "" && line > 0 {
			b, st, err := lc.f.send(ctx, http.MethodPost, base+"/pulls/"+n+"/comments",
				map[string]any{"body": body, "commit_id": snap.HeadSHA, "path": file, "line": line, "side": "RIGHT"})
			if err == nil && st/100 == 2 {
				return noteID(b), true, nil
			}
		}
		if file != "" {
			body = fmt.Sprintf("`%s:%d`\n\n%s", file, line, body)
		}
		b, st, err := lc.f.send(ctx, http.MethodPost, base+"/issues/"+n+"/comments", map[string]any{"body": body})
		if err != nil {
			return "", false, err
		}
		if st/100 != 2 {
			return "", false, fmt.Errorf("github %d: %s", st, remoteMessage(b))
		}
		return noteID(b), false, nil
	}
	mr := lc.mrBase(iid)
	if file != "" && line > 0 && snap.BaseSHA != "" {
		pos := map[string]any{"position_type": "text", "base_sha": snap.BaseSHA, "start_sha": snap.StartSHA, "head_sha": snap.HeadSHA,
			"new_path": file, "old_path": file, "new_line": line}
		b, st, err := lc.f.send(ctx, http.MethodPost, mr+"/discussions", map[string]any{"body": body, "position": pos})
		if err == nil && st/100 == 2 {
			return noteID(b), true, nil
		}
	}
	if file != "" {
		loc := file
		if line > 0 {
			loc += ":" + strconv.Itoa(line)
		}
		body = "`" + loc + "`\n\n" + body
	}
	b, st, err := lc.f.send(ctx, http.MethodPost, mr+"/discussions", map[string]any{"body": body})
	if err != nil {
		return "", false, err
	}
	if st/100 != 2 {
		return "", false, fmt.Errorf("gitlab %d: %s", st, remoteMessage(b))
	}
	return noteID(b), false, nil
}

func noteID(b []byte) string {
	var v struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(b, &v)
	switch x := v.ID.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}
