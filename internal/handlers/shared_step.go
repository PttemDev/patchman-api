package handlers

import (
	"encoding/json"
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type SharedStepHandler struct {
	db *gorm.DB
}

func NewSharedStepHandler(db *gorm.DB) *SharedStepHandler {
	return &SharedStepHandler{db: db}
}

// FE sends `steps` as an array; BE persists it as a JSON-encoded text column.
// Use json.RawMessage so we accept both shapes (array or already-stringified
// JSON) without forcing the caller to pre-stringify.
type createSharedStepInput struct {
	ID    string          `json:"id"`
	Title string          `json:"title" binding:"required"`
	Steps json.RawMessage `json:"steps"`
}

type updateSharedStepInput struct {
	Title *string          `json:"title"`
	Steps *json.RawMessage `json:"steps"`
}

// stepsRawToString normalizes incoming `steps` to a JSON text suitable for
// the DB column. If the caller sent a JSON string we honor it as-is; if they
// sent an array/object we encode it; if they sent nothing, we store "[]".
func stepsRawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "[]"
	}
	// If the caller already passed a JSON string ("…"), unquote it.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return "[]"
		}
		return asString
	}
	return string(raw)
}

// serializeSharedStep returns a SharedStep with steps parsed back to an array
// so FE callers don't need to JSON.parse on every read.
func serializeSharedStep(s models.SharedStep) gin.H {
	var steps interface{} = []interface{}{}
	if s.Steps != "" {
		var parsed interface{}
		if err := json.Unmarshal([]byte(s.Steps), &parsed); err == nil {
			steps = parsed
		}
	}
	return gin.H{
		"id":         s.ID,
		"userId":     s.UserID,
		"title":      s.Title,
		"steps":      steps,
		"usageCount": s.UsageCount,
		"createdAt":  s.CreatedAt,
		"updatedAt":  s.UpdatedAt,
	}
}

func serializeSharedSteps(items []models.SharedStep) []gin.H {
	out := make([]gin.H, len(items))
	for i, s := range items {
		out[i] = serializeSharedStep(s)
	}
	return out
}

func (h *SharedStepHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	var steps []models.SharedStep
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&steps).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch shared steps"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeSharedSteps(steps)})
}

func (h *SharedStepHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createSharedStepInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	step := models.SharedStep{
		ID:     newID,
		UserID: userID,
		Title:  input.Title,
		Steps:  stepsRawToString(input.Steps),
	}

	if err := h.db.Create(&step).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create shared step"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": serializeSharedStep(step)})
}

func (h *SharedStepHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var step models.SharedStep
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&step).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shared step not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeSharedStep(step)})
}

func (h *SharedStepHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var step models.SharedStep
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&step).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shared step not found"})
		return
	}

	var input updateSharedStepInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Title != nil {
		updates["title"] = *input.Title
	}
	if input.Steps != nil {
		updates["steps"] = stepsRawToString(*input.Steps)
	}

	if err := h.db.Model(&step).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update shared step"})
		return
	}

	h.db.First(&step, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": serializeSharedStep(step)})
}

func (h *SharedStepHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var step models.SharedStep
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&step).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shared step not found"})
		return
	}

	if err := h.db.Delete(&step).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete shared step"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Shared step deleted"}})
}

func (h *SharedStepHandler) IncrementUsage(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var step models.SharedStep
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&step).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shared step not found"})
		return
	}

	if err := h.db.Model(&step).Update("usage_count", gorm.Expr("usage_count + 1")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update usage count"})
		return
	}

	h.db.First(&step, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": serializeSharedStep(step)})
}

func (h *SharedStepHandler) DecrementUsage(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var step models.SharedStep
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&step).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Shared step not found"})
		return
	}

	if err := h.db.Model(&step).Update("usage_count", gorm.Expr("CASE WHEN usage_count > 0 THEN usage_count - 1 ELSE 0 END")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update usage count"})
		return
	}

	h.db.First(&step, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": serializeSharedStep(step)})
}
