package handlers

import (
	"net/http"
	"strconv"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type AuditHandler struct {
	db *gorm.DB
}

func NewAuditHandler(db *gorm.DB) *AuditHandler {
	return &AuditHandler{db: db}
}

func (h *AuditHandler) ListAuditLogs(c *gin.Context) {
	userID := c.GetString("userID")

	// Pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	query := h.db.Model(&models.AuditLog{}).Where("user_id = ?", userID)

	// Apply filters
	if action := c.Query("action"); action != "" {
		query = query.Where("action = ?", action)
	}
	if resource := c.Query("resource"); resource != "" {
		query = query.Where("resource = ?", resource)
	}
	if filterUserID := c.Query("userId"); filterUserID != "" {
		query = query.Where("user_id = ?", filterUserID)
	}
	if from := c.Query("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			query = query.Where("created_at >= ?", t)
		}
	}
	if to := c.Query("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			query = query.Where("created_at <= ?", t)
		}
	}

	// Get total count
	var total int64
	query.Count(&total)

	// Fetch paginated results
	var logs []models.AuditLog
	if err := query.Order("created_at DESC").
		Offset(offset).Limit(pageSize).
		Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch audit logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": logs,
		"pagination": gin.H{
			"page":     page,
			"pageSize": pageSize,
			"total":    total,
		},
	})
}

func (h *AuditHandler) ListActivityFeed(c *gin.Context) {
	userID := c.GetString("userID")

	// Pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	query := h.db.Model(&models.ActivityFeed{}).Where("user_id = ?", userID)

	// Get total count
	var total int64
	query.Count(&total)

	// Fetch paginated results with User relation
	var activities []models.ActivityFeed
	if err := h.db.Preload("User").
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Offset(offset).Limit(pageSize).
		Find(&activities).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch activity feed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": activities,
		"pagination": gin.H{
			"page":     page,
			"pageSize": pageSize,
			"total":    total,
		},
	})
}
