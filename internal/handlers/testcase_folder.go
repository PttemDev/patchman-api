package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Test-case folders: a per-user tree (ParentID, NULL = top level) that the
// QA "Cases" list groups test cases into. Access mirrors test cases exactly —
// a folder is visible to, and changeable by, its owner only (test cases have
// no workspace/sharing layer). Every query below is user_id-scoped, so a
// foreign id behaves like a missing one (404), never a 403 that would leak
// its existence.

const maxTestFolderName = 255

type TestCaseFolderHandler struct {
	db *gorm.DB
}

func NewTestCaseFolderHandler(db *gorm.DB) *TestCaseFolderHandler {
	return &TestCaseFolderHandler{db: db}
}

type createTestFolderInput struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parentId"`
}

type renameTestFolderInput struct {
	Name string `json:"name"`
}

type moveTestCaseInput struct {
	// FolderID: a folder id, or null / "" for the root.
	FolderID *string `json:"folderId"`
}

type runTestFolderInput struct {
	EnvironmentID string `json:"environmentId"`
	// Recursive includes the cases of every subfolder (default true).
	Recursive *bool `json:"recursive"`
}

// cleanFolderName trims and validates a folder name.
func cleanFolderName(raw string) (string, string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "Folder name is required"
	}
	if utf8.RuneCountInString(name) > maxTestFolderName {
		return "", "Folder name is too long (max 255 characters)"
	}
	return name, ""
}

// emptyToNil maps "" (and a nil pointer) to nil so the column stores NULL.
func emptyToNil(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

// ownedFolder loads a folder the user owns.
func ownedFolder(db *gorm.DB, userID, id string) (models.TestCaseFolder, bool) {
	var f models.TestCaseFolder
	if id == "" {
		return f, false
	}
	if err := db.Where("id = ? AND user_id = ?", id, userID).First(&f).Error; err != nil {
		return f, false
	}
	return f, true
}

// nullableUpdate is the value for an Update of a nullable string column: a
// plain NULL for nil (GORM would otherwise skip or stringify a nil pointer).
func nullableUpdate(p *string) interface{} {
	if p == nil {
		return gorm.Expr("NULL")
	}
	return *p
}

// List handles GET /api/test-folders — the caller's folders, flat (the FE
// builds the tree from parentId).
func (h *TestCaseFolderHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var folders []models.TestCaseFolder
	if err := h.db.Where("user_id = ?", userID).Order("name ASC, created_at ASC").Find(&folders).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch test folders"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": folders})
}

// Create handles POST /api/test-folders.
func (h *TestCaseFolderHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createTestFolderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name, msg := cleanFolderName(input.Name)
	if msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	parentID := emptyToNil(input.ParentID)
	if parentID != nil {
		if _, ok := ownedFolder(h.db, userID, *parentID); !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "Parent folder not found"})
			return
		}
	}
	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}
	f := models.TestCaseFolder{ID: newID, UserID: userID, ParentID: parentID, Name: name}
	if err := h.db.Create(&f).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test folder"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": f})
}

// Rename handles PUT /api/test-folders/:id. Only the name changes: a folder's
// parent is fixed at creation (no re-parenting, so no cycle checks needed).
func (h *TestCaseFolderHandler) Rename(c *gin.Context) {
	userID := c.GetString("userID")
	f, ok := ownedFolder(h.db, userID, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	var input renameTestFolderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name, msg := cleanFolderName(input.Name)
	if msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.db.Model(&f).Update("name", name).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename test folder"})
		return
	}
	h.db.First(&f, "id = ?", f.ID)
	c.JSON(http.StatusOK, gin.H{"data": f})
}

// Delete handles DELETE /api/test-folders/:id[?moveToParent=true].
// A folder that still holds test cases or subfolders is refused (409) unless
// moveToParent is set, in which case its direct cases and subfolders move up
// to the deleted folder's parent (the root for a top-level folder). Test
// cases are never deleted by deleting a folder.
func (h *TestCaseFolderHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	f, ok := ownedFolder(h.db, userID, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	moveToParent := c.Query("moveToParent") == "true" || c.Query("moveToParent") == "1"

	var caseCount, folderCount int64
	h.db.Model(&models.TestCase{}).Where("user_id = ? AND folder_id = ?", userID, f.ID).Count(&caseCount)
	h.db.Model(&models.TestCaseFolder{}).Where("user_id = ? AND parent_id = ?", userID, f.ID).Count(&folderCount)
	if (caseCount > 0 || folderCount > 0) && !moveToParent {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "Folder is not empty; pass moveToParent=true to move its contents to the parent folder",
			"cases":   caseCount,
			"folders": folderCount,
		})
		return
	}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.TestCase{}).
			Where("user_id = ? AND folder_id = ?", userID, f.ID).
			Update("folder_id", nullableUpdate(f.ParentID)).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.TestCaseFolder{}).
			Where("user_id = ? AND parent_id = ?", userID, f.ID).
			Update("parent_id", nullableUpdate(f.ParentID)).Error; err != nil {
			return err
		}
		return tx.Delete(&f).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete test folder"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"message":      "Folder deleted",
		"movedCases":   caseCount,
		"movedFolders": folderCount,
		"movedTo":      f.ParentID,
	}})
}

