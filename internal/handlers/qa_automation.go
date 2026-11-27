package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CR + QA automation (CR_QA_RFC P3): scheduled test runs and the daily
// channel digest run on the server as non-LLM jobs; Reports read the
// team-level numbers. Agent work (the night run) stays on people's machines.

type QAAutomationHandler struct {
	db       *gorm.DB
	channels *QAChannels // may be nil (no cipher)
}

func NewQAAutomationHandler(db *gorm.DB) *QAAutomationHandler { return &QAAutomationHandler{db: db} }

// SetChannels wires the channel sender (registered with the cipher).
func (h *QAAutomationHandler) SetChannels(c *QAChannels) { h.channels = c }

// Start runs the scheduler (call once, after routes are set up).
func (h *QAAutomationHandler) Start() { StartQAScheduler(h.db, h.channels) }

// ── schedules ─────────────────────────────────────────────────────────

type scheduleInput struct {
	Name          *string `json:"name"`
	TargetKind    *string `json:"targetKind"`
	TargetID      *string `json:"targetId"`
	EnvironmentID *string `json:"environmentId"`
	Days          *string `json:"days"`
	Hour          *int    `json:"hour"`
	Minute        *int    `json:"minute"`
	Timezone      *string `json:"timezone"`
	Enabled       *bool   `json:"enabled"`
}

func (in scheduleInput) apply(db *gorm.DB, userID string, s *models.QAScheduledRun) string {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&s.Name, in.Name)
	set(&s.TargetKind, in.TargetKind)
	set(&s.TargetID, in.TargetID)
	set(&s.EnvironmentID, in.EnvironmentID)
	set(&s.Days, in.Days)
	set(&s.Timezone, in.Timezone)
	if in.Hour != nil {
		s.Hour = *in.Hour
	}
	if in.Minute != nil {
		s.Minute = *in.Minute
	}
	if in.Enabled != nil {
		s.Enabled = *in.Enabled
	}
	switch {
	case s.TargetKind != "folder" && s.TargetKind != "plan":
		return "targetKind must be folder or plan"
	case s.TargetID == "":
		return "targetId is required"
	case s.Hour < 0 || s.Hour > 23 || s.Minute < 0 || s.Minute > 59:
		return "hour 0–23 and minute 0–59"
	}
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return "timezone must be an IANA name"
	}
	for _, d := range strings.Split(s.Days, ",") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if n, err := strconv.Atoi(d); err != nil || n < 1 || n > 7 {
			return "days are 1 (Mon) … 7 (Sun)"
		}
	}
	// The target must be in the schedule's space and editable by the caller.
	var owned qaOwned
	if s.TargetKind == "folder" {
		var f models.TestCaseFolder
		if db.Where("id = ?", s.TargetID).First(&f).Error != nil {
			return "Folder not found"
		}
		owned = f
	} else {
		var p models.TestPlan
		if db.Where("id = ?", s.TargetID).First(&p).Error != nil {
			return "Plan not found"
		}
		owned = p
	}
	if !qaRowRole(db, userID, owned).AtLeast(EditorRole) || !qaSameScopeIDs(owned, s.UserID, s.WorkspaceID) {
		return "The folder / plan must be in this QA space"
	}
	if s.EnvironmentID != "" {
		if _, ok := resolveRunEnvVars(db, userID, s.EnvironmentID); !ok {
			return "Environment not found"
		}
	}
	return ""
}

func qaSameScopeIDs(row qaOwned, owner string, ws *string) bool {
	o, rws := row.QAOwner()
	if ws != nil && *ws != "" {
		return rws != nil && *rws == *ws
	}
	return (rws == nil || *rws == "") && o == owner
}

// ListSchedules — GET /api/qa/schedules?workspaceId=.
func (h *QAAutomationHandler) ListSchedules(c *gin.Context) {
	userID := c.GetString("userID")
	scope, ok := qaListScope(h.db, c, userID)
	if !ok {
		return
	}
	rows := []models.QAScheduledRun{}
	scope.Order("created_at ASC").Find(&rows)
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// CreateSchedule — POST /api/qa/schedules?workspaceId=.
func (h *QAAutomationHandler) CreateSchedule(c *gin.Context) {
	userID := c.GetString("userID")
	var in scheduleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ws, ok := qaCreateWorkspace(h.db, c, userID)
	if !ok {
		return
	}
	s := models.QAScheduledRun{ID: uuid.NewString(), UserID: userID, WorkspaceID: ws, Enabled: true}
	if msg := in.apply(h.db, userID, &s); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.db.Create(&s).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": s})
}

// UpdateSchedule — PUT /api/qa/schedules/:id (its creator, or an admin).
func (h *QAAutomationHandler) UpdateSchedule(c *gin.Context) {
	userID := c.GetString("userID")
	var s models.QAScheduledRun
	if !qaLoad(h.db, c, userID, c.Param("id"), &s, OwnerRole, "Schedule not found") {
		return
	}
	var in scheduleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg := in.apply(h.db, userID, &s); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	h.db.Save(&s)
	c.JSON(http.StatusOK, gin.H{"data": s})
}

// DeleteSchedule — DELETE /api/qa/schedules/:id.
func (h *QAAutomationHandler) DeleteSchedule(c *gin.Context) {
	userID := c.GetString("userID")
	var s models.QAScheduledRun
	if !qaLoad(h.db, c, userID, c.Param("id"), &s, OwnerRole, "Schedule not found") {
		return
	}
	h.db.Delete(&s)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// RunScheduleNow — POST /api/qa/schedules/:id/run.
func (h *QAAutomationHandler) RunScheduleNow(c *gin.Context) {
	userID := c.GetString("userID")
	var s models.QAScheduledRun
	if !qaLoad(h.db, c, userID, c.Param("id"), &s, EditorRole, "Schedule not found") {
		return
	}
	run, err := runSchedule(c.Request.Context(), h.db, &s)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"testRun": serializeTestRun(run), "schedule": s}})
}

