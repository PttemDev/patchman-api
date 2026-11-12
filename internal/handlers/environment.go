package handlers

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// grantRoleFor resolves the effective env-grant role (editor or viewer)
// for `userID` against a public env's grants table. Used by the List
// path so a grant-only collaborator's row carries the right role
// downstream (drives the secret-masking branch). Tries editor first
// because editor strictly subsumes viewer access; falls back to
// viewer. Returns "viewer" as a safe default if neither matches —
// the synthesized row only exists because the caller already passed
// the canSee check above.
func grantRoleFor(db *gorm.DB, environmentID, userID string) string {
	role := ResolveEnvironmentRole(db, userID, environmentID)
	switch role {
	case EditorRole:
		return models.EnvRoleEditor
	case ViewerRole:
		return models.EnvRoleViewer
	}
	return models.EnvRoleViewer
}

// notifyEnvironmentMutated is the env-side analogue of
// notifyCollectionMutated. Resolves recipients (owner + share users +
// public grant subjects) and emits an event keyed on the environment.
// The gin context is optional — pass it so X-Client-Id propagates and
// the originating tab can suppress its self-echo refresh.
func notifyEnvironmentMutated(db *gorm.DB, c *gin.Context, environmentID, eventType string) {
	if environmentID == "" {
		return
	}
	recipients, everyone := realtime.EnvironmentRecipients(db, environmentID)
	realtime.Notify(db, realtime.Event{
		Type:           eventType,
		EnvironmentID:  environmentID,
		Recipients:     recipients,
		Everyone:       everyone,
		OriginClientID: originClientID(c),
	})
}

type EnvironmentHandler struct {
	db *gorm.DB
}

func NewEnvironmentHandler(db *gorm.DB) *EnvironmentHandler {
	return &EnvironmentHandler{db: db}
}

type createEnvironmentInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name" binding:"required"`
	Variables   string  `json:"variables"`
	WorkspaceID *string `json:"workspaceId"`
}

type updateEnvironmentInput struct {
	Name        *string `json:"name"`
	Variables   *string `json:"variables"`
	IsActive    *bool   `json:"isActive"`
	WorkspaceID *string `json:"workspaceId"`
}

// envVariable is the shape we serialise [{key, value, enabled, secret}]
// into / out of. Stored as a JSON string in environments.variables but
// always materialised to this struct for masking + format conversion.
type envVariable struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Enabled bool   `json:"enabled"`
	Secret  bool   `json:"secret"`
}

// envResponse is the wire shape List/Update return. Adds the role of
// the requesting user plus shared-by metadata. `isActive` rides on the
// embedded Environment field — we stamp it directly from the per-user
// activation lookup before serialising so the wire tag isn't ambiguous
// against the legacy Environment.IsActive column.
type envResponse struct {
	models.Environment
	Role       string `json:"role"`
	Shared     bool   `json:"shared,omitempty"`
	SharedByMe bool   `json:"sharedByMe,omitempty"`
	OwnerName  string `json:"ownerName,omitempty"`
	OwnerEmail string `json:"ownerEmail,omitempty"`
}

// ----------------------------------------------------------------------
// Read paths
// ----------------------------------------------------------------------

