package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notecards"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Fix verification (CR_QA_P4_RFC §1). After findings were posted and the
// author pushed, a person's agent checks each open finding against the
// changes since the reviewed commit and gives a verdict. Replies and thread
// resolution go to GitLab / GitHub only on a person's click, under their
// own name.

var verifyVerdicts = map[string]bool{"fixed": true, "not_fixed": true, "partly": true, "obsolete": true}

// verifySource: the MR's latest posted review that still has open findings
// and was made at a commit older than head ("" head = any).
func (h *QARadarHandler) verifySource(linkID string, iid int64, head string) (*models.MRReview, []models.ReviewFinding) {
	var rows []models.MRReview
	h.db.Where("link_id = ? AND iid = ? AND status = ?", linkID, iid, "posted").Order("updated_at DESC").Find(&rows)
	for i := range rows {
		if head != "" && rows[i].HeadSHA == head {
			continue
		}
		open := []models.ReviewFinding{}
		for _, f := range reviewFindings(&rows[i]) {
			if f.FindingOpen() {
				open = append(open, f)
			}
		}
		if len(open) > 0 {
			return &rows[i], open
		}
	}
	return nil, nil
}

func (h *QARadarHandler) spaceGuide(userID string, ws *string) string {
	g, _ := capRunes(strings.TrimSpace(loadQASettings(h.db, userID, ws).ReviewGuide), 8000)
	return g
}

// compareFiles: files changed between two commits (the diff since the
// reviewed commit), secret-redacted by the caller.
func (lc *linkCtx) compareFiles(ctx context.Context, from, to string) ([]mrFile, error) {
	out := []mrFile{}
	if isGitHub(lc.conn) {
		repo, err := lc.ghRepo(ctx)
		if err != nil {
			return nil, err
		}
		var raw struct {
			Files []struct {
				Filename         string `json:"filename"`
				PreviousFilename string `json:"previous_filename"`
				Status           string `json:"status"`
				Patch            string `json:"patch"`
			} `json:"files"`
		}
		if _, err := lc.f.get(ctx, lc.f.h.ghBase(lc.conn)+repo+"/compare/"+from+"..."+to, &raw); err != nil {
			return nil, err
		}
		for _, f := range raw.Files {
			out = append(out, mrFile{Path: f.Filename, OldPath: f.PreviousFilename, Status: f.Status, Diff: f.Patch})
		}
		return out, nil
	}
	var raw struct {
		Diffs []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			NewFile     bool   `json:"new_file"`
			DeletedFile bool   `json:"deleted_file"`
			Diff        string `json:"diff"`
		} `json:"diffs"`
	}
	u := lc.f.glBase + projectPath(lc.link.GitProject) + "/repository/compare?straight=true&from=" + from + "&to=" + to
	if _, err := lc.f.get(ctx, u, &raw); err != nil {
		return nil, err
	}
	for _, d := range raw.Diffs {
		st := "modified"
		if d.NewFile {
			st = "added"
		} else if d.DeletedFile {
			st = "deleted"
		}
		f := mrFile{Path: d.NewPath, Status: st, Diff: d.Diff}
		if d.OldPath != d.NewPath {
			f.OldPath = d.OldPath
		}
		out = append(out, f)
	}
	return out, nil
}

// threadResolved reads a GitLab discussion's resolved flag (nil = unknown).
func (lc *linkCtx) threadResolved(ctx context.Context, iid int64, discussionID string) *bool {
	if isGitHub(lc.conn) || discussionID == "" {
		return nil
	}
	var d struct {
		Notes []struct {
			Resolvable bool `json:"resolvable"`
			Resolved   bool `json:"resolved"`
		} `json:"notes"`
	}
	if _, err := lc.f.get(ctx, lc.mrBase(iid)+"/discussions/"+discussionID, &d); err != nil || len(d.Notes) == 0 || !d.Notes[0].Resolvable {
		return nil
	}
	v := d.Notes[0].Resolved
	return &v
}