// MoveTestCase handles PUT /api/test-cases/:id/move — puts a case into a
// folder (or the root for null / ""). A dedicated route rather than the case
// PUT so a move doesn't write a version snapshot.
func (h *TestCaseFolderHandler) MoveTestCase(c *gin.Context) {
	userID := c.GetString("userID")
	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}
	var input moveTestCaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	target := emptyToNil(input.FolderID)
	if target != nil {
		if _, ok := ownedFolder(h.db, userID, *target); !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
			return
		}
	}
	if err := h.db.Model(&tc).Update("folder_id", nullableUpdate(target)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move test case"})
		return
	}
	h.db.First(&tc, "id = ?", tc.ID)
	c.JSON(http.StatusOK, gin.H{"data": tc})
}

// ---------------------------------------------------------------------------
// Run all
// ---------------------------------------------------------------------------

// executableStepTypes are the step types executeTestCaseSteps runs. A case
// whose steps are all manual (no type) has nothing to execute.
var executableStepTypes = map[string]bool{"request": true, "assertion": true, "extract": true, "wait": true}

// hasExecutableSteps reports whether a case's steps JSON holds at least one
// step the executor can run. Mirrors the FE's rule for the folder-run preview.
func hasExecutableSteps(stepsJSON string) bool {
	if strings.TrimSpace(stepsJSON) == "" {
		return false
	}
	var steps []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		// Unparseable steps still "run" (the executor reports the parse
		// error as a failed case) rather than silently disappearing.
		return true
	}
	for _, s := range steps {
		if executableStepTypes[s.Type] {
			return true
		}
	}
	return false
}

// collectFolderCases returns, in execution order, the user's test cases in
// folderID — and, when recursive, in all of its subfolders — split into the
// ones with executable steps and the ones without (manual-only / empty).
//
// Order is the Cases tree's top-to-bottom display order: a folder's subfolders
// first (name A→Z, depth-first), then its own cases (newest first, as the
// list shows them). Folders are loaded once per user and walked in memory, so
// the same code runs on Postgres and SQLite (no recursive CTE).
func collectFolderCases(db *gorm.DB, userID, folderID string, recursive bool) (runnable, skipped []models.TestCase, err error) {
	var folders []models.TestCaseFolder
	if err = db.Where("user_id = ?", userID).Find(&folders).Error; err != nil {
		return nil, nil, err
	}
	children := map[string][]models.TestCaseFolder{}
	for _, f := range folders {
		if f.ParentID != nil {
			children[*f.ParentID] = append(children[*f.ParentID], f)
		}
	}
	for k := range children {
		kids := children[k]
		sort.SliceStable(kids, func(i, j int) bool {
			a, b := strings.ToLower(kids[i].Name), strings.ToLower(kids[j].Name)
			if a != b {
				return a < b
			}
			return kids[i].CreatedAt.Before(kids[j].CreatedAt)
		})
	}

	// Depth-first folder order; `seen` guards against a (corrupt) cycle.
	var order []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if recursive {
			for _, kid := range children[id] {
				walk(kid.ID)
			}
		}
		order = append(order, id)
	}
	walk(folderID)

	var cases []models.TestCase
	if err = db.Where("user_id = ? AND folder_id IN ?", userID, order).
		Order("created_at DESC, id ASC").Find(&cases).Error; err != nil {
		return nil, nil, err
	}
	byFolder := map[string][]models.TestCase{}
	for _, tc := range cases {
		if tc.FolderID != nil {
			byFolder[*tc.FolderID] = append(byFolder[*tc.FolderID], tc)
		}
	}
	for _, fid := range order {
		for _, tc := range byFolder[fid] {
			if hasExecutableSteps(tc.Steps) {
				runnable = append(runnable, tc)
			} else {
				skipped = append(skipped, tc)
			}
		}
	}
	return runnable, skipped, nil
}