// List returns the user's own environments + every environment shared
// with them. Workspace filter applies to OWN envs only — shared envs
// have no workspace-relationship with the joiner (same reasoning as
// collection.List), so filtering would hide them every time the joiner
// switched workspaces.
//
// Variables are masked according to role: Viewer sees "***" for any
// secret variable, Editor/Owner see raw values. The reveal endpoint
// supports per-variable opt-in for Viewers.
func (h *EnvironmentHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	workspaceID := c.Query("workspaceId")

	// Own envs (same workspace logic as before)
	ownQuery := h.db.Session(&gorm.Session{}).
		Model(&models.Environment{}).
		Where("user_id = ?", userID)
	if workspaceID != "" {
		ownQuery = ownQuery.Where("workspace_id = ? OR workspace_id IS NULL OR workspace_id = ?", workspaceID, "")
	}
	var ownEnvs []models.Environment
	if err := ownQuery.Order("created_at ASC").Find(&ownEnvs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch environments"})
		return
	}

	// Shared envs come from two sources, deduped on environment id:
	//   a) Legacy EnvironmentShare collaborator rows (user_id = me)
	//   b) Public grants — visibility='public' AND a matching
	//      environment_public_grants row (user, team membership, or
	//      'everyone'). This is the path used by the new share-dialog
	//      Editors/Viewers panel; without it, adding someone via the
	//      public grants surface had no effect on the recipient's List
	//      response and the env never appeared in their selector.
	var shares []models.EnvironmentShare
	h.db.Session(&gorm.Session{}).Where("user_id = ?", userID).Find(&shares)
	// A share row's own role can be below what another path grants (a
	// team admin in the env's workspace edits it), so report the
	// resolved max-wins role, as the write paths do.
	for i := range shares {
		shares[i].Role = grantRoleFor(h.db, shares[i].EnvironmentID, userID)
	}

	// Resolve grant-accessible env ids in one query. Two branches:
	//   a) visibility=public AND a matching public-grant row (user,
	//      team membership, or 'everyone')
	//   b) env's workspace is bound to a team I'm in (workspace cascade
	//      — gives viewer access regardless of the env's own visibility)
	var grantEnvIDs []string
	h.db.Session(&gorm.Session{}).
		Table("environments").
		Select("environments.id").
		Where(
			"(environments.visibility = 'public' AND EXISTS ("+
				"   SELECT 1 FROM environment_public_grants g"+
				"   WHERE g.environment_id = environments.id"+
				"     AND ( (g.subject_kind = 'user' AND g.subject_id = ?)"+
				"        OR (g.subject_kind = 'team' AND g.subject_id IN ("+
				"             SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?"+
				"           ))"+
				"        OR (g.subject_kind = 'everyone') ) ))"+
				" OR (environments.workspace_id IN ("+
				"   SELECT w.id FROM workspaces w"+
				"   WHERE w.team_id IS NOT NULL"+
				"     AND w.team_id IN (SELECT team_id FROM team_members WHERE user_id = ?)"+
				" ))",
			userID, userID, userID).
		Pluck("environments.id", &grantEnvIDs)

	// Dedup helper — own envs + share rows already account for the
	// caller's owned envs, so we only need to add grant ids that
	// AREN'T already covered.
	seen := map[string]struct{}{}
	for _, e := range ownEnvs {
		seen[e.ID] = struct{}{}
	}
	for _, s := range shares {
		seen[s.EnvironmentID] = struct{}{}
	}
	for _, envID := range grantEnvIDs {
		if _, ok := seen[envID]; ok {
			continue
		}
		// Synthesize a share-shaped row so the loop below treats this
		// like a legacy share collaborator. The role is resolved via
		// ResolveEnvironmentRole downstream; we set the role here to
		// editor when an editor grant matches and viewer otherwise so
		// the secret-mask path picks the right answer without a second
		// query. Default to viewer (safer; secrets stay masked) and let
		// the role lookup correct upward.
		shares = append(shares, models.EnvironmentShare{
			EnvironmentID: envID,
			Role:          grantRoleFor(h.db, envID, userID),
		})
		seen[envID] = struct{}{}
	}

	// Per-user activation lookup so we can stamp `isActive` per env per
	// workspace. We resolve the active env relative to the workspace
	// the FE asked about; if the FE didn't pass a workspace we look at
	// the global bucket (workspaceId="").
	activationKey := workspaceID
	if activationKey == "__none__" {
		activationKey = ""
	}
	var activation models.EnvironmentActivation
	h.db.Where("user_id = ? AND workspace_id = ?", userID, activationKey).First(&activation)

	// Bulk-collect collaborator counts on own envs (for the shared-by-me
	// badge in the sidebar).
	ownIDs := make([]string, 0, len(ownEnvs))
	for _, e := range ownEnvs {
		ownIDs = append(ownIDs, e.ID)
	}
	ownCollabCount := map[string]int{}
	if len(ownIDs) > 0 {
		type row struct {
			EnvironmentID string
			Count         int64
		}
		var rows []row
		h.db.Table("environment_shares").
			Select("environment_id, COUNT(*) AS count").
			Where("environment_id IN ? AND user_id IS NOT NULL AND user_id != ''", ownIDs).
			Group("environment_id").
			Find(&rows)
		for _, r := range rows {
			ownCollabCount[r.EnvironmentID] = int(r.Count)
		}
	}

	out := make([]envResponse, 0, len(ownEnvs)+len(shares))

	for _, env := range ownEnvs {
		// Stamp isActive directly on the embedded record so the JSON
		// output is unambiguous, then build the response.
		env.IsActive = activation.EnvironmentID == env.ID
		out = append(out, envResponse{
			Environment: env,
			Role:        models.EnvRoleOwner,
			SharedByMe:  ownCollabCount[env.ID] > 0,
		})
	}

	for _, share := range shares {
		var env models.Environment
		if err := h.db.First(&env, "id = ?", share.EnvironmentID).Error; err != nil {
			continue
		}
		// Mask Viewer's secrets before exposing on the wire.
		if share.Role == models.EnvRoleViewer {
			env.Variables = maskSecretValues(env.Variables)
		}
		env.IsActive = activation.EnvironmentID == env.ID
		var owner models.User
		ownerName, ownerEmail := "", ""
		if err := h.db.Select("name, email").First(&owner, "id = ?", env.UserID).Error; err == nil {
			ownerName = owner.Name
			ownerEmail = owner.Email
		}
		out = append(out, envResponse{
			Environment: env,
			Role:        share.Role,
			Shared:      true,
			OwnerName:   ownerName,
			OwnerEmail:  ownerEmail,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ----------------------------------------------------------------------
// Write paths (Create / Update / Delete)
// ----------------------------------------------------------------------

func (h *EnvironmentHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createEnvironmentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	workspaceID := ""
	if input.WorkspaceID != nil {
		workspaceID = *input.WorkspaceID
	}

	env := models.Environment{
		ID:          newID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Name:        input.Name,
		Variables:   input.Variables,
	}

	if err := h.db.Create(&env).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create environment"})
		return
	}

	notifyEnvironmentMutated(h.db, c, env.ID, realtime.EventEnvironmentCreated)

	c.JSON(http.StatusCreated, gin.H{"data": env})
}

func (h *EnvironmentHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	role, env, ok := h.envWithRole(id, userID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	if role != models.EnvRoleOwner && role != models.EnvRoleEditor {
		c.JSON(http.StatusForbidden, gin.H{"error": "Read-only access — ask the owner to grant Editor role"})
		return
	}

	var input updateEnvironmentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Name != nil && role == models.EnvRoleOwner {
		// Editors can edit variables but not rename the env — rename is
		// owner-only so a collaborator can't surprise the owner with a
		// title change in their own list.
		updates["name"] = *input.Name
	}
	if input.Variables != nil {
		updates["variables"] = *input.Variables
	}
	// IsActive is now per-user. Honour it through the activation table
	// instead of writing the legacy column. Owner toggling their own
	// activation flips their own activation row, not anyone else's.
	if input.IsActive != nil && *input.IsActive {
		h.setActivation(userID, env.WorkspaceID, env.ID)
	}
	// WorkspaceID flip is owner-only — joiners shouldn't be able to
	// move someone else's env between workspaces.
	if input.WorkspaceID != nil && role == models.EnvRoleOwner {
		updates["workspace_id"] = *input.WorkspaceID
	}

	if len(updates) > 0 {
		if err := h.db.Model(&models.Environment{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update environment"})
			return
		}
	}

	h.db.First(&env, "id = ?", id)
	notifyEnvironmentMutated(h.db, c, env.ID, realtime.EventEnvironmentUpdated)
	c.JSON(http.StatusOK, gin.H{"data": env})
}

func (h *EnvironmentHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var env models.Environment
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&env).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	// Snapshot recipients before the rows go away — once shares are
	// gone, EnvironmentRecipients returns owner-only.
	recipients, everyone := realtime.EnvironmentRecipients(h.db, id)

	// Tear down related share rows + activations so we don't leave
	// orphans pointing at a missing environment.
	h.db.Where("environment_id = ?", id).Delete(&models.EnvironmentShare{})
	h.db.Where("environment_id = ?", id).Delete(&models.EnvironmentActivation{})
	if err := h.db.Delete(&env).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete environment"})
		return
	}

	notifyRT(h.db, c, realtime.Event{
		Type:          realtime.EventEnvironmentDeleted,
		EnvironmentID: id,
		Recipients:    recipients,
		Everyone:      everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Environment deleted"}})
}

// Activate sets THIS user's active environment for the workspace they
// are CURRENTLY viewing (?workspaceId=…), not for the env's own
// workspace. A user can flip a global env on while looking at workspace
// A; their activation row is keyed on (user, A) and List for workspace
// A surfaces it as active. Switching to workspace B then shows whatever
// they activated for B, independent of the env's own scope.
func (h *EnvironmentHandler) Activate(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	workspaceID := c.Query("workspaceId")
	if workspaceID == "__none__" {
		workspaceID = ""
	}

	role, env, ok := h.envWithRole(id, userID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	_ = role // anyone with access can activate for themselves
	h.setActivation(userID, workspaceID, env.ID)

	// Echo back the env with isActive stamped so the FE can patch its
	// store without a follow-up List round-trip.
	env.IsActive = true
	resp := envResponse{Environment: env, Role: role}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// ----------------------------------------------------------------------
// Sharing
// ----------------------------------------------------------------------

type setShareInput struct {
	Role string `json:"role"` // viewer | editor — defaults to viewer
}

// CreateShare generates (or returns the existing) share token row.
// Owner-only.
func (h *EnvironmentHandler) CreateShare(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var env models.Environment
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&env).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	var input setShareInput
	_ = c.ShouldBindJSON(&input)
	role := strings.ToLower(strings.TrimSpace(input.Role))
	if role != models.EnvRoleEditor {
		role = models.EnvRoleViewer
	}

	// Reuse an existing token row if one is already in place — owner
	// re-clicking "Share" should yield the same code, not a fresh one
	// that orphans previous joiners.
	var existing models.EnvironmentShare
	if err := h.db.Where("environment_id = ? AND user_id IS NULL", id).First(&existing).Error; err == nil {
		// Allow role change on the link itself so the owner can downgrade
		// new joiners after the fact.
		if existing.Role != role {
			h.db.Model(&existing).UpdateColumn("role", role)
			existing.Role = role
		}
		c.JSON(http.StatusOK, gin.H{"data": existing})
		return
	}

	share := models.EnvironmentShare{
		ID:            uuid.New().String(),
		EnvironmentID: id,
		ShareToken:    randomShareToken(),
		Role:          role,
	}
	if err := h.db.Create(&share).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create share"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": share})
}

// JoinByToken resolves a share token into a collaborator row.
type joinShareInput struct {
	ShareToken string `json:"shareToken" binding:"required"`
}

func (h *EnvironmentHandler) JoinByToken(c *gin.Context) {
	userID := c.GetString("userID")

	var input joinShareInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var link models.EnvironmentShare
	if err := h.db.Where("share_token = ? AND user_id IS NULL", input.ShareToken).First(&link).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid share code"})
		return
	}

	var env models.Environment
	if err := h.db.First(&env, "id = ?", link.EnvironmentID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	if env.UserID == userID {
		// Owner clicking their own link — idempotent success so the FE
		// can finish the redirect without a scary error screen.
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"environment": env, "alreadyOwner": true}})
		return
	}

	// Already-a-collaborator → idempotent 200 (same pattern as
	// collection JoinByToken).
	var existing models.EnvironmentShare
	if err := h.db.Where("environment_id = ? AND user_id = ?", link.EnvironmentID, userID).First(&existing).Error; err == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"environment": env, "alreadyJoined": true}})
		return
	}

	collab := models.EnvironmentShare{
		ID:            uuid.New().String(),
		EnvironmentID: link.EnvironmentID,
		ShareToken:    link.ShareToken,
		UserID:        &userID,
		Role:          link.Role,
	}
	if err := h.db.Create(&collab).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join"})
		return
	}

	notifyEnvironmentMutated(h.db, c, link.EnvironmentID, realtime.EventEnvironmentShared)

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"environment": env, "message": "Joined successfully"}})
}