// VerifyContext — GET /api/qa/verify-context?linkId=&iid=&claim=1: the open
// findings of the last posted review, what changed since it, and the team's
// review guide.
func (h *QARadarHandler) VerifyContext(c *gin.Context) {
	userID := c.GetString("userID")
	iid, ok := mrIID(c, c.Query("iid"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, c.Query("linkId"))
	if !ok {
		return
	}
	snap, err := lc.snapshot(ctx, iid, false)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	src, open := h.verifySource(lc.link.ID, iid, snap.HeadSHA)
	if src == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"nothingToVerify": true, "mr": gin.H{"iid": iid, "headSha": snap.HeadSHA, "title": snap.Title}}})
		return
	}
	redactions := 0
	red := func(s string) string {
		o, n := notecards.Redact(s)
		redactions += n
		return o
	}
	// A rebase / force-push can drop the reviewed commit: then the agent gets
	// the findings and a note to read the whole MR diff instead.
	compareNote := ""
	files, err := lc.compareFiles(ctx, src.HeadSHA, snap.HeadSHA)
	if err != nil {
		files = nil
		compareNote = "The changes since the reviewed commit could not be read (the branch was probably rebased or force-pushed). Read the MR's current diff with qa_mr_context (without claim) and judge each finding against it."
	}
	kept, omitted := []mrFile{}, []string{}
	budget := reviewDiffBudget
	for _, f := range files {
		d := red(f.Diff)
		if len(d) > reviewFileBudget {
			d, f.Truncated = d[:reviewFileBudget], true
		}
		if budget-len(d) < 0 {
			omitted = append(omitted, f.Path)
			continue
		}
		budget -= len(d)
		f.Diff = d
		kept = append(kept, f)
	}
	fs := []gin.H{}
	for _, f := range open {
		item := gin.H{"id": f.ID, "severity": f.Severity, "file": f.File, "line": f.Line, "title": f.Title, "body": f.Body, "suggestion": f.Suggestion}
		if v := lc.threadResolved(ctx, iid, f.NoteID); v != nil {
			item["threadResolved"] = *v
		}
		if f.Verdict != "" {
			item["previousVerdict"] = f.Verdict
		}
		fs = append(fs, item)
	}
	out := gin.H{
		"link":         gin.H{"id": lc.link.ID, "project": lc.link.GitProject, "provider": providerOf(lc.conn)},
		"mr":           gin.H{"iid": iid, "title": red(snap.Title), "headSha": snap.HeadSHA, "webUrl": snap.WebURL, "author": snap.Author},
		"reviewId":     src.ID,
		"fromSha":      src.HeadSHA,
		"findings":     fs,
		"files":        kept,
		"omittedFiles": omitted,
		"teamGuide":    h.spaceGuide(userID, lc.ws),
		"verdicts":     []string{"fixed", "not_fixed", "partly", "obsolete"},
		"guidance":     "For each finding decide from the changes since the reviewed commit: fixed (the problem is gone), not_fixed (still there), partly (improved but not done) or obsolete (the code it was about was removed or rewritten so it no longer applies). Give one line of evidence (file:line or what changed). Don't raise new findings here.",
	}
	if compareNote != "" {
		out["compareNote"] = compareNote
	}
	if c.Query("claim") == "1" {
		var v models.MRVerification
		found := false
		if q := h.db.Where("review_id = ? AND head_sha = ?", src.ID, snap.HeadSHA).Limit(1).Find(&v); q.Error == nil && q.RowsAffected > 0 {
			found = true
		}
		switch {
		case found && v.Status != "claimed":
			out["verification"] = gin.H{"id": v.ID, "status": v.Status, "claimed": false, "note": "This commit was already verified"}
		case found && v.ClaimedBy != userID && h.now().Sub(v.UpdatedAt) < reviewClaimTTL:
			out["verification"] = gin.H{"id": v.ID, "status": v.Status, "claimed": false, "note": "Another person's agent is verifying this commit"}
		default:
			if !found {
				v = models.MRVerification{ID: uuid.NewString(), WorkspaceID: lc.ws, LinkID: lc.link.ID, IID: iid, ReviewID: src.ID, FromSHA: src.HeadSHA, HeadSHA: snap.HeadSHA}
			}
			v.Status, v.ClaimedBy, v.Agent = "claimed", userID, noteAgent(c)
			if err := h.db.Save(&v).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim"})
				return
			}
			out["verification"] = gin.H{"id": v.ID, "status": v.Status, "claimed": true}
			invalidateQACaches(userID, lc.ws)
		}
	}
	out["redactions"] = redactions
	h.audit(userID, src, lc.link.ID, iid, snap.HeadSHA, "verify_context", noteAgent(c), "", fmt.Sprintf("%d findings, %d files", len(fs), len(kept)))
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func verificationDTO(v *models.MRVerification) gin.H {
	vs := []models.FindingVerdict{}
	if v.VerdictsJSON != "" {
		_ = json.Unmarshal([]byte(v.VerdictsJSON), &vs)
	}
	counts := map[string]int{}
	for _, x := range vs {
		counts[x.Verdict]++
	}
	return gin.H{"id": v.ID, "linkId": v.LinkID, "iid": v.IID, "reviewId": v.ReviewID, "fromSha": v.FromSHA, "headSha": v.HeadSHA,
		"status": v.Status, "claimedBy": v.ClaimedBy, "agent": v.Agent, "model": v.Model, "summary": v.Summary,
		"verdicts": vs, "counts": counts, "createdAt": v.CreatedAt, "updatedAt": v.UpdatedAt}
}

