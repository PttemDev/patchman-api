package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TeamHandler manages the new Team entity introduced by the
// TEAMS_AND_PUBLIC_SHARING RFC. Teams are asset-free groups of users
// that can be referenced from public-grant rows on collections and
// environments. Any registered user can create a team; the creator is
// implicitly the first admin.
type TeamHandler struct {
	db *gorm.DB
}

func NewTeamHandler(db *gorm.DB) *TeamHandler {
	return &TeamHandler{db: db}
}

// ── request payloads ────────────────────────────────────────────────

type createTeamInput struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

type updateTeamInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type addMemberInput struct {
	Email string `json:"email" binding:"required"`
	Role  string `json:"role"`
}

type updateMemberRoleInput struct {
	Role string `json:"role" binding:"required"`
}

// ── response shapes ─────────────────────────────────────────────────

// teamWithStats joins a team with the data the FE always needs anyway
// (member count + creator email) so listing pages don't need N+1 follow-up
// requests. Mirrors the shape of `team` plus two derived fields.
type teamWithStats struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatorName string    `json:"creatorName"`
	MemberCount int64     `json:"memberCount"`
	MyRole      string    `json:"myRole"` // 'admin' | 'member' | '' if not a member
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type memberWithUser struct {
	TeamID   string    `json:"teamId"`
	UserID   string    `json:"userId"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// ── internal helpers ───────────────────────────────────────────────

// findMember returns the requesting user's membership row for a team, or
// nil if they aren't a member. Used to gate "must be a member" + "must be
// admin" checks.
func (h *TeamHandler) findMember(teamID, userID string) *models.TeamMember {
	var m models.TeamMember
	if err := h.db.Where("team_id = ? AND user_id = ?", teamID, userID).First(&m).Error; err != nil {
		return nil
	}
	return &m
}

// ── handlers ────────────────────────────────────────────────────────

// CreateTeam — any authenticated user may create a team; they become the
// first admin via an automatic TeamMember row inserted in the same
// transaction.
func (h *TeamHandler) CreateTeam(c *gin.Context) {
	userID := c.GetString("userID")

	var input createTeamInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	team := models.Team{
		ID:          uuid.New().String(),
		Name:        input.Name,
		Description: input.Description,
		CreatedBy:   userID,
	}

	tx := h.db.Begin()
	if err := tx.Create(&team).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create team"})
		return
	}
	creatorMember := models.TeamMember{
		TeamID:   team.ID,
		UserID:   userID,
		Role:     "admin",
		JoinedAt: time.Now(),
	}
	if err := tx.Create(&creatorMember).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to seed team admin"})
		return
	}
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit team"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": team})
}

// ListTeams returns every team the caller can see. Per the RFC's open
// question #1 (resolved: open visibility), every registered user can
// list all teams (internal-tool transparency). `myRole` is populated for
// teams the caller is a member of, empty otherwise. Soft-deleted teams
// (deleted_at IS NOT NULL) are excluded.
func (h *TeamHandler) ListTeams(c *gin.Context) {
	userID := c.GetString("userID")

	type row struct {
		ID          string
		Name        string
		Description string
		CreatedBy   string
		CreatedAt   time.Time
		UpdatedAt   time.Time
		CreatorName string
		MemberCount int64
		MyRole      string
	}
	var rows []row
	// One SQL: left-join the caller's membership row + count of all
	// members + join the creator user once. Keeps the listing endpoint
	// to a single round trip even at high team counts.
	err := h.db.Table("teams").
		Select(`teams.id, teams.name, teams.description, teams.created_by,
		        teams.created_at, teams.updated_at,
		        COALESCE(creator.name, creator.email) AS creator_name,
		        (SELECT COUNT(*) FROM team_members tm WHERE tm.team_id = teams.id) AS member_count,
		        COALESCE(my.role, '') AS my_role`).
		Joins("LEFT JOIN users creator ON creator.id = teams.created_by").
		Joins("LEFT JOIN team_members my ON my.team_id = teams.id AND my.user_id = ?", userID).
		Where("teams.deleted_at IS NULL").
		Order("teams.created_at DESC").
		Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch teams"})
		return
	}

	out := make([]teamWithStats, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamWithStats{
			ID:          r.ID,
			Name:        r.Name,
			Description: r.Description,
			CreatedBy:   r.CreatedBy,
			CreatorName: r.CreatorName,
			MemberCount: r.MemberCount,
			MyRole:      r.MyRole,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// GetTeam returns full detail for a single team. Members may always see
// it; non-members get the public summary (name + description + creator)
// without member roster.
func (h *TeamHandler) GetTeam(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")

	var team models.Team
	if err := h.db.Where("id = ? AND deleted_at IS NULL", teamID).First(&team).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Team not found"})
		return
	}

	myRole := ""
	if m := h.findMember(teamID, userID); m != nil {
		myRole = m.Role
	}

	var memberCount int64
	h.db.Model(&models.TeamMember{}).Where("team_id = ?", teamID).Count(&memberCount)

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":          team.ID,
		"name":        team.Name,
		"description": team.Description,
		"createdBy":   team.CreatedBy,
		"myRole":      myRole,
		"memberCount": memberCount,
		"createdAt":   team.CreatedAt,
		"updatedAt":   team.UpdatedAt,
	}})
}

// UpdateTeam — admin only.
func (h *TeamHandler) UpdateTeam(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")

	m := h.findMember(teamID, userID)
	if m == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You are not a member of this team"})
		return
	}
	if m.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only team admins can update the team"})
		return
	}

	var team models.Team
	if err := h.db.Where("id = ? AND deleted_at IS NULL", teamID).First(&team).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Team not found"})
		return
	}

	var input updateTeamInput
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
	if err := h.db.Model(&team).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update team"})
		return
	}

	h.db.First(&team, "id = ?", teamID)
	c.JSON(http.StatusOK, gin.H{"data": team})
}

// DeleteTeam — soft delete. Admin only. Membership rows cascade through
// FK ON DELETE CASCADE (declared on team_members.team_id). To avoid
// orphan rows accumulating in the public-grant tables (which would
// silently grow forever as teams come and go), we hard-delete every
// collection/environment grant that references this team in the same
// transaction. The team itself stays soft-deleted so audit history is
// preserved; the grants are gone since they have no value once their
// subject team is unreachable.
func (h *TeamHandler) DeleteTeam(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")

	m := h.findMember(teamID, userID)
	if m == nil || m.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only team admins can delete the team"})
		return
	}

	tx := h.db.Begin()
	now := time.Now()
	if err := tx.Model(&models.Team{}).Where("id = ?", teamID).Update("deleted_at", &now).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete team"})
		return
	}
	// Hard-purge any grants pointing at this team — the row is meaningless
	// once the team is gone, and leaving it lets the table balloon over time.
	if err := tx.Where("subject_kind = 'team' AND subject_id = ?", teamID).
		Delete(&models.CollectionPublicGrant{}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clean up collection grants"})
		return
	}
	if err := tx.Where("subject_kind = 'team' AND subject_id = ?", teamID).
		Delete(&models.EnvironmentPublicGrant{}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clean up environment grants"})
		return
	}
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit team deletion"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Team deleted"}})
}

// ListMembers — open to any caller (per RFC Q1 resolution: team membership
// is visible to everyone for transparency in this internal tool).
func (h *TeamHandler) ListMembers(c *gin.Context) {
	teamID := c.Param("id")

	type row struct {
		TeamID   string
		UserID   string
		Email    string
		Name     string
		Role     string
		JoinedAt time.Time
	}
	var rows []row
	err := h.db.Table("team_members tm").
		Select("tm.team_id, tm.user_id, users.email, users.name, tm.role, tm.joined_at").
		Joins("JOIN users ON users.id = tm.user_id").
		Where("tm.team_id = ?", teamID).
		Order("tm.joined_at ASC").
		Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}

	out := make([]memberWithUser, 0, len(rows))
	for _, r := range rows {
		out = append(out, memberWithUser{
			TeamID:   r.TeamID,
			UserID:   r.UserID,
			Email:    r.Email,
			Name:     r.Name,
			Role:     r.Role,
			JoinedAt: r.JoinedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// AddMember — admin only. Look up user by email, default role = 'member'.
func (h *TeamHandler) AddMember(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")

	caller := h.findMember(teamID, userID)
	if caller == nil || caller.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only team admins can add members"})
		return
	}

	var input addMemberInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role := input.Role
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'admin' or 'member'"})
		return
	}

	var invitee models.User
	if err := h.db.Where("email = ?", input.Email).First(&invitee).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registered user with this email"})
		return
	}

	if existing := h.findMember(teamID, invitee.ID); existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a team member"})
		return
	}

	tm := models.TeamMember{
		TeamID:   teamID,
		UserID:   invitee.ID,
		Role:     role,
		JoinedAt: time.Now(),
	}
	if err := h.db.Create(&tm).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": tm})
}

// UpdateMemberRole — admin only. Cannot demote the last admin (would
// leave the team unmanageable).
func (h *TeamHandler) UpdateMemberRole(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")
	targetUserID := c.Param("userId")

	caller := h.findMember(teamID, userID)
	if caller == nil || caller.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only team admins can change roles"})
		return
	}

	target := h.findMember(teamID, targetUserID)
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Member not found"})
		return
	}

	var input updateMemberRoleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Role != "admin" && input.Role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'admin' or 'member'"})
		return
	}

	// Demotion guard: refuse if this would leave zero admins on the team.
	if target.Role == "admin" && input.Role != "admin" {
		var adminCount int64
		h.db.Model(&models.TeamMember{}).Where("team_id = ? AND role = ?", teamID, "admin").Count(&adminCount)
		if adminCount <= 1 {
			c.JSON(http.StatusConflict, gin.H{"error": "Cannot demote the last admin; promote another member first"})
			return
		}
	}

	if err := h.db.Model(&models.TeamMember{}).
		Where("team_id = ? AND user_id = ?", teamID, targetUserID).
		Update("role", input.Role).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update role"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"role": input.Role}})
}

// RemoveMember — admin can remove anyone, members can remove themselves.
// Refuses to remove the last admin (same reasoning as UpdateMemberRole).
func (h *TeamHandler) RemoveMember(c *gin.Context) {
	userID := c.GetString("userID")
	teamID := c.Param("id")
	targetUserID := c.Param("userId")

	caller := h.findMember(teamID, userID)
	if caller == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You are not a member of this team"})
		return
	}

	target := h.findMember(teamID, targetUserID)
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Member not found"})
		return
	}

	isSelf := target.UserID == userID
	if caller.Role != "admin" && !isSelf {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can remove other members"})
		return
	}

	if target.Role == "admin" {
		var adminCount int64
		h.db.Model(&models.TeamMember{}).Where("team_id = ? AND role = ?", teamID, "admin").Count(&adminCount)
		if adminCount <= 1 {
			c.JSON(http.StatusConflict, gin.H{"error": "Cannot remove the last admin; promote another member first"})
			return
		}
	}

	if err := h.db.Where("team_id = ? AND user_id = ?", teamID, targetUserID).
		Delete(&models.TeamMember{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Member removed"}})
}
