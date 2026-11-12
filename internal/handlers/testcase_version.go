package handlers

import (
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type TestCaseVersionHandler struct {
	db *gorm.DB
}

func NewTestCaseVersionHandler(db *gorm.DB) *TestCaseVersionHandler {
	return &TestCaseVersionHandler{db: db}
}

// ListVersions returns all versions of a test case ordered by version descending.
// GET /api/test-cases/:id/versions
func (h *TestCaseVersionHandler) ListVersions(c *gin.Context) {
	userID := c.GetString("userID")
	testCaseID := c.Param("id")

	// Verify the test case belongs to the user
	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", testCaseID, userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}

	var versions []models.TestCaseVersion
	if err := h.db.Where("test_case_id = ?", testCaseID).
		Order("version DESC").
		Find(&versions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch versions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": versions})
}

// GetVersion returns a specific version of a test case.
// GET /api/test-cases/:id/versions/:versionId
func (h *TestCaseVersionHandler) GetVersion(c *gin.Context) {
	userID := c.GetString("userID")
	testCaseID := c.Param("id")
	versionID := c.Param("versionId")

	// Verify the test case belongs to the user
	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", testCaseID, userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}

	var version models.TestCaseVersion
	if err := h.db.Where("id = ? AND test_case_id = ?", versionID, testCaseID).First(&version).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": version})
}

// RestoreVersion restores a previous version by creating a new version snapshot of the
// current state and then applying the old version's data to the test case.
// POST /api/test-cases/:id/versions/:versionId/restore
func (h *TestCaseVersionHandler) RestoreVersion(c *gin.Context) {
	userID := c.GetString("userID")
	testCaseID := c.Param("id")
	versionID := c.Param("versionId")

	// Verify the test case belongs to the user
	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", testCaseID, userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}

	// Find the version to restore
	var restoreVersion models.TestCaseVersion
	if err := h.db.Where("id = ? AND test_case_id = ?", versionID, testCaseID).First(&restoreVersion).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}

	// Determine the next version number
	var maxVersion int
	h.db.Model(&models.TestCaseVersion{}).
		Where("test_case_id = ?", testCaseID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxVersion)
	nextVersion := maxVersion + 1

	// Snapshot current state before restoring
	snapshot := models.TestCaseVersion{
		ID:          uuid.New().String(),
		TestCaseID:  testCaseID,
		Version:     nextVersion,
		Title:       tc.Title,
		Description: tc.Description,
		Steps:       tc.Steps,
		Priority:    tc.Priority,
		Tags:        tc.Tags,
		ChangedBy:   userID,
		ChangeNote:  "Snapshot before restore to version " + versionID,
	}

	if err := h.db.Create(&snapshot).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create version snapshot"})
		return
	}

	// Apply the restored version's data to the test case
	updates := map[string]interface{}{
		"title":       restoreVersion.Title,
		"description": restoreVersion.Description,
		"steps":       restoreVersion.Steps,
		"priority":    restoreVersion.Priority,
		"tags":        restoreVersion.Tags,
	}

	if err := h.db.Model(&tc).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restore test case"})
		return
	}

	h.db.First(&tc, "id = ?", testCaseID)
	c.JSON(http.StatusOK, gin.H{"data": tc})
}

// createVersionSnapshot creates a version snapshot of the current test case state.
// This is called internally before a test case is updated.
func createVersionSnapshot(db *gorm.DB, tc models.TestCase, userID string, changeNote string) error {
	var maxVersion int
	db.Model(&models.TestCaseVersion{}).
		Where("test_case_id = ?", tc.ID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxVersion)

	snapshot := models.TestCaseVersion{
		ID:          uuid.New().String(),
		TestCaseID:  tc.ID,
		Version:     maxVersion + 1,
		Title:       tc.Title,
		Description: tc.Description,
		Steps:       tc.Steps,
		Priority:    tc.Priority,
		Tags:        tc.Tags,
		ChangedBy:   userID,
		ChangeNote:  changeNote,
	}

	return db.Create(&snapshot).Error
}
