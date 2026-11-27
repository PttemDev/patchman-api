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
	"patchman-be/internal/qalink"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MR tests (CR_QA_RFC P2): the test cases linked to an MR (by its ref or its
// Jira key), runs of them against the link's test environment (one click =
// one run; O7) and the results posted to the MR or the Jira issue on a
// person's click with their own credentials.

// mrCases: the cases linked to the MR in the link's space.
func mrCases(db *gorm.DB, userID string, ws *string, ref, jiraKey string) []models.TestCase {
	q := db.Where("mr_ref = ?", ref)
	if jiraKey != "" {
		q = db.Where("mr_ref = ? OR jira_key = ?", ref, jiraKey)
	}
	if ws != nil && *ws != "" {
		q = q.Where("workspace_id = ?", *ws)
	} else {
		q = q.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", userID)
	}
	var cases []models.TestCase
	q.Order("created_at ASC").Limit(200).Find(&cases)
	return cases
}

func (lc *linkCtx) jiraKeyOf(snap *mrSnapshot) string {
	if lc.link.JiraProjectKey == "" {
		return ""
	}
	return qalink.ExtractJiraKey(snap.Title, snap.SourceBranch, snap.Description, []string{lc.link.JiraProjectKey}, lc.link.KeyPattern)
}

func mrIID(c *gin.Context, raw string) (int64, bool) {
	iid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || iid <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iid is required"})
		return 0, false
	}
	return iid, true
}

// MRTests — GET /api/qa/mr-tests?linkId=&iid=.
func (h *QARadarHandler) MRTests(c *gin.Context) {
	userID := c.GetString("userID")
	iid, ok := mrIID(c, c.Query("iid"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
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
	ref := reviewRef(lc.link.ID, iid)
	key := lc.jiraKeyOf(snap)
	cases := mrCases(h.db, userID, lc.ws, ref, key)
	out := make([]gin.H, 0, len(cases))
	for _, t := range cases {
		out = append(out, gin.H{"id": t.ID, "title": t.Title, "status": t.Status, "type": t.Type, "jiraKey": t.JiraKey, "mrRef": t.MRRef, "executable": hasExecutableSteps(t.Steps)})
	}
	var runs []models.TestRun
	h.db.Where("mr_ref = ?", ref).Order("started_at DESC").Limit(10).Find(&runs)
	rs := make([]gin.H, 0, len(runs))
	for _, r := range runs {
		rs = append(rs, serializeTestRun(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ref": ref, "jiraKey": key, "headSha": snap.HeadSHA, "environmentId": lc.link.TestEnvironment,
		"cases": out, "runs": rs,
	}})
}

type mrRunInput struct {
	LinkID        string `json:"linkId" binding:"required"`
	IID           int64  `json:"iid" binding:"required"`
	EnvironmentID string `json:"environmentId"` // "" = the link's test environment
}

// RunMRTests — POST /api/qa/mr-tests/run: runs the MR's executable linked
// cases once and records a TestRun tagged with the MR (and Jira key).
func (h *QARadarHandler) RunMRTests(c *gin.Context) {
	userID := c.GetString("userID")
	var in mrRunInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "linkId and iid are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	lc, ok := h.linkAccess(ctx, c, userID, in.LinkID)
	if !ok {
		cancel()
		return
	}
	if lc.ws != nil && !qaWorkspaceRole(h.db, *lc.ws, userID).AtLeast(EditorRole) {
		cancel()
		c.JSON(http.StatusForbidden, gin.H{"error": qaForbidden(EditorRole)})
		return
	}
	snap, err := lc.snapshot(ctx, in.IID, false)
	cancel()
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	env := in.EnvironmentID
	if env == "" {
		env = lc.link.TestEnvironment
	}
	envVars, ok := resolveRunEnvVars(h.db, userID, env)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	ref := reviewRef(lc.link.ID, in.IID)
	key := lc.jiraKeyOf(snap)
	// Only active cases run: a draft (an agent may have written it from MR
	// text) runs only after a person reviews and activates it.
	active, drafts := []models.TestCase{}, 0
	for _, t := range mrCases(h.db, userID, lc.ws, ref, key) {
		if t.Status == "active" {
			active = append(active, t)
		} else {
			drafts++
		}
	}
	run, results, summary, err := h.runCases(c.Request.Context(), userID, lc.ws, active, envVars, env, ref, key)
	summary.Drafts = drafts
	if err != nil && drafts > 0 {
		err = fmt.Errorf("no active executable test cases — %d draft or inactive case(s) need review first (activate them in Test cases)", drafts)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"testRun": serializeTestRun(run), "testCases": results, "summary": summary, "jiraKey": key, "headSha": snap.HeadSHA}})
}