// runSchedule runs one schedule with its creator's access and records the
// outcome on it.
func runSchedule(ctx context.Context, db *gorm.DB, s *models.QAScheduledRun) (models.TestRun, error) {
	fail := func(err error) (models.TestRun, error) {
		now := time.Now()
		db.Model(&models.QAScheduledRun{}).Where("id = ?", s.ID).Updates(map[string]any{"last_run_at": &now, "last_status": "error", "last_error": err.Error()})
		return models.TestRun{}, err
	}
	tmpl := models.TestRun{UserID: s.UserID, WorkspaceID: s.WorkspaceID, Environment: s.EnvironmentID}
	var cases []models.TestCase
	if s.TargetKind == "folder" {
		var f models.TestCaseFolder
		if err := db.Where("id = ?", s.TargetID).First(&f).Error; err != nil {
			return fail(fmt.Errorf("the folder no longer exists"))
		}
		if !qaRowRole(db, s.UserID, f).AtLeast(EditorRole) {
			return fail(fmt.Errorf("the schedule's creator can no longer run this folder"))
		}
		runnable, skipped, err := collectFolderCases(db, f, true)
		if err != nil {
			return fail(err)
		}
		cases = append(runnable, skipped...)
		id := f.ID
		tmpl.TestFolderID = &id
	} else {
		var p models.TestPlan
		if err := db.Where("id = ?", s.TargetID).First(&p).Error; err != nil {
			return fail(fmt.Errorf("the plan no longer exists"))
		}
		if !qaRowRole(db, s.UserID, p).AtLeast(EditorRole) {
			return fail(fmt.Errorf("the schedule's creator can no longer run this plan"))
		}
		var ids []string
		_ = json.Unmarshal([]byte(p.TestCaseIDs), &ids)
		if len(ids) > 0 {
			qaSameScope(db, p).Where("id IN ?", ids).Find(&cases)
		}
		tmpl.TestPlanID = p.ID
	}
	envVars, ok := resolveRunEnvVars(db, s.UserID, s.EnvironmentID)
	if !ok {
		return fail(fmt.Errorf("the environment is not available to the schedule's creator"))
	}
	run, _, _, err := qaRunCases(ctx, db, tmpl, cases, envVars)
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	db.Model(&models.QAScheduledRun{}).Where("id = ?", s.ID).Updates(map[string]any{"last_run_at": &now, "last_run_id": run.ID, "last_status": run.Status, "last_error": ""})
	s.LastRunAt, s.LastRunID, s.LastStatus, s.LastError = &now, run.ID, run.Status, ""
	return run, nil
}

// dueAt: the schedule's run time today in its zone, and whether today is
// one of its days.
func dueAt(s models.QAScheduledRun, now time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC
	}
	n := now.In(loc)
	at := time.Date(n.Year(), n.Month(), n.Day(), s.Hour, s.Minute, 0, 0, loc)
	if strings.TrimSpace(s.Days) == "" {
		return at, true
	}
	wd := int(n.Weekday())
	if wd == 0 {
		wd = 7
	}
	for _, d := range strings.Split(s.Days, ",") {
		if strings.TrimSpace(d) == strconv.Itoa(wd) {
			return at, true
		}
	}
	return at, false
}

// StartQAScheduler runs due schedules and the daily channel digests once a
// minute. A run or digest is claimed with a conditional UPDATE, so several
// server replicas never run the same one twice.
func StartQAScheduler(db *gorm.DB, channels *QAChannels) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			qaSchedulerTick(db, channels, time.Now())
		}
	}()
}

const scheduleWindow = 15 * time.Minute

