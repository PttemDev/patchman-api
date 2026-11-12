package handlers

import (
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type DefectHandler struct {
	db *gorm.DB
}

func NewDefectHandler(db *gorm.DB) *DefectHandler {
	return &DefectHandler{db: db}
}

type createDefectInput struct {
	ID               string `json:"id"`
	Title            string `json:"title" binding:"required"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	Priority         string `json:"priority"`
	Severity         string `json:"severity"`
	AssigneeID       string `json:"assigneeId"`
	TestCaseID       string `json:"testCaseId"`
	TestRunID        string `json:"testRunId"`
	Environment      string `json:"environment"`
	StepsToReproduce string `json:"stepsToReproduce"`
	Tags             string `json:"tags"`
}

type updateDefectInput struct {
	Title            *string `json:"title"`
	Description      *string `json:"description"`
	Status           *string `json:"status"`
	Priority         *string `json:"priority"`
	Severity         *string `json:"severity"`
	AssigneeID       *string `json:"assigneeId"`
	TestCaseID       *string `json:"testCaseId"`
	TestRunID        *string `json:"testRunId"`
	Environment      *string `json:"environment"`
	StepsToReproduce *string `json:"stepsToReproduce"`
	Tags             *string `json:"tags"`
}

func (h *DefectHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createDefectInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	status := input.Status
	if status == "" {
		status = "open"
	}
	priority := input.Priority
	if priority == "" {
		priority = "P2"
	}
	severity := input.Severity
	if severity == "" {
		severity = "minor"
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	defect := models.Defect{
		ID:               newID,
		UserID:           userID,
		Title:            input.Title,
		Description:      input.Description,
		Status:           status,
		Priority:         priority,
		Severity:         severity,
		AssigneeID:       input.AssigneeID,
		TestCaseID:       input.TestCaseID,
		TestRunID:        input.TestRunID,
		Environment:      input.Environment,
		StepsToReproduce: input.StepsToReproduce,
		Tags:             input.Tags,
	}

	if err := h.db.Create(&defect).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create defect"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": defect})
}

func (h *DefectHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	query := h.db.Where("user_id = ?", userID)

	// Apply filters
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if priority := c.Query("priority"); priority != "" {
		query = query.Where("priority = ?", priority)
	}
	if severity := c.Query("severity"); severity != "" {
		query = query.Where("severity = ?", severity)
	}
	if assigneeID := c.Query("assigneeId"); assigneeID != "" {
		query = query.Where("assignee_id = ?", assigneeID)
	}
	if testCaseID := c.Query("testCaseId"); testCaseID != "" {
		query = query.Where("test_case_id = ?", testCaseID)
	}
	if testRunID := c.Query("testRunId"); testRunID != "" {
		query = query.Where("test_run_id = ?", testRunID)
	}
	if tag := c.Query("tag"); tag != "" {
		query = query.Where("tags LIKE ?", "%"+tag+"%")
	}

	var defects []models.Defect
	if err := query.Order("created_at DESC").Find(&defects).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch defects"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": defects})
}

func (h *DefectHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var defect models.Defect
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&defect).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Defect not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": defect})
}

func (h *DefectHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var defect models.Defect
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&defect).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Defect not found"})
		return
	}

	var input updateDefectInput
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
	if input.Status != nil {
		updates["status"] = *input.Status
	}
	if input.Priority != nil {
		updates["priority"] = *input.Priority
	}
	if input.Severity != nil {
		updates["severity"] = *input.Severity
	}
	if input.AssigneeID != nil {
		updates["assignee_id"] = *input.AssigneeID
	}
	if input.TestCaseID != nil {
		updates["test_case_id"] = *input.TestCaseID
	}
	if input.TestRunID != nil {
		updates["test_run_id"] = *input.TestRunID
	}
	if input.Environment != nil {
		updates["environment"] = *input.Environment
	}
	if input.StepsToReproduce != nil {
		updates["steps_to_reproduce"] = *input.StepsToReproduce
	}
	if input.Tags != nil {
		updates["tags"] = *input.Tags
	}

	if err := h.db.Model(&defect).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update defect"})
		return
	}

	h.db.First(&defect, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": defect})
}

func (h *DefectHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var defect models.Defect
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&defect).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Defect not found"})
		return
	}

	if err := h.db.Delete(&defect).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete defect"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Defect deleted"}})
}