// runCases executes the executable cases sequentially into one TestRun.
func (h *QARadarHandler) runCases(ctx context.Context, userID string, ws *string, cases []models.TestCase, envVars map[string]string, env, ref, key string) (models.TestRun, []folderRunCaseResult, folderRunSummary, error) {
	return qaRunCases(ctx, h.db, models.TestRun{UserID: userID, WorkspaceID: ws, Environment: env, MRRef: ref, JiraKey: key}, cases, envVars)
}

// qaRunCases runs the executable cases into a new TestRun built from tmpl
// (owner, space, environment and tags); manual / empty cases are skipped.
func qaRunCases(ctx context.Context, db *gorm.DB, tmpl models.TestRun, cases []models.TestCase, envVars map[string]string) (models.TestRun, []folderRunCaseResult, folderRunSummary, error) {
	runnable := []models.TestCase{}
	summary := folderRunSummary{}
	for _, t := range cases {
		if hasExecutableSteps(t.Steps) {
			runnable = append(runnable, t)
		} else {
			summary.Skipped++
		}
	}
	if len(runnable) == 0 {
		return models.TestRun{}, nil, summary, fmt.Errorf("no executable test cases (%d manual or empty)", summary.Skipped)
	}
	run := tmpl
	run.ID, run.Status, run.Results, run.StartedAt = uuid.NewString(), "running", "[]", time.Now()
	if err := db.Create(&run).Error; err != nil {
		return run, nil, summary, fmt.Errorf("failed to create the run")
	}
	results := make([]folderRunCaseResult, 0, len(runnable))
	status := "passed"
	for _, t := range runnable {
		if ctx.Err() != nil {
			status = "cancelled"
			break
		}
		res := executeTestCaseSteps(t.Steps, envVars)
		results = append(results, folderRunCaseResult{TestCaseID: t.ID, Title: t.Title, FolderID: derefStr(t.FolderID), Status: res.Status, Duration: res.Duration, Result: res})
		summary.Total++
		if res.Status == "passed" {
			summary.Passed++
		} else {
			summary.Failed++
			if status == "passed" {
				status = "failed"
			}
		}
	}
	b, _ := json.Marshal(results)
	done := time.Now()
	db.Model(&run).Updates(map[string]any{"status": status, "results": string(b), "completed_at": &done})
	db.First(&run, "id = ?", run.ID)
	return run, results, summary, nil
}

type mrPostInput struct {
	LinkID string `json:"linkId" binding:"required"`
	IID    int64  `json:"iid" binding:"required"`
	RunID  string `json:"runId" binding:"required"`
	Target string `json:"target" binding:"required"` // mr | jira
}