// ListCollaborators — owner + collab can both list. Viewers see the
// list (so they know who else has access) but only the owner can mutate
// it.
func (h *EnvironmentHandler) ListCollaborators(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	role, _, ok := h.envWithRole(id, userID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	_ = role

	type collabResponse struct {
		models.EnvironmentShare
		UserName  string `json:"userName"`
		UserEmail string `json:"userEmail"`
	}

	var shares []models.EnvironmentShare
	h.db.Where("environment_id = ? AND user_id IS NOT NULL", id).Find(&shares)

	result := make([]collabResponse, 0, len(shares))
	for _, s := range shares {
		var user models.User
		name, email := "", ""
		if s.UserID != nil {
			if err := h.db.First(&user, "id = ?", *s.UserID).Error; err == nil {
				name = user.Name
				email = user.Email
			}
		}
		result = append(result, collabResponse{
			EnvironmentShare: s,
			UserName:         name,
			UserEmail:        email,
		})
	}

	var link models.EnvironmentShare
	var token string
	var linkRole string
	if err := h.db.Where("environment_id = ? AND user_id IS NULL", id).First(&link).Error; err == nil {
		token = link.ShareToken
		linkRole = link.Role
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"collaborators": result,
		"shareToken":    token,
		"linkRole":      linkRole,
	}})
}