func qaSchedulerTick(db *gorm.DB, channels *QAChannels, now time.Time) {
	var schedules []models.QAScheduledRun
	db.Where("enabled = ?", true).Find(&schedules)
	for i := range schedules {
		s := schedules[i]
		at, today := dueAt(s, now)
		if !today || now.Before(at) || now.Sub(at) > scheduleWindow {
			continue
		}
		if s.LastRunAt != nil && !s.LastRunAt.Before(at) {
			continue
		}
		claim := db.Model(&models.QAScheduledRun{}).Where("id = ? AND (last_run_at IS NULL OR last_run_at < ?)", s.ID, at).
			Update("last_run_at", now)
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		go func(s models.QAScheduledRun) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if _, err := runSchedule(ctx, db, &s); err != nil {
				log.Printf("[qa-scheduler] schedule %s: %v", s.ID, err)
			}
		}(s)
	}
	if channels != nil {
		channels.digestTick(now)
	}
}

// ── reports ───────────────────────────────────────────────────────────

// Reports — GET /api/qa/reports?workspaceId=&days=30: team-level numbers
// only (CR_QA_RFC invariant: no per-person ranking).
func (h *QAAutomationHandler) Reports(c *gin.Context) {
	userID := c.GetString("userID")
	ws := qaWorkspaceParam(c)
	if ws != "" && qaWorkspaceRole(h.db, ws, userID) == NoRole {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}
	var wsp *string
	if ws != "" {
		wsp = &ws
	}
	days, _ := strconv.Atoi(c.Query("days"))
	if days <= 0 || days > 180 {
		days = 30
	}
	since := time.Now().AddDate(0, 0, -days)
	sinceDay := since.UTC().Format("2006-01-02")

	// Radar trend.
	var snaps []models.QARadarSnapshot
	h.db.Where("space_key = ? AND day >= ?", qaSpaceKey(userID, wsp), sinceDay).Order("day ASC").Find(&snaps)
	trend := make([]gin.H, 0, len(snaps))
	for _, s := range snaps {
		sig := map[string]int{}
		_ = json.Unmarshal([]byte(s.SignalsJSON), &sig)
		trend = append(trend, gin.H{"day": s.Day, "openMrs": s.OpenMRs, "signals": sig, "merged": s.Merged, "leadMedianH": s.LeadMedianH, "leadP90H": s.LeadP90H})
	}

	scope := func(q *gorm.DB) *gorm.DB {
		if wsp != nil {
			return q.Where("workspace_id = ?", ws)
		}
		return q.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", userID)
	}

	// Test runs per day (pass / fail).
	var runs []models.TestRun
	scope(h.db.Model(&models.TestRun{})).Where("started_at >= ?", since).Find(&runs)
	type dayRuns struct{ Passed, Failed, Runs int }
	byDay := map[string]*dayRuns{}
	casesPassed, casesFailed := 0, 0
	for _, r := range runs {
		d := r.StartedAt.UTC().Format("2006-01-02")
		if byDay[d] == nil {
			byDay[d] = &dayRuns{}
		}
		byDay[d].Runs++
		var rows []struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal([]byte(r.Results), &rows)
		for _, x := range rows {
			if x.Status == "passed" {
				byDay[d].Passed++
				casesPassed++
			} else {
				byDay[d].Failed++
				casesFailed++
			}
		}
	}
	tests := make([]gin.H, 0, len(byDay))
	for d, v := range byDay {
		tests = append(tests, gin.H{"day": d, "passed": v.Passed, "failed": v.Failed, "runs": v.Runs})
	}
	sort.Slice(tests, func(a, b int) bool { return tests[a]["day"].(string) < tests[b]["day"].(string) })

	// Defects: open vs closed, created / closed in the window.
	var defects []models.Defect
	scope(h.db.Model(&models.Defect{})).Find(&defects)
	open, closed, created := 0, 0, 0
	for _, d := range defects {
		switch d.Status {
		case "closed", "verified", "fixed":
			closed++
		default:
			open++
		}
		if d.CreatedAt.After(since) {
			created++
		}
	}

	// AI review findings by severity × decision.
	var reviews []models.MRReview
	rq := h.db.Model(&models.MRReview{}).Where("updated_at >= ?", since)
	if wsp != nil {
		rq = rq.Where("workspace_id = ?", ws)
	} else {
		rq = rq.Where("claimed_by = ? AND (workspace_id IS NULL OR workspace_id = '')", userID)
	}
	rq.Find(&reviews)
	findings := map[string]map[string]int{}
	for i := range reviews {
		for _, f := range reviewFindings(&reviews[i]) {
			if findings[f.Severity] == nil {
				findings[f.Severity] = map[string]int{}
			}
			findings[f.Severity][f.Status]++
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"days": days, "radar": trend, "tests": tests,
		"testTotals": gin.H{"passed": casesPassed, "failed": casesFailed, "runs": len(runs)},
		"defects":    gin.H{"open": open, "closed": closed, "createdInWindow": created},
		"reviews":    gin.H{"count": len(reviews), "findings": findings},
	}})
}