type verifySubmitInput struct {
	Summary  string `json:"summary"`
	Model    string `json:"model"`
	Verdicts []struct {
		FindingID string `json:"findingId"`
		Verdict   string `json:"verdict"`
		Evidence  string `json:"evidence"`
	} `json:"verdicts"`
}

// SubmitVerification — POST /api/qa/verifications/:id/submit (the claimer).
func (h *QARadarHandler) SubmitVerification(c *gin.Context) {
	userID := c.GetString("userID")
	var v models.MRVerification
	if !qaLoad(h.db, c, userID, c.Param("id"), &v, ViewerRole, "Verification not found") {
		return
	}
	if v.ClaimedBy != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the person who claimed this check can submit it"})
		return
	}
	if v.Status == "applied" {
		c.JSON(http.StatusConflict, gin.H{"error": "This check was already applied"})
		return
	}
	var in verifySubmitInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var r models.MRReview
	if err := h.db.Where("id = ?", v.ReviewID).First(&r).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "The review is gone"})
		return
	}
	fs := reviewFindings(&r)
	byID := map[string]int{}
	for i := range fs {
		byID[fs[i].ID] = i
	}
	out := []models.FindingVerdict{}
	for i, x := range in.Verdicts {
		if !verifyVerdicts[x.Verdict] {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("verdict %d: must be fixed, not_fixed, partly or obsolete", i+1)})
			return
		}
		idx, ok := byID[x.FindingID]
		if ok && fs[idx].Status != "posted" {
			ok = false
		}
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("verdict %d: unknown finding %q", i+1, x.FindingID)})
			return
		}
		ev, _ := capRunes(strings.TrimSpace(x.Evidence), 1000)
		ev, _ = notecards.Redact(ev)
		out = append(out, models.FindingVerdict{FindingID: x.FindingID, Verdict: x.Verdict, Evidence: ev})
		fs[idx].Verdict, fs[idx].VerifiedSHA = x.Verdict, v.HeadSHA
	}
	b, _ := json.Marshal(out)
	v.VerdictsJSON, v.Status = string(b), "submitted"
	v.Summary, _ = capRunes(in.Summary, reviewTextCap)
	v.Summary, _ = notecards.Redact(v.Summary)
	v.Model, _ = capRunes(strings.TrimSpace(in.Model), 100)
	if a := noteAgent(c); a != "" {
		v.Agent = a
	}
	setReviewFindings(&r, fs)
	h.db.Save(&r)
	h.db.Save(&v)
	invalidateQACaches(userID, v.WorkspaceID)
	h.audit(userID, &r, v.LinkID, v.IID, v.HeadSHA, "verify_submit", v.Agent, v.Model, fmt.Sprintf("%d verdicts", len(out)))
	c.JSON(http.StatusOK, gin.H{"data": verificationDTO(&v)})
}

