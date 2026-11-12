package handlers

import (
	"net/http"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// notifyWorkspaceMutated fans a workspace mutation event out to the
// owner + every member of the bound team (if any). Used by Create /
// Update / SetTeam / Delete so the sidebar selector refreshes for
// everyone who can see the workspace.
func notifyWorkspaceMutated(db *gorm.DB, c *gin.Context, w *models.Workspace, eventType string) {
	if w == nil {
		return
	}
	set := map[string]struct{}{w.OwnerID: {}}
	if w.TeamID != nil && *w.TeamID != "" {
		var memberIDs []string
		db.Session(&gorm.Session{}).
			Table("team_members").
			Select("user_id").
			Where("team_id = ?", *w.TeamID).
			Scan(&memberIDs)
		for _, u := range memberIDs {
			set[u] = struct{}{}
		}
	}
	recipients := make([]string, 0, len(set))
	for u := range set {
		recipients = append(recipients, u)
	}
	realtime.Notify(db, realtime.Event{
		Type:           eventType,
		Recipients:     recipients,
		OriginClientID: originClientID(c),
	})
}

// canSeeWorkspace returns true when userID owns the workspace OR is a
// member of the team the workspace is bound to. Used by Get + the
// collection list filter so a team member can switch to the shared
// workspace via the selector.
func canSeeWorkspace(db *gorm.DB, workspaceID, userID string) bool {
	if workspaceID == "" || userID == "" {
		return false
	}
	var row struct {
		OwnerID string
		TeamID  *string
	}
	if err := db.Session(&gorm.Session{}).
		Table("workspaces").
		Select("owner_id, team_id").
		Where("id = ?", workspaceID).
		Scan(&row).Error; err != nil || row.OwnerID == "" {
		return false
	}
	if row.OwnerID == userID {
		return true
	}
	if row.TeamID == nil || *row.TeamID == "" {
		return false
	}
	var count int64
	db.Session(&gorm.Session{}).
		Table("team_members").
		Where("team_id = ? AND user_id = ?", *row.TeamID, userID).
		Count(&count)
	return count > 0
}

// WorkspaceHandler manages the personal workspace scope. Workspaces are
// owner-only after the Teams RFC — there is no shared workspace, no
// member roster. Sharing has moved to the dedicated Team entity which
// is referenced from public-grant rows on individual collections /
// environments.
type WorkspaceHandler struct {
	db *gorm.DB
}

func NewWorkspaceHandler(db *gorm.DB) *WorkspaceHandler {
	return &WorkspaceHandler{db: db}
}

type createWorkspaceInput struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

type updateWorkspaceInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (h *WorkspaceHandler) CreateWorkspace(c *gin.Context) {
	userID := c.GetString("userID")

	var input createWorkspaceInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	wsType := input.Type
	if wsType == "" {
		wsType = "personal"
	}
	// 'team' kept accepted for backward compat with FE forms still passing
	// it, but the value is informational only — there is no team-shared
	// workspace post-RFC.
	if wsType != "personal" && wsType != "team" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Type must be 'personal' or 'team'"})
		return
	}

	workspace := models.Workspace{
		ID:          uuid.New().String(),
		Name:        input.Name,
		Description: input.Description,
		OwnerID:     userID,
		Type:        wsType,
	}

	if err := h.db.Create(&workspace).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create workspace"})
		return
	}

	notifyWorkspaceMutated(h.db, c, &workspace, "workspace.created")

	c.JSON(http.StatusCreated, gin.H{"data": workspace})
}

// workspaceResponse decorates the workspace row with the bound team's
// display name so the FE selector can render "Pivot QA · Pivot Team"
// without a per-row roundtrip. TeamName stays empty for personal
// workspaces (TeamID nil) and is also empty for the rare case where
// the bound team was soft-deleted.
type workspaceResponse struct {
	models.Workspace
	TeamName string `json:"teamName,omitempty"`
}