// RemoveCollaborator — owner-only.
func (h *EnvironmentHandler) RemoveCollaborator(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	target := c.Param("userId")

	var env models.Environment
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&env).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can remove collaborators"})
		return
	}

	h.db.Where("environment_id = ? AND user_id = ?", id, target).Delete(&models.EnvironmentShare{})
	// Drop their activation row too — otherwise the env would stay
	// "active" for them silently after losing access.
	h.db.Where("environment_id = ? AND user_id = ?", id, target).Delete(&models.EnvironmentActivation{})

	// Notify remaining recipients AND the removed user so their sidebar
	// loses the env without manual refresh.
	recipients, everyone := realtime.EnvironmentRecipients(h.db, id)
	recipients = append(recipients, target)
	notifyRT(h.db, c, realtime.Event{
		Type:          realtime.EventEnvironmentUnshared,
		EnvironmentID: id,
		Recipients:    recipients,
		Everyone:      everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Collaborator removed"}})
}

// LeaveShare — collaborator self-removes.
func (h *EnvironmentHandler) LeaveShare(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var env models.Environment
	if err := h.db.First(&env, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	if env.UserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Owners cannot leave their own environment"})
		return
	}
	res := h.db.Where("environment_id = ? AND user_id = ?", id, userID).Delete(&models.EnvironmentShare{})
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "You are not a collaborator on this environment"})
		return
	}
	h.db.Where("environment_id = ? AND user_id = ?", id, userID).Delete(&models.EnvironmentActivation{})

	recipients, everyone := realtime.EnvironmentRecipients(h.db, id)
	recipients = append(recipients, userID)
	notifyRT(h.db, c, realtime.Event{
		Type:          realtime.EventEnvironmentUnshared,
		EnvironmentID: id,
		Recipients:    recipients,
		Everyone:      everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Left shared environment"}})
}

// RevokeShare — owner-only, drops the share token + every collaborator.
func (h *EnvironmentHandler) RevokeShare(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var env models.Environment
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&env).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can revoke sharing"})
		return
	}

	// Snapshot recipients BEFORE the delete so revoked collaborators
	// still hear about it.
	recipients, everyone := realtime.EnvironmentRecipients(h.db, id)

	h.db.Where("environment_id = ?", id).Delete(&models.EnvironmentShare{})
	// Sweep activations from previous collaborators too so they're not
	// left pointing at an env they can no longer see.
	h.db.Where("environment_id = ? AND user_id != ?", id, userID).Delete(&models.EnvironmentActivation{})

	notifyRT(h.db, c, realtime.Event{
		Type:          realtime.EventEnvironmentUnshared,
		EnvironmentID: id,
		Recipients:    recipients,
		Everyone:      everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Sharing revoked"}})
}

// RevealSecret returns the raw value of a single secret variable for a
// Viewer. Editors and Owners get raw values through List anyway and
// don't hit this path. Logs an audit-trail event so we have forensics
// if a secret leaks through this path. Rate-limited indirectly by the
// audit middleware (no specialised limit yet).
func (h *EnvironmentHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	key := c.Param("key")

	role, env, ok := h.envWithRole(id, userID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}
	if role != models.EnvRoleViewer && role != models.EnvRoleEditor && role != models.EnvRoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "No access"})
		return
	}

	vars := decodeVars(env.Variables)
	for _, v := range vars {
		if v.Key == key {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"key": v.Key, "value": v.Value, "secret": v.Secret}})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "Variable not found"})
}

