package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"patchman-be/internal/forwarder"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// --- Test Plan Handler ---

type TestPlanHandler struct {
	db *gorm.DB
}

func NewTestPlanHandler(db *gorm.DB) *TestPlanHandler {
	return &TestPlanHandler{db: db}
}

type createTestPlanInput struct {
	ID          string   `json:"id"`
	Name        string   `json:"name" binding:"required"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	TestCaseIDs []string `json:"testCaseIds"`
}

type updateTestPlanInput struct {
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	Status      *string   `json:"status"`
	TestCaseIDs *[]string `json:"testCaseIds"`
}

// serializeTestPlan returns a TestPlan with testCaseIds parsed back to an
// array so FE callers don't have to know that the column is JSON-text on the
// wire. Mirrors the loadtest serializer.
func serializeTestPlan(p models.TestPlan) gin.H {
	ids := []string{}
	if p.TestCaseIDs != "" {
		_ = json.Unmarshal([]byte(p.TestCaseIDs), &ids)
	}
	return gin.H{
		"id":          p.ID,
		"userId":      p.UserID,
		"name":        p.Name,
		"description": p.Description,
		"status":      p.Status,
		"testCaseIds": ids,
		"createdAt":   p.CreatedAt,
		"updatedAt":   p.UpdatedAt,
	}
}

func serializeTestPlans(ps []models.TestPlan) []gin.H {
	out := make([]gin.H, len(ps))
	for i, p := range ps {
		out[i] = serializeTestPlan(p)
	}
	return out
}

func (h *TestPlanHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	var plans []models.TestPlan
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch test plans"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestPlans(plans)})
}

func (h *TestPlanHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createTestPlanInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	status := input.Status
	if status == "" {
		status = "draft"
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	testCaseIDsJSON := "[]"
	if len(input.TestCaseIDs) > 0 {
		b, _ := json.Marshal(input.TestCaseIDs)
		testCaseIDsJSON = string(b)
	}

	plan := models.TestPlan{
		ID:          newID,
		UserID:      userID,
		Name:        input.Name,
		Description: input.Description,
		Status:      status,
		TestCaseIDs: testCaseIDsJSON,
	}

	if err := h.db.Create(&plan).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test plan"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": serializeTestPlan(plan)})
}

func (h *TestPlanHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestPlan(plan)})
}

func (h *TestPlanHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	var input updateTestPlanInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Description != nil {
		updates["description"] = *input.Description
	}
	if input.Status != nil {
		updates["status"] = *input.Status
	}
	if input.TestCaseIDs != nil {
		b, _ := json.Marshal(*input.TestCaseIDs)
		updates["test_case_ids"] = string(b)
	}

	if err := h.db.Model(&plan).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update test plan"})
		return
	}

	h.db.First(&plan, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": serializeTestPlan(plan)})
}

func (h *TestPlanHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	// Also delete associated runs
	h.db.Where("test_plan_id = ?", id).Delete(&models.TestRun{})

	if err := h.db.Delete(&plan).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete test plan"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Test plan deleted"}})
}

// --- Test Run Handler ---

type TestRunHandler struct {
	db  *gorm.DB
	fwd *forwarder.Client
}

func NewTestRunHandler(db *gorm.DB) *TestRunHandler {
	return &TestRunHandler{db: db}
}

// WithForwarder mirrors TestRun create/update to a remote BE. See
// ProxyHandler.WithForwarder for the rationale.
func (h *TestRunHandler) WithForwarder(fwd *forwarder.Client) *TestRunHandler {
	h.fwd = fwd
	return h
}

type createTestRunInput struct {
	Environment string `json:"environment"`
}

type updateTestRunInput struct {
	Status  *string `json:"status"`
	Results *string `json:"results"`
}

// serializeTestRun returns a TestRun with results parsed to an array so FE
// callers don't have to JSON.parse on every read.
func serializeTestRun(r models.TestRun) gin.H {
	var results interface{} = []interface{}{}
	if r.Results != "" {
		var parsed interface{}
		if err := json.Unmarshal([]byte(r.Results), &parsed); err == nil {
			results = parsed
		}
	}
	out := gin.H{
		"id":          r.ID,
		"testPlanId":  r.TestPlanID,
		"userId":      r.UserID,
		"environment": r.Environment,
		"status":      r.Status,
		"results":     results,
		"startedAt":   r.StartedAt,
		"completedAt": r.CompletedAt,
		"createdAt":   r.CreatedAt,
	}
	if r.TestFolderID != nil {
		out["testFolderId"] = *r.TestFolderID
	}
	return out
}

func serializeTestRuns(rs []models.TestRun) []gin.H {
	out := make([]gin.H, len(rs))
	for i, r := range rs {
		out[i] = serializeTestRun(r)
	}
	return out
}

func (h *TestRunHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	planID := c.Param("id")

	// Verify plan belongs to user
	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	var input createTestRunInput
	if err := c.ShouldBindJSON(&input); err != nil {
		// Allow empty body
		input = createTestRunInput{}
	}

	now := time.Now()
	run := models.TestRun{
		ID:          uuid.New().String(),
		TestPlanID:  planID,
		UserID:      userID,
		Environment: input.Environment,
		Status:      "running",
		Results:     "[]",
		StartedAt:   now,
	}

	if err := h.db.Create(&run).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test run"})
		return
	}

	if h.fwd != nil {
		h.fwd.ForwardTestRun(run)
	}

	c.JSON(http.StatusCreated, gin.H{"data": serializeTestRun(run)})
}

// ListAll returns every TestRun for the current user across all plans, newest
// first. Used by the QA Planning > Runs > History tab so users don't have to
// drill into a plan to see past executions.
func (h *TestRunHandler) ListAll(c *gin.Context) {
	userID := c.GetString("userID")

	var runs []models.TestRun
	if err := h.db.Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&runs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch test runs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestRuns(runs)})
}

func (h *TestRunHandler) ListByPlan(c *gin.Context) {
	userID := c.GetString("userID")
	planID := c.Param("id")

	// Verify plan belongs to user
	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	var runs []models.TestRun
	if err := h.db.Where("test_plan_id = ?", planID).
		Order("created_at DESC").
		Find(&runs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch test runs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestRuns(runs)})
}

func (h *TestRunHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var run models.TestRun
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test run not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestRun(run)})
}

func (h *TestRunHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var run models.TestRun
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test run not found"})
		return
	}

	var input updateTestRunInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Status != nil {
		updates["status"] = *input.Status
		if *input.Status == "completed" {
			now := time.Now()
			updates["completed_at"] = &now
		}
	}
	if input.Results != nil {
		updates["results"] = *input.Results
	}

	if err := h.db.Model(&run).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update test run"})
		return
	}

	h.db.First(&run, "id = ?", id)

	if h.fwd != nil {
		h.fwd.ForwardTestRun(run)
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeTestRun(run)})
}

// Delete removes a TestRun (used by the History tab cleanup action).
func (h *TestRunHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var run models.TestRun
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&run).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test run not found"})
		return
	}

	if err := h.db.Delete(&run).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete test run"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Test run deleted"}})
}
