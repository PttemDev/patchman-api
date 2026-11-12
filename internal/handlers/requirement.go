package handlers

import (
	"encoding/json"
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type RequirementHandler struct {
	db *gorm.DB
}

func NewRequirementHandler(db *gorm.DB) *RequirementHandler {
	return &RequirementHandler{db: db}
}

type createRequirementInput struct {
	ID          string  `json:"id"`
	Title       string  `json:"title" binding:"required"`
	Description string  `json:"description"`
	Type        string  `json:"type"`
	Priority    string  `json:"priority"`
	Status      string  `json:"status"`
	ParentID    *string `json:"parentId"`
	ExternalRef string  `json:"externalRef"`
	Tags        string  `json:"tags"`
}

type updateRequirementInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Type        *string `json:"type"`
	Priority    *string `json:"priority"`
	Status      *string `json:"status"`
	ParentID    *string `json:"parentId"`
	ExternalRef *string `json:"externalRef"`
	Tags        *string `json:"tags"`
}

type linkTestCaseInput struct {
	TestCaseID string `json:"testCaseId" binding:"required"`
}

func (h *RequirementHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	query := h.db.Where("user_id = ?", userID)

	// Apply filters
	if reqType := c.Query("type"); reqType != "" {
		query = query.Where("type = ?", reqType)
	}
	if priority := c.Query("priority"); priority != "" {
		query = query.Where("priority = ?", priority)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if tag := c.Query("tag"); tag != "" {
		query = query.Where("tags LIKE ?", "%"+tag+"%")
	}

	var requirements []models.Requirement
	if err := query.Order("created_at DESC").Find(&requirements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch requirements"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": requirements})
}

func (h *RequirementHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createRequirementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	reqType := input.Type
	if reqType == "" {
		reqType = "functional"
	}
	priority := input.Priority
	if priority == "" {
		priority = "medium"
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

	req := models.Requirement{
		ID:          newID,
		UserID:      userID,
		Title:       input.Title,
		Description: input.Description,
		Type:        reqType,
		Priority:    priority,
		Status:      status,
		ParentID:    input.ParentID,
		ExternalRef: input.ExternalRef,
		Tags:        input.Tags,
	}

	if err := h.db.Create(&req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create requirement"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": req})
}

func (h *RequirementHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.Requirement
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&req).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Requirement not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": req})
}

func (h *RequirementHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.Requirement
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&req).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Requirement not found"})
		return
	}

	var input updateRequirementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Title != nil {
		updates["title"] = *input.Title
	}
	if input.Description != nil {
		updates["description"] = *input.Description
	}
	if input.Type != nil {
		updates["type"] = *input.Type
	}
	if input.Priority != nil {
		updates["priority"] = *input.Priority
	}
	if input.Status != nil {
		updates["status"] = *input.Status
	}
	if input.ParentID != nil {
		updates["parent_id"] = *input.ParentID
	}
	if input.ExternalRef != nil {
		updates["external_ref"] = *input.ExternalRef
	}
	if input.Tags != nil {
		updates["tags"] = *input.Tags
	}

	if err := h.db.Model(&req).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update requirement"})
		return
	}

	h.db.First(&req, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": req})
}

func (h *RequirementHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.Requirement
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&req).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Requirement not found"})
		return
	}

	// Also delete associated coverage links
	h.db.Where("requirement_id = ?", id).Delete(&models.RequirementCoverage{})

	if err := h.db.Delete(&req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete requirement"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Requirement deleted"}})
}

// LinkTestCase links a test case to a requirement.
// POST /api/requirements/:id/test-cases
func (h *RequirementHandler) LinkTestCase(c *gin.Context) {
	userID := c.GetString("userID")
	requirementID := c.Param("id")

	// Verify requirement belongs to user
	var req models.Requirement
	if err := h.db.Where("id = ? AND user_id = ?", requirementID, userID).First(&req).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Requirement not found"})
		return
	}

	var input linkTestCaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify test case belongs to user
	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", input.TestCaseID, userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}

	// Check if link already exists
	var existing models.RequirementCoverage
	if err := h.db.Where("requirement_id = ? AND test_case_id = ?", requirementID, input.TestCaseID).First(&existing).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Test case is already linked to this requirement"})
		return
	}

	coverage := models.RequirementCoverage{
		ID:            uuid.New().String(),
		RequirementID: requirementID,
		TestCaseID:    input.TestCaseID,
	}

	if err := h.db.Create(&coverage).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to link test case"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": coverage})
}

