package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DBGrantHandler manages delegation grants on a DB connection.
// role='read' → query-only; role='write' → full co-owner. Mutations
// require owner/co-owner (canManage); everyone with access can list.
type DBGrantHandler struct {
	db *gorm.DB
}

func NewDBGrantHandler(db *gorm.DB) *DBGrantHandler { return &DBGrantHandler{db: db} }

type dbGrantInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"` // user | team
	SubjectID   string `json:"subjectId" binding:"required"`
	Role        string `json:"role"` // 'read' (default) | 'write' (co-owner)
}

// Add — owner or 'write' co-owner only.
func (h *DBGrantHandler) Add(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.DBConnection
	if err := h.db.First(&conn, "id = ?", connID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	// Only an owner/co-owner may manage sharing. A user with no access gets
	// 404 (don't confirm existence); a read-grantee gets 403.
	role := dbConnRole(h.db, &conn, userID)
	if role == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if !canManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
		return
	}
	var input dbGrantInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.SubjectKind != "user" && input.SubjectKind != "team" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectKind must be 'user' or 'team'"})
		return
	}
	grantRole := input.Role
	if grantRole == "" {
		grantRole = "read"
	}
	// 'read' = query-only; 'write' = full co-owner. Anything else is
	// rejected so a stray client can't invent a privilege level.
	if grantRole != "read" && grantRole != "write" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'read' or 'write'"})
		return
	}
	grant := models.DBConnectionGrant{
		ID:           uuid.New().String(),
		ConnectionID: connID,
		SubjectKind:  input.SubjectKind,
		SubjectID:    input.SubjectID,
		Role:         grantRole,
		GrantedBy:    userID,
		CreatedAt:    time.Now(),
	}
	if err := h.db.Create(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save grant"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *DBGrantHandler) List(c *gin.Context) {
	connID := c.Param("id")
	var grants []models.DBConnectionGrant
	if err := h.db.Where("connection_id = ?", connID).Order("created_at DESC").Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list grants"})
		return
	}
	// Enrich with label.
	type dto struct {
		ID          string `json:"id"`
		SubjectKind string `json:"subjectKind"`
		SubjectID   string `json:"subjectId"`
		Role        string `json:"role"`
		Label       string `json:"label"`
		GrantedBy   string `json:"grantedBy"`
		CreatedAt   string `json:"createdAt"`
	}
	out := make([]dto, 0, len(grants))
	for _, g := range grants {
		label := g.SubjectID
		if g.SubjectKind == "user" {
			var u struct{ ID, Email, Name string }
			h.db.Table("users").Select("id, email, name").Where("id = ?", g.SubjectID).Scan(&u)
			if u.Name != "" {
				label = u.Name
			} else if u.Email != "" {
				label = u.Email
			}
		} else if g.SubjectKind == "team" {
			var t struct{ ID, Name string }
			h.db.Table("teams").Select("id, name").Where("id = ? AND deleted_at IS NULL", g.SubjectID).Scan(&t)
			if t.Name != "" {
				label = t.Name
			}
		}
		out = append(out, dto{
			ID: g.ID, SubjectKind: g.SubjectKind, SubjectID: g.SubjectID,
			Role: g.Role, Label: label, GrantedBy: g.GrantedBy,
			CreatedAt: g.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *DBGrantHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	grantID := c.Param("grantId")
	// Owner or 'write' co-owner may revoke grants. Grant rows don't carry
	// the owner directly, so resolve the caller's role off the connection.
	var conn models.DBConnection
	if err := h.db.First(&conn, "id = ?", connID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	role := dbConnRole(h.db, &conn, userID)
	if role == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if !canManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
		return
	}
	res := h.db.Where("id = ? AND connection_id = ?", grantID, connID).Delete(&models.DBConnectionGrant{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete grant"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// ── Saved queries ────────────────────────────────────────────────────

type DBSavedQueryHandler struct{ db *gorm.DB }

func NewDBSavedQueryHandler(db *gorm.DB) *DBSavedQueryHandler { return &DBSavedQueryHandler{db: db} }

type saveQueryInput struct {
	ConnectionID string `json:"connectionId" binding:"required"`
	Name         string `json:"name" binding:"required"`
	QueryText    string `json:"queryText" binding:"required"`
	WorkspaceID  string `json:"workspaceId"`
	Folder       string `json:"folder"`
	Tags         string `json:"tags"`
}

func (h *DBSavedQueryHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Query("connectionId")
	q := h.db.Where("user_id = ?", userID).Order("sort_order ASC, created_at DESC")
	if connID != "" {
		q = q.Where("connection_id = ?", connID)
	}
	var rows []models.SavedDBQuery
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list saved queries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

func (h *DBSavedQueryHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input saveQueryInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row := models.SavedDBQuery{
		ID: uuid.New().String(), UserID: userID, WorkspaceID: input.WorkspaceID,
		ConnectionID: input.ConnectionID, Name: input.Name, QueryText: input.QueryText,
		Folder: input.Folder, Tags: input.Tags,
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": row})
}

func (h *DBSavedQueryHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var row models.SavedDBQuery
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	var input struct {
		Name      *string `json:"name"`
		QueryText *string `json:"queryText"`
		SortOrder *int    `json:"sortOrder"`
		Folder    *string `json:"folder"`
		Tags      *string `json:"tags"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.QueryText != nil {
		updates["query_text"] = *input.QueryText
	}
	if input.SortOrder != nil {
		updates["sort_order"] = *input.SortOrder
	}
	if input.Folder != nil {
		updates["folder"] = *input.Folder
	}
	if input.Tags != nil {
		updates["tags"] = *input.Tags
	}
	h.db.Model(&row).Updates(updates)
	h.db.First(&row, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": row})
}

func (h *DBSavedQueryHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.SavedDBQuery{})
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// ── History ─────────────────────────────────────────────────────────

type DBHistoryHandler struct{ db *gorm.DB }

func NewDBHistoryHandler(db *gorm.DB) *DBHistoryHandler { return &DBHistoryHandler{db: db} }

func (h *DBHistoryHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Query("connectionId")
	limit := 50
	q := h.db.Where("user_id = ? OR delegated_owner = ?", userID, userID).
		Order("executed_at DESC").
		Limit(limit)
	if connID != "" {
		q = q.Where("connection_id = ?", connID)
	}
	var rows []models.DBQueryHistory
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list history"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}
