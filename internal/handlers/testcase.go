package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TestCaseHandler struct {
	db *gorm.DB
}

func NewTestCaseHandler(db *gorm.DB) *TestCaseHandler {
	return &TestCaseHandler{db: db}
}

type createTestCaseInput struct {
	ID          string `json:"id"`
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Type        string `json:"type"`
	Tags        string `json:"tags"`
	JiraKey     string `json:"jiraKey"`
	MRRef       string `json:"mrRef"`
	Steps       string `json:"steps"`
	Status      string `json:"status"`
	// FolderID optionally files the new case in a folder (null/"" = root).
	FolderID *string `json:"folderId"`
}

type updateTestCaseInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Priority    *string `json:"priority"`
	Type        *string `json:"type"`
	Tags        *string `json:"tags"`
	JiraKey     *string `json:"jiraKey"`
	MRRef       *string `json:"mrRef"`
	Steps       *string `json:"steps"`
	Status      *string `json:"status"`
}

func (h *TestCaseHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	query, ok := qaListScope(h.db, c, userID)
	if !ok {
		return
	}

	// Apply filters
	if priority := c.Query("priority"); priority != "" {
		query = query.Where("priority = ?", priority)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if tag := c.Query("tag"); tag != "" {
		query = query.Where("tags LIKE ?", "%"+tag+"%")
	}

	var testCases []models.TestCase
	if err := query.Order("created_at DESC").Find(&testCases).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch test cases"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": testCases})
}

func (h *TestCaseHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createTestCaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	priority := input.Priority
	if priority == "" {
		priority = "P2"
	}
	tcType := normalizeTestCaseType(input.Type)
	status := input.Status
	if status == "" {
		status = "draft"
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	folderID := emptyToNil(input.FolderID)
	ws, ok := qaCreateWorkspace(h.db, c, userID)
	if !ok {
		return
	}
	if folderID != nil {
		if f, ok := ownedFolder(h.db, userID, *folderID); !ok || !sameQAScope(f, models.TestCase{UserID: userID, WorkspaceID: ws}) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
			return
		}
	}

	tc := models.TestCase{
		ID:          newID,
		UserID:      userID,
		WorkspaceID: ws,
		FolderID:    folderID,
		Title:       input.Title,
		Description: input.Description,
		Priority:    priority,
		Type:        tcType,
		Tags:        normalizeTestCaseTags(input.Tags),
		Steps:       normalizeTestCaseSteps(input.Steps),
		Status:      status,
		JiraKey:     strings.ToUpper(strings.TrimSpace(input.JiraKey)),
		MRRef:       strings.TrimSpace(input.MRRef),
	}

	if err := h.db.Create(&tc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test case"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": tc})
}

func (h *TestCaseHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var tc models.TestCase
	if !qaLoad(h.db, c, userID, id, &tc, ViewerRole, "Test case not found") {
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": tc})
}

func (h *TestCaseHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var tc models.TestCase
	if !qaLoad(h.db, c, userID, id, &tc, EditorRole, "Test case not found") {
		return
	}

	var input updateTestCaseInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create a version snapshot of the current state before applying updates
	changeNote := "Auto-snapshot before update"
	if err := createVersionSnapshot(h.db, tc, userID, changeNote); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create version snapshot"})
		return
	}

	updates := map[string]interface{}{}
	if input.Title != nil {
		updates["title"] = *input.Title
	}
	if input.Description != nil {
		updates["description"] = *input.Description
	}
	if input.Priority != nil {
		updates["priority"] = *input.Priority
	}
	if input.Type != nil {
		updates["type"] = normalizeTestCaseType(*input.Type)
	}
	if input.JiraKey != nil {
		updates["jira_key"] = strings.ToUpper(strings.TrimSpace(*input.JiraKey))
	}
	if input.MRRef != nil {
		updates["mr_ref"] = strings.TrimSpace(*input.MRRef)
	}
	if input.Tags != nil {
		updates["tags"] = normalizeTestCaseTags(*input.Tags)
	}
	if input.Steps != nil {
		updates["steps"] = normalizeTestCaseSteps(*input.Steps)
	}
	if input.Status != nil {
		updates["status"] = *input.Status
	}

	if err := h.db.Model(&tc).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update test case"})
		return
	}

	h.db.First(&tc, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": tc})
}

func (h *TestCaseHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var tc models.TestCase
	if !qaLoad(h.db, c, userID, id, &tc, OwnerRole, "Test case not found") {
		return
	}

	if err := h.db.Delete(&tc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete test case"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Test case deleted"}})
}