func (h *WorkspaceHandler) ListWorkspaces(c *gin.Context) {
	userID := c.GetString("userID")

	// Own + any workspace bound to a team I'm a member of. Single query
	// keeps the round-trip count constant regardless of team membership
	// fan-out. The IN subquery is small (teams a user is in is rarely
	// over a handful) so no index gymnastics needed.
	var workspaces []models.Workspace
	if err := h.db.Session(&gorm.Session{}).
		Where(
			"owner_id = ? OR (team_id IS NOT NULL AND team_id IN ("+
				"SELECT team_id FROM team_members WHERE user_id = ?"+
				"))",
			userID, userID,
		).
		Order("created_at ASC").
		Find(&workspaces).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch workspaces"})
		return
	}

	// First-time bootstrap: every user needs a Personal workspace so the rest
	// of the app (which now scopes by workspace) has somewhere to anchor
	// collections. Pre-existing orphan collections stay unbound so users
	// can see/manage them under the "No Workspace" bucket in the FE
	// selector rather than being silently absorbed into Personal.
	if len(workspaces) == 0 {
		ws, err := h.ensurePersonalWorkspace(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to bootstrap personal workspace"})
			return
		}
		workspaces = []models.Workspace{*ws}
	}

	// Bulk-resolve team display names so the FE can render
	// "<workspace> · <team>" without a per-row roundtrip.
	teamIDs := make([]string, 0)
	for _, w := range workspaces {
		if w.TeamID != nil && *w.TeamID != "" {
			teamIDs = append(teamIDs, *w.TeamID)
		}
	}
	teamNames := map[string]string{}
	if len(teamIDs) > 0 {
		var rows []struct{ ID, Name string }
		h.db.Session(&gorm.Session{}).
			Table("teams").
			Select("id, name").
			Where("id IN ? AND deleted_at IS NULL", teamIDs).
			Scan(&rows)
		for _, r := range rows {
			teamNames[r.ID] = r.Name
		}
	}

	out := make([]workspaceResponse, 0, len(workspaces))
	for _, w := range workspaces {
		resp := workspaceResponse{Workspace: w}
		if w.TeamID != nil && *w.TeamID != "" {
			resp.TeamName = teamNames[*w.TeamID]
		}
		out = append(out, resp)
	}

	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ensurePersonalWorkspace creates a default personal workspace for the user.
// No team-member row is created — workspace ownership is implicit in the
// OwnerID column.
func (h *WorkspaceHandler) ensurePersonalWorkspace(userID string) (*models.Workspace, error) {
	workspace := models.Workspace{
		ID:      uuid.New().String(),
		Name:    "Personal",
		OwnerID: userID,
		Type:    "personal",
	}
	if err := h.db.Create(&workspace).Error; err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (h *WorkspaceHandler) GetWorkspace(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	if !canSeeWorkspace(h.db, id, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}

	var workspace models.Workspace
	if err := h.db.First(&workspace, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": workspace})
}

func (h *WorkspaceHandler) UpdateWorkspace(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var workspace models.Workspace
	if err := h.db.Where("id = ? AND owner_id = ?", id, userID).First(&workspace).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}

	var input updateWorkspaceInput
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

	if err := h.db.Model(&workspace).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update workspace"})
		return
	}

	h.db.First(&workspace, "id = ?", id)
	notifyWorkspaceMutated(h.db, c, &workspace, "workspace.updated")
	c.JSON(http.StatusOK, gin.H{"data": workspace})
}

// setTeamInput accepts null/empty to unbind. TeamID is *string so the
// FE can send `{"teamId": null}` for explicit unbind versus an absent
// field which keeps current binding.
type setTeamInput struct {
	TeamID *string `json:"teamId"`
}