// ListVerifications — GET /api/qa/verifications?linkId=&iid=.
func (h *QARadarHandler) ListVerifications(c *gin.Context) {
	userID := c.GetString("userID")
	linkID := c.Query("linkId")
	if !h.canSeeLink(userID, linkID, nil) {
		c.JSON(http.StatusOK, gin.H{"data": []gin.H{}})
		return
	}
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, linkID, &l, ViewerRole, "Link not found") {
		return
	}
	var rows []models.MRVerification
	h.db.Where("link_id = ? AND iid = ?", linkID, c.Query("iid")).Order("updated_at DESC").Limit(20).Find(&rows)
	out := make([]gin.H, 0, len(rows))
	for i := range rows {
		out = append(out, verificationDTO(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type verifyApplyInput struct {
	Resolve []string `json:"resolve"` // finding ids judged fixed / obsolete: reply + resolve the thread
	Reply   []string `json:"reply"`   // finding ids judged not_fixed / partly: reply with the evidence
}

// ApplyVerification — POST /api/qa/verifications/:id/apply: the person's
// click that sends the replies (and resolves GitLab threads of fixed
// findings) under their own name.
func (h *QARadarHandler) ApplyVerification(c *gin.Context) {
	userID := c.GetString("userID")
	var v models.MRVerification
	if !qaLoad(h.db, c, userID, c.Param("id"), &v, ViewerRole, "Verification not found") {
		return
	}
	var in verifyApplyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, v.LinkID)
	if !ok {
		return
	}
	var r models.MRReview
	if err := h.db.Where("id = ?", v.ReviewID).First(&r).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "The review is gone"})
		return
	}
	if !canApprove(&r, userID, lc.me) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the MR's author, its reviewers or the review's owner can apply a fix check"})
		return
	}
	if !lc.ownIdentity(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errNotYourIdentity})
		return
	}
	// Like posting a stale review: verdicts are about one commit.
	if snap, err := lc.snapshot(ctx, v.IID, false); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	} else if snap.HeadSHA != v.HeadSHA {
		c.JSON(http.StatusConflict, gin.H{"error": "The MR got new commits since this fix check; run it again"})
		return
	}
	var vs []models.FindingVerdict
	_ = json.Unmarshal([]byte(v.VerdictsJSON), &vs)
	fs := reviewFindings(&r)
	fIdx := map[string]int{}
	for i := range fs {
		fIdx[fs[i].ID] = i
	}
	want := map[string]string{}
	for _, id := range in.Resolve {
		want[id] = "resolve"
	}
	for _, id := range in.Reply {
		want[id] = "reply"
	}
	short := v.HeadSHA
	if len(short) > 8 {
		short = short[:8]
	}
	sent, failed := 0, 0
	for i := range vs {
		action := want[vs[i].FindingID]
		fi, ok := fIdx[vs[i].FindingID]
		if action == "" || !ok || vs[i].Applied != "" {
			continue
		}
		f := &fs[fi]
		var body string
		if action == "resolve" {
			if vs[i].Verdict != "fixed" && vs[i].Verdict != "obsolete" {
				continue
			}
			body = fmt.Sprintf("✅ Verified %s in %s: %s\n\n_PatchMan fix check, applied by a person_", strings.ReplaceAll(vs[i].Verdict, "_", " "), short, vs[i].Evidence)
		} else {
			if vs[i].Verdict != "not_fixed" && vs[i].Verdict != "partly" {
				continue
			}
			body = fmt.Sprintf("🔁 Still open in %s (%s): %s\n\n_PatchMan fix check, applied by a person_", short, strings.ReplaceAll(vs[i].Verdict, "_", " "), vs[i].Evidence)
		}
		if err := lc.replyThread(ctx, v.IID, f, body, action == "resolve"); err != nil {
			vs[i].ApplyError, _ = capRunes(err.Error(), 300)
			failed++
			continue
		}
		if action == "resolve" {
			vs[i].Applied, f.Resolved = "resolved", true
		} else {
			vs[i].Applied = "replied"
		}
		vs[i].ApplyError = ""
		sent++
	}
	b, _ := json.Marshal(vs)
	v.VerdictsJSON = string(b)
	if sent > 0 {
		v.Status = "applied"
	}
	setReviewFindings(&r, fs)
	h.db.Save(&r)
	h.db.Save(&v)
	invalidateQACaches(userID, v.WorkspaceID)
	h.audit(userID, &r, v.LinkID, v.IID, v.HeadSHA, "verify_apply", "", "", fmt.Sprintf("%d sent, %d failed", sent, failed))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"verification": verificationDTO(&v), "sent": sent, "failed": failed}})
}