// UnlinkTestCase removes a test case link from a requirement.
// DELETE /api/requirements/:id/test-cases/:testCaseId
func (h *RequirementHandler) UnlinkTestCase(c *gin.Context) {
	userID := c.GetString("userID")
	requirementID := c.Param("id")
	testCaseID := c.Param("testCaseId")

	// Verify requirement belongs to user
	var req models.Requirement
	if err := h.db.Where("id = ? AND user_id = ?", requirementID, userID).First(&req).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Requirement not found"})
		return
	}

	var coverage models.RequirementCoverage
	if err := h.db.Where("requirement_id = ? AND test_case_id = ?", requirementID, testCaseID).First(&coverage).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Coverage link not found"})
		return
	}

	if err := h.db.Delete(&coverage).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unlink test case"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Test case unlinked from requirement"}})
}

// traceabilityRow represents a single row in the traceability matrix response.
type traceabilityRow struct {
	Requirement models.Requirement `json:"requirement"`
	TestCases   []traceabilityTC   `json:"testCases"`
	Coverage    int                `json:"coverage"` // Number of linked test cases
}

type traceabilityTC struct {
	TestCase     models.TestCase `json:"testCase"`
	LatestStatus string          `json:"latestStatus"` // pass/fail/blocked/not_run
}

// GetTraceabilityMatrix returns requirements with linked test cases and their
// latest test run results.
// GET /api/requirements/traceability
func (h *RequirementHandler) GetTraceabilityMatrix(c *gin.Context) {
	userID := c.GetString("userID")

	// Fetch all requirements for the user
	var requirements []models.Requirement
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&requirements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch requirements"})
		return
	}

	// Fetch all coverage links for these requirements
	reqIDs := make([]string, len(requirements))
	for i, r := range requirements {
		reqIDs[i] = r.ID
	}

	var coverages []models.RequirementCoverage
	if len(reqIDs) > 0 {
		h.db.Where("requirement_id IN ?", reqIDs).Find(&coverages)
	}

	// Build a map: requirementID -> []testCaseID
	reqTCMap := map[string][]string{}
	allTCIDs := map[string]bool{}
	for _, cov := range coverages {
		reqTCMap[cov.RequirementID] = append(reqTCMap[cov.RequirementID], cov.TestCaseID)
		allTCIDs[cov.TestCaseID] = true
	}

	// Fetch all referenced test cases
	tcIDs := make([]string, 0, len(allTCIDs))
	for id := range allTCIDs {
		tcIDs = append(tcIDs, id)
	}

	tcMap := map[string]models.TestCase{}
	if len(tcIDs) > 0 {
		var testCases []models.TestCase
		h.db.Where("id IN ?", tcIDs).Find(&testCases)
		for _, tc := range testCases {
			tcMap[tc.ID] = tc
		}
	}

	// Fetch latest test run results to determine pass/fail status per test case.
	// We look at all completed test runs and collect the most recent result for each test case.
	latestStatus := map[string]string{} // testCaseID -> status
	var runs []models.TestRun
	h.db.Where("user_id = ? AND status = ?", userID, "completed").
		Order("created_at DESC").
		Find(&runs)

	for _, run := range runs {
		// Parse results JSON: [{testCaseId, status, notes, duration}]
		var results []struct {
			TestCaseID string `json:"testCaseId"`
			Status     string `json:"status"`
		}
		if err := json.Unmarshal([]byte(run.Results), &results); err != nil {
			continue
		}
		for _, r := range results {
			// Only record the first (most recent) status we find for each test case
			if _, exists := latestStatus[r.TestCaseID]; !exists {
				latestStatus[r.TestCaseID] = r.Status
			}
		}
	}

	// Build the traceability matrix
	matrix := make([]traceabilityRow, 0, len(requirements))
	for _, req := range requirements {
		tcIDsForReq := reqTCMap[req.ID]
		tcs := make([]traceabilityTC, 0, len(tcIDsForReq))
		for _, tcID := range tcIDsForReq {
			tc, ok := tcMap[tcID]
			if !ok {
				continue
			}
			status := "not_run"
			if s, exists := latestStatus[tcID]; exists {
				status = s
			}
			tcs = append(tcs, traceabilityTC{
				TestCase:     tc,
				LatestStatus: status,
			})
		}
		matrix = append(matrix, traceabilityRow{
			Requirement: req,
			TestCases:   tcs,
			Coverage:    len(tcs),
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": matrix})
}