// SetTeam binds (or unbinds) the workspace to a team. Owner-only — the
// team members don't get to choose who shares with them, only the
// owner does. The caller must be a member of the target team so a
// stranger can't dump workspaces into a group they're not in.
func (h *WorkspaceHandler) SetTeam(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var workspace models.Workspace
	if err := h.db.Where("id = ? AND owner_id = ?", id, userID).First(&workspace).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}

	var input setTeamInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Membership check: owner has to actually be in the team they're
	// binding to. Unbind (nil / empty) is always allowed.
	if input.TeamID != nil && *input.TeamID != "" {
		var count int64
		h.db.Session(&gorm.Session{}).
			Table("team_members").
			Where("team_id = ? AND user_id = ?", *input.TeamID, userID).
			Count(&count)
		if count == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "You must be a member of the team to bind a workspace to it"})
			return
		}
	}

	// Snapshot the *old* recipient set so members losing visibility
	// also hear about the change (their sidebar selector should drop
	// the workspace row).
	oldRecipients := map[string]struct{}{workspace.OwnerID: {}}
	if workspace.TeamID != nil && *workspace.TeamID != "" {
		var oldMembers []string
		h.db.Session(&gorm.Session{}).
			Table("team_members").
			Select("user_id").
			Where("team_id = ?", *workspace.TeamID).
			Scan(&oldMembers)
		for _, u := range oldMembers {
			oldRecipients[u] = struct{}{}
		}
	}

	// Persist (NULL on unbind via Updates with explicit map so GORM
	// writes the NULL instead of skipping the zero-value pointer).
	updates := map[string]interface{}{"team_id": nil}
	if input.TeamID != nil && *input.TeamID != "" {
		updates["team_id"] = *input.TeamID
	}
	if err := h.db.Model(&workspace).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update team binding"})
		return
	}
	h.db.First(&workspace, "id = ?", id)

	// Union old + new recipients into one event so every affected
	// client refreshes its workspace list.
	notify := map[string]struct{}{}
	for u := range oldRecipients {
		notify[u] = struct{}{}
	}
	notify[workspace.OwnerID] = struct{}{}
	if workspace.TeamID != nil && *workspace.TeamID != "" {
		var newMembers []string
		h.db.Session(&gorm.Session{}).
			Table("team_members").
			Select("user_id").
			Where("team_id = ?", *workspace.TeamID).
			Scan(&newMembers)
		for _, u := range newMembers {
			notify[u] = struct{}{}
		}
	}
	recipients := make([]string, 0, len(notify))
	for u := range notify {
		recipients = append(recipients, u)
	}
	realtime.Notify(h.db, realtime.Event{
		Type:           "workspace.updated",
		Recipients:     recipients,
		OriginClientID: originClientID(c),
	})

	c.JSON(http.StatusOK, gin.H{"data": workspace})
}

func (h *WorkspaceHandler) DeleteWorkspace(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var workspace models.Workspace
	if err := h.db.Where("id = ? AND owner_id = ?", id, userID).First(&workspace).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}

	// Snapshot recipients BEFORE the delete so the team members still
	// hear about it after the binding row is gone.
	wsCopy := workspace
	notifyWorkspaceMutated(h.db, c, &wsCopy, "workspace.deleted")

	// Sweep workspace-scoped rows back to the "No Workspace" bucket
	// before tearing down the workspace row itself. Without this step
	// collections / envs / db-connections / saved-queries keep pointing
	// at a now-dead workspace_id — they vanish from every workspace
	// view (no filter matches a dangling uuid) while still occupying
	// list space. Search surfaces them but the sidebar doesn't, which
	// looks like "ghost data the user can't reach". Wrapped in a tx so
	// a partial sweep doesn't leak orphans on the failure path.
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Collection{}).
			Where("workspace_id = ?", id).
			UpdateColumn("workspace_id", "").Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Environment{}).
			Where("workspace_id = ?", id).
			UpdateColumn("workspace_id", "").Error; err != nil {
			return err
		}
		if err := tx.Model(&models.DBConnection{}).
			Where("workspace_id = ?", id).
			UpdateColumn("workspace_id", "").Error; err != nil {
			return err
		}
		if err := tx.Model(&models.SavedDBQuery{}).
			Where("workspace_id = ?", id).
			UpdateColumn("workspace_id", "").Error; err != nil {
			return err
		}
		// EnvironmentActivation rows scoped to this workspace are dead
		// weight (they pin "the env active in workspace X" — workspace
		// X is going away). Drop them.
		if err := tx.Where("workspace_id = ?", id).
			Delete(&models.EnvironmentActivation{}).Error; err != nil {
			return err
		}
		return tx.Delete(&workspace).Error
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete workspace"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Workspace deleted"}})
}