// runReport renders a run as markdown-ish text for a comment.
func runReport(run models.TestRun) (string, int, int) {
	var rows []struct {
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(run.Results), &rows)
	passed, failed := 0, 0
	var b strings.Builder
	for _, r := range rows {
		mark := "✅"
		if r.Status == "passed" {
			passed++
		} else {
			failed++
			mark = "❌"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, r.Title)
	}
	head := fmt.Sprintf("PatchMan test run: %d passed, %d failed (%s)", passed, failed, run.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	return head + "\n" + b.String(), passed, failed
}

// PostMRTestResults — POST /api/qa/mr-tests/post {linkId, iid, runId,
// target}: a results comment on the MR (caller's Git credentials; a stored
// token needs a write grant) or on the Jira issue (a write grant on the
// Jira connection, or owning it).
func (h *QARadarHandler) PostMRTestResults(c *gin.Context) {
	userID := c.GetString("userID")
	var in mrPostInput
	if err := c.ShouldBindJSON(&in); err != nil || (in.Target != "mr" && in.Target != "jira") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "linkId, iid, runId and target (mr|jira) are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, in.LinkID)
	if !ok {
		return
	}
	var run models.TestRun
	if err := h.db.Where("id = ? AND mr_ref = ?", in.RunID, reviewRef(lc.link.ID, in.IID)).First(&run).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Run not found for this MR"})
		return
	}
	text, _, _ := runReport(run)
	if in.Target == "mr" {
		if !lc.ownIdentity(userID) {
			c.JSON(http.StatusForbidden, gin.H{"error": errNotYourIdentity})
			return
		}
		snap, err := lc.snapshot(ctx, in.IID, false)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if _, _, err := lc.postNote(ctx, in.IID, text, snap, "", 0); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		h.audit(userID, nil, lc.link.ID, in.IID, snap.HeadSHA, "post_tests_mr", "", "", run.ID)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"posted": true}})
		return
	}
	key := run.JiraKey
	if key == "" || lc.link.JiraConnectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "This run has no Jira issue"})
		return
	}
	jc, role, err := h.jira.loadAccessible(lc.link.JiraConnectionID, userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't use this link's Jira connection"})
		return
	}
	// The stored credential is its owner's: only the owner comments with it,
	// so the comment is under the commenter's own name.
	if jc.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": errNotYourJira})
		return
	}
	cl, err := h.jira.client(*jc)
	if err != nil {
		jiraFail(c, err)
		return
	}
	if err := cl.AddComment(ctx, key, text); err != nil {
		jiraFail(c, err)
		return
	}
	h.jira.auditWrite(jc, userID, role, "comment", key)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"posted": true, "key": key}})
}

type defectJiraInput struct {
	JiraConnectionID string `json:"jiraConnectionId" binding:"required"`
	ProjectKey       string `json:"projectKey" binding:"required"`
	IssueType        string `json:"issueType"` // default Bug
}

// DefectToJira — POST /api/qa/defects/:id/jira: creates a Jira issue from
// a defect (a person's click) and stores the key on the defect.
func (h *QARadarHandler) DefectToJira(c *gin.Context) {
	userID := c.GetString("userID")
	var d models.Defect
	if !qaLoad(h.db, c, userID, c.Param("id"), &d, EditorRole, "Defect not found") {
		return
	}
	if d.JiraKey != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "This defect is already in Jira as " + d.JiraKey, "key": d.JiraKey})
		return
	}
	var in defectJiraInput
	if err := c.ShouldBindJSON(&in); err != nil || !jiraProjectKeyRE.MatchString(strings.ToUpper(in.ProjectKey)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jiraConnectionId and a valid projectKey are required"})
		return
	}
	jc, role, err := h.jira.loadAccessible(in.JiraConnectionID, userID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't use that Jira connection"})
		return
	}
	if jc.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": errNotYourJira})
		return
	}
	cl, err := h.jira.client(*jc)
	if err != nil {
		jiraFail(c, err)
		return
	}
	var b strings.Builder
	b.WriteString(d.Description)
	if s := strings.TrimSpace(d.StepsToReproduce); s != "" {
		b.WriteString("\n\nSteps to reproduce:\n" + s)
	}
	if d.Environment != "" {
		b.WriteString("\n\nEnvironment: " + d.Environment)
	}
	fmt.Fprintf(&b, "\n\nSeverity: %s · Priority: %s\n(Created from PatchMan QA)", d.Severity, d.Priority)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	key, err := cl.CreateIssue(ctx, strings.ToUpper(in.ProjectKey), strings.TrimSpace(in.IssueType), d.Title, b.String())
	if err != nil {
		jiraFail(c, err)
		return
	}
	h.db.Model(&models.Defect{}).Where("id = ?", d.ID).Update("jira_key", key)
	h.jira.auditWrite(jc, userID, role, "create_issue", key)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"key": key, "url": cl.IssueWebURL(key)}})
}
