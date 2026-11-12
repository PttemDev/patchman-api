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

// CollectionSharingHandler covers the "make public / configure editors
// / configure viewers" surface introduced by the Teams + Public Sharing
// RFC. It complements the existing CollectionHandler (CRUD) and
// CollectionShareHandler (legacy share tokens).
type CollectionSharingHandler struct {
	db *gorm.DB
}

func NewCollectionSharingHandler(db *gorm.DB) *CollectionSharingHandler {
	return &CollectionSharingHandler{db: db}
}

// ── payloads ────────────────────────────────────────────────────────

type setVisibilityInput struct {
	Visibility string `json:"visibility" binding:"required"`
}

type addGrantInput struct {
	Role        string  `json:"role" binding:"required"`         // editor | viewer
	SubjectKind string  `json:"subjectKind" binding:"required"`  // user | team | everyone
	SubjectID   *string `json:"subjectId,omitempty"`             // required unless kind=everyone
}

// ── visibility toggle ───────────────────────────────────────────────

// SetVisibility flips a collection between 'private' and 'public'.
// Owner only (per RFC Q4 — start owner-only, design allows future
// editor extension without schema change).
//
// On private → public: the row is set to public AND default grants are
// seeded in the same transaction (editors=[owner], viewers=[everyone]).
// On public → private: all grants for the collection are deleted in the
// same transaction; legacy CollectionShare rows are left untouched.
func (h *CollectionSharingHandler) SetVisibility(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	var col models.Collection
	if err := h.db.First(&col, "id = ?", collectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	if col.UserID != userID {
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
	// No-op fast path keeps audit log clean.
	if col.Visibility == input.Visibility {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": col.Visibility}})
		return
	}

	tx := h.db.Begin()
	if err := tx.Model(&col).Update("visibility", input.Visibility).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update visibility"})
		return
	}

	if input.Visibility == "public" {
		// Mutual-exclusion guardrail: a public collection cannot also
		// run on the legacy share-code path. Wipe both the share link
		// templates (user_id IS NULL) AND the per-user collaborator
		// rows so the new Editors / Viewers panel is the only access
		// surface. The FE confirm-modal already warned the user; this
		// is the enforcement step.
		if err := tx.Where("collection_id = ?", collectionID).Delete(&models.CollectionShare{}).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear legacy share rows"})
			return
		}

		// Seed defaults: owner is editor, everyone is viewer.
		now := time.Now()
		grants := []models.CollectionPublicGrant{
			{
				ID:           uuid.New().String(),
				CollectionID: collectionID,
				Role:         "editor",
				SubjectKind:  "user",
				SubjectID:    &userID,
				CreatedAt:    now,
			},
			{
				ID:           uuid.New().String(),
				CollectionID: collectionID,
				Role:         "viewer",
				SubjectKind:  "everyone",
				SubjectID:    nil,
				CreatedAt:    now,
			},
		}
		if err := tx.Create(&grants).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to seed default grants"})
			return
		}
	} else {
		// public → private: clear all grants. Legacy CollectionShare rows
		// (individual collaborators) survive — the RFC keeps the two
		// systems parallel by design.
		if err := tx.Where("collection_id = ?", collectionID).Delete(&models.CollectionPublicGrant{}).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear grants"})
			return
		}
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit visibility change"})
		return
	}

	// A visibility flip changes who *can* see the collection (default
	// public seeds 'everyone' viewer; private→public revokes everyone).
	// Either direction means existing FE caches need to refresh — fan
	// out broadly. private→public uses Everyone=true so the new
	// discoverers also hear about it; public→private goes through the
	// computed recipients (which is now just owner + legacy shares since
	// grants are gone).
	if input.Visibility == "public" {
		notifyRT(h.db, c, realtime.Event{
			Type:         realtime.EventCollectionUpdated,
			CollectionID: collectionID,
			Everyone:     true,
		})
	} else {
		// Snapshot recipients post-revert. Add 'everyone' so previously-
		// public viewers also lose the row from their Public accordion.
		notifyCollectionMutated(h.db, c, collectionID, realtime.EventCollectionUpdated)
		notifyRT(h.db, c, realtime.Event{
			Type:         realtime.EventCollectionUpdated,
			CollectionID: collectionID,
			Everyone:     true,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": input.Visibility}})
}

// ── grants CRUD ────────────────────────────────────────────────────

// ListGrants returns both Editors and Viewers in a single payload,
// shaped for the dual-list editor on the FE. Anyone with ANY access to
// the collection can read the grant list (owner, editors, viewers,
// individual collaborators) — there's nothing sensitive in the
// "who-can-see-this-list" question itself.
func (h *CollectionSharingHandler) ListGrants(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	if !ResolveCollectionRole(h.db, userID, collectionID).AtLeast(ViewerRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "No access to this collection"})
		return
	}

	var grants []models.CollectionPublicGrant
	if err := h.db.Where("collection_id = ?", collectionID).
		Order("role DESC, created_at ASC"). // editor before viewer
		Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch grants"})
		return
	}

	// Enrich with display-friendly name for users/teams. Single round
	// trip via batched IN queries.
	userIDs := []string{}
	teamIDs := []string{}
	for _, g := range grants {
		if g.SubjectID == nil {
			continue
		}
		if g.SubjectKind == "user" {
			userIDs = append(userIDs, *g.SubjectID)
		} else if g.SubjectKind == "team" {
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
		Label       string `json:"label"` // display name
	}
	out := make([]grantDTO, 0, len(grants))
	for _, g := range grants {
		dto := grantDTO{
			ID:          g.ID,
			Role:        g.Role,
			SubjectKind: g.SubjectKind,
		}
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

// AddGrant inserts a single editor or viewer entry. Owner only. The
// "everyone" subject is a singleton per role — UI is expected to block
// mixing it with specific user/team entries in the same list, but BE
// also enforces idempotency (duplicate insert returns the existing row).
//
// Per RFC Q6: when caller is trying to add "everyone" to a list that
// already has specific users/teams, BE deletes the specifics in the
// same transaction so the resulting state matches the UX rule
// ("everyone takes over"). Conversely, adding a specific user/team to a
// list that already has "everyone" is rejected with 409 (UI is expected
// to ask the caller to drop "everyone" first).
func (h *CollectionSharingHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	var col models.Collection
	if err := h.db.First(&col, "id = ?", collectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	// Owner OR editor can manage the Editors / Viewers grant lists.
	// Visibility flips (private<->public, "Make Private") stay owner-only
	// since those touch the entire access model; granular list edits are
	// safe for editors per the share-dialog UX rebalance.
	if !canEditCollection(h.db, collectionID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to change grants"})
		return
	}
	if col.Visibility != "public" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Collection is not public; flip visibility first"})
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

	// Existing "everyone" guard.
	var everyoneCount int64
	h.db.Model(&models.CollectionPublicGrant{}).
		Where("collection_id = ? AND role = ? AND subject_kind = 'everyone'", collectionID, input.Role).
		Count(&everyoneCount)

	tx := h.db.Begin()

	if input.SubjectKind == "everyone" {
		// Adding "everyone" — wipe specifics on the same role so the
		// resulting list is just [everyone].
		if err := tx.Where("collection_id = ? AND role = ? AND subject_kind <> 'everyone'", collectionID, input.Role).
			Delete(&models.CollectionPublicGrant{}).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear specifics"})
			return
		}
		if everyoneCount > 0 {
			// Already present — no-op.
			tx.Commit()
			var existing models.CollectionPublicGrant
			h.db.Where("collection_id = ? AND role = ? AND subject_kind = 'everyone'", collectionID, input.Role).First(&existing)
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	} else {
		if everyoneCount > 0 {
			tx.Rollback()
			c.JSON(http.StatusConflict, gin.H{"error": "Cannot add specific subject while 'everyone' is in the list — remove 'everyone' first"})
			return
		}
		// Idempotent check.
		var existing models.CollectionPublicGrant
		err := tx.Where("collection_id = ? AND role = ? AND subject_kind = ? AND subject_id = ?",
			collectionID, input.Role, input.SubjectKind, *input.SubjectID).First(&existing).Error
		if err == nil {
			tx.Commit()
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	}

	grant := models.CollectionPublicGrant{
		ID:           uuid.New().String(),
		CollectionID: collectionID,
		Role:         input.Role,
		SubjectKind:  input.SubjectKind,
		SubjectID:    input.SubjectID,
		CreatedAt:    time.Now(),
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

	// Granting access changes who should be receiving events. If the
	// new grant is `everyone`, broadcast wide so any signed-in user
	// rediscovers the collection. Otherwise recompute recipients —
	// the new subject is already in that set thanks to the just-
	// committed row.
	if input.SubjectKind == "everyone" {
		notifyRT(h.db, c, realtime.Event{
			Type:         realtime.EventCollectionUpdated,
			CollectionID: collectionID,
			Everyone:     true,
		})
	} else {
		notifyCollectionMutated(h.db, c, collectionID, realtime.EventCollectionUpdated)
	}

	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

// DeleteGrant — owner only. Refuses to remove the owner's own editor
// row (would leave the collection with zero editors; the BE-enforced
// minimum is `[owner]`).
func (h *CollectionSharingHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")
	grantID := c.Param("grantId")

	var col models.Collection
	if err := h.db.First(&col, "id = ?", collectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	// Owner OR editor — same scope as AddGrant.
	if !canEditCollection(h.db, collectionID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to change grants"})
		return
	}

	var grant models.CollectionPublicGrant
	if err := h.db.Where("id = ? AND collection_id = ?", grantID, collectionID).First(&grant).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}

	// Owner-self-editor guard: BE-side mirror of the UI rule.
	if grant.Role == "editor" && grant.SubjectKind == "user" && grant.SubjectID != nil && *grant.SubjectID == col.UserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Owner cannot be removed from the Editors list"})
		return
	}

	if err := h.db.Delete(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete grant"})
		return
	}

	// Removing a grant revokes access for whoever it covered. The
	// straightforward broadcast strategy: notify current recipients
	// (so the badge updates) AND broadcast to everyone if the deleted
	// grant was an everyone or team row — those subjects can't be
	// enumerated cheaply post-delete, and the Public accordion needs
	// to drop the row for them. Wide fan-out is acceptable because
	// the FE just calls refreshLists which is cheap.
	notifyCollectionMutated(h.db, c, collectionID, realtime.EventCollectionUpdated)
	if grant.SubjectKind == "everyone" || grant.SubjectKind == "team" {
		notifyRT(h.db, c, realtime.Event{
			Type:         realtime.EventCollectionUpdated,
			CollectionID: collectionID,
			Everyone:     true,
		})
	} else if grant.SubjectKind == "user" && grant.SubjectID != nil {
		// User grant removal: include the specific user so their
		// sidebar refresh picks up the lost access.
		notifyRT(h.db, c, realtime.Event{
			Type:         realtime.EventCollectionUpdated,
			CollectionID: collectionID,
			Recipients:   []string{*grant.SubjectID},
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Grant removed"}})
}
