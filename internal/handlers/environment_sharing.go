package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// EnvironmentSharingHandler is the environment-side parity of
// CollectionSharingHandler. Behavior is identical — see that file for
// the design rationale (RFC questions resolved, "everyone" merge rules,
// owner-self editor guard, etc.). Only model/table names differ.
type EnvironmentSharingHandler struct {
	db *gorm.DB
}

func NewEnvironmentSharingHandler(db *gorm.DB) *EnvironmentSharingHandler {
	return &EnvironmentSharingHandler{db: db}
}

func (h *EnvironmentSharingHandler) SetVisibility(c *gin.Context) {
	userID := c.GetString("userID")
	envID := c.Param("id")

	var env models.Environment
	if err := h.db.First(&env, "id = ?", envID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	if env.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can change visibility"})
		return
	}

	var input setVisibilityInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Visibility != "public" && input.Visibility != "private" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Visibility must be 'public' or 'private'"})
		return
	}
	if env.Visibility == input.Visibility {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": env.Visibility}})
		return
	}

	tx := h.db.Begin()
	if err := tx.Model(&env).Update("visibility", input.Visibility).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update visibility"})
		return
	}

	if input.Visibility == "public" {
		now := time.Now()
		grants := []models.EnvironmentPublicGrant{
			{ID: uuid.New().String(), EnvironmentID: envID, Role: "editor", SubjectKind: "user", SubjectID: &userID, CreatedAt: now},
			{ID: uuid.New().String(), EnvironmentID: envID, Role: "viewer", SubjectKind: "everyone", SubjectID: nil, CreatedAt: now},
		}
		if err := tx.Create(&grants).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to seed default grants"})
			return
		}
	} else {
		if err := tx.Where("environment_id = ?", envID).Delete(&models.EnvironmentPublicGrant{}).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear grants"})
			return
		}
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit visibility change"})
		return
	}

	// Visibility flips change who *can* see the env; broadcast wide so
	// every previously-eligible viewer refreshes (Public accordion adds
	// or removes the row). Mirrors the same pattern in collection_sharing.
	if input.Visibility == "public" {
		notifyRT(h.db, c, realtime.Event{
			Type:          realtime.EventEnvironmentUpdated,
			EnvironmentID: envID,
			Everyone:      true,
		})
	} else {
		notifyEnvironmentMutated(h.db, c, envID, realtime.EventEnvironmentUpdated)
		notifyRT(h.db, c, realtime.Event{
			Type:          realtime.EventEnvironmentUpdated,
			EnvironmentID: envID,
			Everyone:      true,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": input.Visibility}})
}

func (h *EnvironmentSharingHandler) ListGrants(c *gin.Context) {
	userID := c.GetString("userID")
	envID := c.Param("id")

	if !ResolveEnvironmentRole(h.db, userID, envID).AtLeast(ViewerRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "No access to this environment"})
		return
	}

	var grants []models.EnvironmentPublicGrant
	if err := h.db.Where("environment_id = ?", envID).
		Order("role DESC, created_at ASC").
		Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch grants"})
		return
	}

	userIDs := []string{}
	teamIDs := []string{}
	for _, g := range grants {
		if g.SubjectID == nil {
			continue
		}
		switch g.SubjectKind {
		case "user":
			userIDs = append(userIDs, *g.SubjectID)
		case "team":
			teamIDs = append(teamIDs, *g.SubjectID)
		}
	}
	userNames := map[string]string{}
	if len(userIDs) > 0 {
		var rows []struct{ ID, Email, Name string }
		h.db.Table("users").Select("id, email, name").Where("id IN (?)", userIDs).Scan(&rows)
		for _, r := range rows {
			label := r.Name
			if label == "" {
				label = r.Email
			}
			userNames[r.ID] = label
		}
	}
	teamNames := map[string]string{}
	if len(teamIDs) > 0 {
		var rows []struct{ ID, Name string }
		h.db.Table("teams").Select("id, name").Where("id IN (?) AND deleted_at IS NULL", teamIDs).Scan(&rows)
		for _, r := range rows {
			teamNames[r.ID] = r.Name
		}
	}

	type grantDTO struct {
		ID          string `json:"id"`
		Role        string `json:"role"`
		SubjectKind string `json:"subjectKind"`
		SubjectID   string `json:"subjectId,omitempty"`
		Label       string `json:"label"`
	}
	out := make([]grantDTO, 0, len(grants))
	for _, g := range grants {
		dto := grantDTO{ID: g.ID, Role: g.Role, SubjectKind: g.SubjectKind}
		if g.SubjectID != nil {
			dto.SubjectID = *g.SubjectID
		}
		switch g.SubjectKind {
		case "user":
			dto.Label = userNames[dto.SubjectID]
		case "team":
			dto.Label = teamNames[dto.SubjectID]
		case "everyone":
			dto.Label = "Everyone"
		}
		out = append(out, dto)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *EnvironmentSharingHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	envID := c.Param("id")

	var env models.Environment
	if err := h.db.First(&env, "id = ?", envID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	// Owner OR editor — visibility flips stay owner-only above; granular
	// list edits are editor-allowed (mirrors the collection rebalance).
	envH := &EnvironmentHandler{db: h.db}
	if role, _, ok := envH.envWithRole(envID, userID); !ok || (role != models.EnvRoleOwner && role != models.EnvRoleEditor) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to change grants"})
		return
	}
	if env.Visibility != "public" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Environment is not public; flip visibility first"})
		return
	}

	var input addGrantInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Role != "editor" && input.Role != "viewer" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'editor' or 'viewer'"})
		return
	}
	if input.SubjectKind != "user" && input.SubjectKind != "team" && input.SubjectKind != "everyone" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectKind must be 'user', 'team', or 'everyone'"})
		return
	}
	if input.SubjectKind != "everyone" && (input.SubjectID == nil || *input.SubjectID == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectId is required for user/team grants"})
		return
	}

	var everyoneCount int64
	h.db.Model(&models.EnvironmentPublicGrant{}).
		Where("environment_id = ? AND role = ? AND subject_kind = 'everyone'", envID, input.Role).
		Count(&everyoneCount)

	tx := h.db.Begin()

	if input.SubjectKind == "everyone" {
		if err := tx.Where("environment_id = ? AND role = ? AND subject_kind <> 'everyone'", envID, input.Role).
			Delete(&models.EnvironmentPublicGrant{}).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear specifics"})
			return
		}
		if everyoneCount > 0 {
			tx.Commit()
			var existing models.EnvironmentPublicGrant
			h.db.Where("environment_id = ? AND role = ? AND subject_kind = 'everyone'", envID, input.Role).First(&existing)
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	} else {
		if everyoneCount > 0 {
			tx.Rollback()
			c.JSON(http.StatusConflict, gin.H{"error": "Cannot add specific subject while 'everyone' is in the list — remove 'everyone' first"})
			return
		}
		var existing models.EnvironmentPublicGrant
		err := tx.Where("environment_id = ? AND role = ? AND subject_kind = ? AND subject_id = ?",
			envID, input.Role, input.SubjectKind, *input.SubjectID).First(&existing).Error
		if err == nil {
			tx.Commit()
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	}

	grant := models.EnvironmentPublicGrant{
		ID:            uuid.New().String(),
		EnvironmentID: envID,
		Role:          input.Role,
		SubjectKind:   input.SubjectKind,
		SubjectID:     input.SubjectID,
		CreatedAt:     time.Now(),
	}
	if err := tx.Create(&grant).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add grant"})
		return
	}
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit grant"})
		return
	}

	if input.SubjectKind == "everyone" {
		notifyRT(h.db, c, realtime.Event{
			Type:          realtime.EventEnvironmentUpdated,
			EnvironmentID: envID,
			Everyone:      true,
		})
	} else {
		notifyEnvironmentMutated(h.db, c, envID, realtime.EventEnvironmentUpdated)
	}

	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *EnvironmentSharingHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	envID := c.Param("id")
	grantID := c.Param("grantId")

	var env models.Environment
	if err := h.db.First(&env, "id = ?", envID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	// Owner OR editor — same scope as AddGrant.
	envH := &EnvironmentHandler{db: h.db}
	if role, _, ok := envH.envWithRole(envID, userID); !ok || (role != models.EnvRoleOwner && role != models.EnvRoleEditor) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to change grants"})
		return
	}

	var grant models.EnvironmentPublicGrant
	if err := h.db.Where("id = ? AND environment_id = ?", grantID, envID).First(&grant).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}

	if grant.Role == "editor" && grant.SubjectKind == "user" && grant.SubjectID != nil && *grant.SubjectID == env.UserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Owner cannot be removed from the Editors list"})
		return
	}

	if err := h.db.Delete(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete grant"})
		return
	}

	notifyEnvironmentMutated(h.db, c, envID, realtime.EventEnvironmentUpdated)
	if grant.SubjectKind == "everyone" || grant.SubjectKind == "team" {
		notifyRT(h.db, c, realtime.Event{
			Type:          realtime.EventEnvironmentUpdated,
			EnvironmentID: envID,
			Everyone:      true,
		})
	} else if grant.SubjectKind == "user" && grant.SubjectID != nil {
		notifyRT(h.db, c, realtime.Event{
			Type:          realtime.EventEnvironmentUpdated,
			EnvironmentID: envID,
			Recipients:    []string{*grant.SubjectID},
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Grant removed"}})
}