// ----------------------------------------------------------------------
// Export — multi-env, multi-format
// ----------------------------------------------------------------------

type exportInput struct {
	IDs            []string `json:"ids"`
	WorkspaceID    *string  `json:"workspaceId"`
	Format         string   `json:"format"`         // "patchman" | "postman" | "dotenv"
	IncludeSecrets bool     `json:"includeSecrets"`
	EnabledOnly    bool     `json:"enabledOnly"`
}

// BulkExport packages one or more environments in the chosen format.
// Single env + dotenv → text/plain attachment; multi env + dotenv →
// zip with one .env per environment; otherwise → JSON.
func (h *EnvironmentHandler) BulkExport(c *gin.Context) {
	userID := c.GetString("userID")

	var input exportInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	format := strings.ToLower(strings.TrimSpace(input.Format))
	if format == "" {
		format = "patchman"
	}
	if format != "patchman" && format != "postman" && format != "dotenv" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Format must be patchman, postman, or dotenv"})
		return
	}

	envs := h.collectExportSet(userID, input)
	if len(envs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No environments matched"})
		return
	}

	switch format {
	case "dotenv":
		if len(envs) == 1 {
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.env"`, sanitizeFilename(envs[0].Name)))
			c.String(http.StatusOK, renderDotEnv(envs[0], input))
			return
		}
		// multi env → zip
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, env := range envs {
			fw, _ := zw.Create(sanitizeFilename(env.Name) + ".env")
			fw.Write([]byte(renderDotEnv(env, input)))
		}
		zw.Close()
		c.Header("Content-Type", "application/zip")
		c.Header("Content-Disposition", `attachment; filename="patchman-environments.zip"`)
		c.Data(http.StatusOK, "application/zip", buf.Bytes())
		return

	case "postman":
		out := make([]map[string]interface{}, 0, len(envs))
		for _, env := range envs {
			out = append(out, renderPostman(env, input))
		}
		if len(out) == 1 {
			c.JSON(http.StatusOK, gin.H{"data": out[0]})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return

	default: // patchman
		out := make([]map[string]interface{}, 0, len(envs))
		for _, env := range envs {
			out = append(out, renderPatchmanExport(env, input))
		}
		if len(out) == 1 {
			c.JSON(http.StatusOK, gin.H{"data": out[0]})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": map[string]interface{}{
			"_patchman_export_version": 1,
			"environments":             out,
		}})
	}
}