// resolveRunEnvVars loads the variables a test run substitutes from envID —
// the same environment the editor's {{var}} autocomplete reads. Access follows
// the environment's own model (owner, share, public grant, workspace-team
// cascade), not just ownership, so running against a shared active env works.
// A Viewer never gets secret values substituted (they are masked for viewers
// everywhere else outside the audited reveal path). ok=false → no access.
func resolveRunEnvVars(db *gorm.DB, userID, envID string) (map[string]string, bool) {
	vars := map[string]string{}
	if envID == "" {
		return vars, true
	}
	role := ResolveEnvironmentRole(db, userID, envID)
	if role == NoRole {
		return nil, false
	}
	var env models.Environment
	if err := db.Where("id = ?", envID).First(&env).Error; err != nil {
		return nil, false
	}
	for _, v := range decodeVars(env.Variables) {
		if !v.Enabled || (v.Secret && role == ViewerRole) {
			continue
		}
		vars[v.Key] = v.Value
	}
	return vars, true
}

// folderRunCaseResult is one case of a folder run — the plan-run shape
// (testCaseId/title/status/duration/result) plus the case's folder.
type folderRunCaseResult struct {
	TestCaseID string          `json:"testCaseId"`
	Title      string          `json:"title"`
	FolderID   string          `json:"folderId"`
	Status     string          `json:"status"`
	Duration   int64           `json:"duration"`
	Result     executionResult `json:"result"`
}

type folderRunSkipped struct {
	TestCaseID string `json:"testCaseId"`
	Title      string `json:"title"`
	FolderID   string `json:"folderId"`
}

type folderRunSummary struct {
	Total   int `json:"total"`   // cases executed
	Passed  int `json:"passed"`  // executed and passed
	Failed  int `json:"failed"`  // executed and failed / errored
	Skipped int `json:"skipped"` // in the folder but nothing executable (manual-only / no steps)
}

// Execute handles POST /api/test-folders/:id/execute — "Run all": runs every
// executable case in the folder (and its subfolders unless recursive=false)
// sequentially with the existing step executor, persists one TestRun (with
// testFolderId set, testPlanId empty) so it shows in the Runs history, and
// returns the plan-run response shape plus a summary.
func (h *TestCaseFolderHandler) Execute(c *gin.Context) {
	userID := c.GetString("userID")
	f, ok := ownedFolder(h.db, userID, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	var input runTestFolderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		input = runTestFolderInput{} // allow an empty body
	}
	recursive := input.Recursive == nil || *input.Recursive

	envVars, ok := resolveRunEnvVars(h.db, userID, input.EnvironmentID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	runnable, skippedCases, err := collectFolderCases(h.db, userID, f.ID, recursive)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load test cases"})
		return
	}
	skipped := make([]folderRunSkipped, 0, len(skippedCases))
	for _, tc := range skippedCases {
		skipped = append(skipped, folderRunSkipped{TestCaseID: tc.ID, Title: tc.Title, FolderID: derefStr(tc.FolderID)})
	}
	if len(runnable) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Folder has no executable test cases",
			"skipped": skipped,
		})
		return
	}

	folderID := f.ID
	run := models.TestRun{
		ID:           uuid.New().String(),
		TestPlanID:   "",
		TestFolderID: &folderID,
		UserID:       userID,
		Environment:  input.EnvironmentID,
		Status:       "running",
		Results:      "[]",
		StartedAt:    time.Now(),
	}
	if err := h.db.Create(&run).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test run"})
		return
	}

	ctx := c.Request.Context()
	results := make([]folderRunCaseResult, 0, len(runnable))
	summary := folderRunSummary{Skipped: len(skipped)}
	overallStatus := "passed"
	totalDuration := int64(0)
	for _, tc := range runnable {
		// A cancelled call (closed tab, aborted MCP tool call) stops before
		// the next case instead of running the rest of the folder.
		if ctx.Err() != nil {
			overallStatus = "cancelled"
			break
		}
		res := executeTestCaseSteps(tc.Steps, envVars)
		totalDuration += res.Duration
		results = append(results, folderRunCaseResult{
			TestCaseID: tc.ID,
			Title:      tc.Title,
			FolderID:   derefStr(tc.FolderID),
			Status:     res.Status,
			Duration:   res.Duration,
			Result:     res,
		})
		summary.Total++
		if res.Status == "passed" {
			summary.Passed++
		} else {
			summary.Failed++
			if overallStatus == "passed" {
				overallStatus = "failed"
			}
		}
	}

	resultsJSON, _ := json.Marshal(results)
	completedAt := time.Now()
	h.db.Model(&run).Updates(map[string]interface{}{
		"status":       overallStatus,
		"results":      string(resultsJSON),
		"completed_at": &completedAt,
	})
	h.db.First(&run, "id = ?", run.ID)
	// Not forwarded upstream: the sync forward endpoint is plan-keyed (it
	// rejects a run without a test plan), so folder runs stay on the BE that
	// executed them.

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"testRun":       serializeTestRun(run),
		"folder":        gin.H{"id": f.ID, "name": f.Name},
		"testCases":     results,
		"skipped":       skipped,
		"overallStatus": overallStatus,
		"totalDuration": totalDuration,
		"summary":       summary,
	}})
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