// replyThread replies to a finding's thread and, with resolve, resolves it
// (GitLab). GitHub: a reply to the review comment (or a new comment);
// GitHub threads are not resolved from here.
func (lc *linkCtx) replyThread(ctx context.Context, iid int64, f *models.ReviewFinding, body string, resolve bool) error {
	n := strconv.FormatInt(iid, 10)
	if isGitHub(lc.conn) {
		repo, err := lc.ghRepo(ctx)
		if err != nil {
			return err
		}
		base := lc.f.h.ghBase(lc.conn) + repo
		if f.Inline && f.NoteID != "" {
			b, st, err := lc.f.send(ctx, http.MethodPost, base+"/pulls/"+n+"/comments/"+f.NoteID+"/replies", map[string]any{"body": body})
			if err == nil && st/100 == 2 {
				return nil
			}
			if err == nil {
				_ = b
			}
		}
		b, st, err := lc.f.send(ctx, http.MethodPost, base+"/issues/"+n+"/comments", map[string]any{"body": "> " + f.Title + "\n\n" + body})
		if err != nil {
			return err
		}
		if st/100 != 2 {
			return fmt.Errorf("github %d: %s", st, remoteMessage(b))
		}
		return nil
	}
	mr := lc.mrBase(iid)
	if f.NoteID == "" {
		b, st, err := lc.f.send(ctx, http.MethodPost, mr+"/discussions", map[string]any{"body": "> " + f.Title + "\n\n" + body})
		if err != nil {
			return err
		}
		if st/100 != 2 {
			return fmt.Errorf("gitlab %d: %s", st, remoteMessage(b))
		}
		return nil
	}
	b, st, err := lc.f.send(ctx, http.MethodPost, mr+"/discussions/"+f.NoteID+"/notes", map[string]any{"body": body})
	if err != nil {
		return err
	}
	if st/100 != 2 {
		return fmt.Errorf("gitlab %d: %s", st, remoteMessage(b))
	}
	if resolve {
		b, st, err = lc.f.send(ctx, http.MethodPut, mr+"/discussions/"+f.NoteID+"?resolved=true", nil)
		if err != nil {
			return err
		}
		if st/100 != 2 {
			return fmt.Errorf("replied, but resolving failed: gitlab %d: %s", st, remoteMessage(b))
		}
	}
	return nil
}