// Export keeps the legacy single-env GET working for any FE that
// hasn't migrated to the bulk path yet.
func (h *EnvironmentHandler) Export(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var env models.Environment
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&env).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": renderPatchmanExport(env, exportInput{IncludeSecrets: true})})
}

// ----------------------------------------------------------------------
// Import (unchanged interface, now delegates through unchanged paths)
// ----------------------------------------------------------------------

func (h *EnvironmentHandler) Import(c *gin.Context) {
	userID := c.GetString("userID")

	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	var envName string
	var variables []map[string]interface{}

	if _, hasValues := generic["values"]; hasValues {
		// Postman
		name, _ := generic["name"].(string)
		if name == "" {
			name = "Imported Environment"
		}
		envName = name
		valuesRaw, _ := json.Marshal(generic["values"])
		var postmanVars []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Type    string `json:"type"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal(valuesRaw, &postmanVars); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid Postman environment format"})
			return
		}
		for _, v := range postmanVars {
			variables = append(variables, map[string]interface{}{
				"key": v.Key, "value": v.Value, "enabled": v.Enabled, "secret": v.Type == "secret",
			})
		}
	} else if _, hasVars := generic["variables"]; hasVars {
		name, _ := generic["name"].(string)
		if name == "" {
			name = "Imported Environment"
		}
		envName = name
		varsRaw, _ := json.Marshal(generic["variables"])
		var patchmanVars []map[string]interface{}
		if err := json.Unmarshal(varsRaw, &patchmanVars); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid environment variables format"})
			return
		}
		variables = patchmanVars
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unrecognized environment format. Expected PatchMan or Postman environment JSON."})
		return
	}

	varsJSON, _ := json.Marshal(variables)
	env := models.Environment{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      envName,
		Variables: string(varsJSON),
	}
	if err := h.db.Create(&env).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to import environment"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": env})
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// envWithRole returns the env + the requesting user's role (owner /
// editor / viewer) or ok=false if they have no access at all.
//
// Delegates to ResolveEnvironmentRole so every access route the
// resolver covers (owner, legacy share, public grant, workspace-team
// cascade) lights up here too. The pre-1.16 implementation only
// checked owner + EnvironmentShare which silently failed every
// public-grant and cascade recipient with "Environment not found".
func (h *EnvironmentHandler) envWithRole(envID, userID string) (string, models.Environment, bool) {
	var env models.Environment
	if err := h.db.First(&env, "id = ?", envID).Error; err != nil {
		return "", env, false
	}
	role := ResolveEnvironmentRole(h.db, userID, envID)
	switch role {
	case OwnerRole:
		return models.EnvRoleOwner, env, true
	case EditorRole:
		return models.EnvRoleEditor, env, true
	case ViewerRole:
		return models.EnvRoleViewer, env, true
	}
	return "", env, false
}

func (h *EnvironmentHandler) setActivation(userID, workspaceID, envID string) {
	var existing models.EnvironmentActivation
	err := h.db.Where("user_id = ? AND workspace_id = ?", userID, workspaceID).First(&existing).Error
	if err == nil {
		h.db.Model(&existing).Update("environment_id", envID)
		return
	}
	h.db.Create(&models.EnvironmentActivation{
		ID:            uuid.New().String(),
		UserID:        userID,
		WorkspaceID:   workspaceID,
		EnvironmentID: envID,
	})
}

func (h *EnvironmentHandler) collectExportSet(userID string, in exportInput) []models.Environment {
	if len(in.IDs) > 0 {
		var envs []models.Environment
		// Only return ones the user owns. Sharing-aware export can be
		// added later — for now exporting someone else's env would
		// surface secrets they may not be entitled to.
		h.db.Where("id IN ? AND user_id = ?", in.IDs, userID).Find(&envs)
		return envs
	}
	q := h.db.Where("user_id = ?", userID)
	if in.WorkspaceID != nil {
		ws := *in.WorkspaceID
		if ws == "__none__" || ws == "" {
			q = q.Where("workspace_id IS NULL OR workspace_id = ?", "")
		} else {
			q = q.Where("workspace_id = ?", ws)
		}
	}
	var envs []models.Environment
	q.Order("created_at ASC").Find(&envs)
	return envs
}

func decodeVars(s string) []envVariable {
	if s == "" {
		return nil
	}
	var raw []envVariable
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	return raw
}

// maskSecretValues returns a JSON string identical to the input but
// with every variable where secret==true having its value replaced by
// "***". Used on the wire for Viewer responses; reveal endpoint
// returns the raw value when explicitly asked.
func maskSecretValues(s string) string {
	vars := decodeVars(s)
	if vars == nil {
		return s
	}
	for i := range vars {
		if vars[i].Secret {
			vars[i].Value = "***"
		}
	}
	out, err := json.Marshal(vars)
	if err != nil {
		return s
	}
	return string(out)
}

func renderPatchmanExport(env models.Environment, in exportInput) map[string]interface{} {
	vars := filteredVars(env, in)
	flat := make([]map[string]interface{}, 0, len(vars))
	for _, v := range vars {
		flat = append(flat, map[string]interface{}{
			"key": v.Key, "value": v.Value, "enabled": v.Enabled, "secret": v.Secret,
		})
	}
	return map[string]interface{}{
		"name":                  env.Name,
		"variables":             flat,
		"_patchman_exported_at": env.UpdatedAt,
	}
}

func renderPostman(env models.Environment, in exportInput) map[string]interface{} {
	vars := filteredVars(env, in)
	values := make([]map[string]interface{}, 0, len(vars))
	for _, v := range vars {
		t := "default"
		if v.Secret {
			t = "secret"
		}
		values = append(values, map[string]interface{}{
			"key": v.Key, "value": v.Value, "enabled": v.Enabled, "type": t,
		})
	}
	return map[string]interface{}{
		"id":                       env.ID,
		"name":                     env.Name,
		"values":                   values,
		"_postman_variable_scope":  "environment",
		"_postman_exported_at":     env.UpdatedAt,
		"_postman_exported_using":  "PatchMan",
	}
}

func renderDotEnv(env models.Environment, in exportInput) string {
	vars := filteredVars(env, in)
	// Stable alphabetical order so re-exports diff cleanly in source
	// control.
	sort.Slice(vars, func(i, j int) bool { return vars[i].Key < vars[j].Key })
	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(env.Name)
	sb.WriteString("\n# Exported from PatchMan\n\n")
	for _, v := range vars {
		if v.Key == "" {
			continue
		}
		sb.WriteString(v.Key)
		sb.WriteString("=")
		sb.WriteString(escapeDotEnv(v.Value))
		sb.WriteString("\n")
	}
	return sb.String()
}

func filteredVars(env models.Environment, in exportInput) []envVariable {
	vars := decodeVars(env.Variables)
	out := make([]envVariable, 0, len(vars))
	for _, v := range vars {
		if in.EnabledOnly && !v.Enabled {
			continue
		}
		if v.Secret && !in.IncludeSecrets {
			out = append(out, envVariable{Key: v.Key, Value: "***", Enabled: v.Enabled, Secret: true})
			continue
		}
		out = append(out, v)
	}
	return out
}

// escapeDotEnv quotes values that contain whitespace or special chars
// so a downstream `dotenv` parser reads them back identical to the
// original. Empty values stay empty (no quotes).
func escapeDotEnv(v string) string {
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, " \t\n\"'$#") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
	}
	return v
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "environment"
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return strings.TrimSpace(string(out))
}

func randomShareToken() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b) // 8 hex chars, mirrors collection share
}
