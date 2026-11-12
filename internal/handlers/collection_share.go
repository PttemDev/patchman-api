package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// rootCollectionID walks up the parent chain to the top-level collection
// id. Returns the input id if the chain breaks (legacy/orphan rows
// behave as their own root). Used to anchor share / grant lookups
// since CollectionShare + CollectionPublicGrant rows only ever live on
// the root, not on nested folders.
//
// Implementation note: a fresh struct is used each iteration. Reusing
// the same Collection across First() calls is unsafe when columns are
// nullable — GORM v2 does not always reset pointer fields to nil when
// the source row's column is NULL, so a stale ParentID from the prior
// iteration can persist and either infinite-loop or short-circuit the
// walk early. The bug was visible in production: a sub-folder edit
// (`PUT /api/requests/:id` whose request lived 2 levels deep) would
// resolve role on the sub-folder's own id instead of the root, so the
// public-grant editor row never matched and the writer got a 403.
// One Select-only scan per hop also dodges loading the unused
// Children/Requests slices.
func rootCollectionID(db *gorm.DB, collectionID string) string {
	current := collectionID
	// Bounded walk — collection trees are flat in practice; the cap is a
	// safety net for an accidental cycle that bypasses isDescendantOf.
	for depth := 0; depth < 64; depth++ {
		var row struct {
			ID       string
			ParentID *string
		}
		err := db.Table("collections").
			Select("id, parent_id").
			Where("id = ?", current).
			Scan(&row).Error
		if err != nil || row.ID == "" {
			return current
		}
		if row.ParentID == nil || *row.ParentID == "" {
			return row.ID
		}
		current = *row.ParentID
	}
	return current
}

// canAccessCollection returns true when userID has at least viewer
// access to the (root of the) collection. Honors owner, individual
// CollectionShare rows AND the new public-visibility grants. Use this
// for read-path guards; for write paths (create / update / delete
// requests, rename collection, etc.) call `canEditCollection` instead
// so legacy 'viewer' shares and public-viewer grants are correctly
// excluded.
func canAccessCollection(db *gorm.DB, collectionID, userID string) bool {
	return ResolveCollectionRole(db, userID, rootCollectionID(db, collectionID)).AtLeast(ViewerRole)
}

// canEditCollection returns true when userID can mutate the
// (root of the) collection — owner OR editor (via CollectionShare or
// via a public editor grant). Refuses viewer-only access.
func canEditCollection(db *gorm.DB, collectionID, userID string) bool {
	return ResolveCollectionRole(db, userID, rootCollectionID(db, collectionID)).AtLeast(EditorRole)
}

type CollectionShareHandler struct {
	db *gorm.DB
}

func NewCollectionShareHandler(db *gorm.DB) *CollectionShareHandler {
	return &CollectionShareHandler{db: db}
}

func generateToken() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b) // 8 hex chars
}

// CreateShare generates (or returns existing) share token for a collection.
func (h *CollectionShareHandler) CreateShare(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	// Only owner can create share
	var col models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", collectionID, userID).First(&col).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	// Mutual-exclusion guardrail with the public-grants surface. A
	// collection runs on EITHER the share-code path OR the public
	// Editors/Viewers path, never both — flipping to public wipes
	// share rows (see CollectionSharingHandler.SetVisibility); this
	// branch refuses to mint a share code while public. The FE hides
	// the Generate button in that state; the BE enforces it so stale
	// tabs / direct API callers can't recreate the conflict.
	if col.Visibility == "public" {
		c.JSON(http.StatusConflict, gin.H{"error": "This collection is public — manage access via Editors / Viewers instead of share codes. Make the collection private first to use share codes."})
		return
	}

	// Check if share link already exists
	var existing models.CollectionShare
	if err := h.db.Where("collection_id = ? AND user_id IS NULL", collectionID).First(&existing).Error; err == nil {
		c.JSON(http.StatusOK, gin.H{"data": existing})
		return
	}

	share := models.CollectionShare{
		ID:           uuid.New().String(),
		CollectionID: collectionID,
		ShareToken:   generateToken(),
		Role:         "editor",
	}
	if err := h.db.Create(&share).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create share"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": share})
}

// JoinByToken lets a user join a shared collection.
func (h *CollectionShareHandler) JoinByToken(c *gin.Context) {
	userID := c.GetString("userID")

	var input struct {
		ShareToken string `json:"shareToken" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Find share link
	var link models.CollectionShare
	if err := h.db.Where("share_token = ? AND user_id IS NULL", input.ShareToken).First(&link).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid share code"})
		return
	}

	// Check not already owner
	var col models.Collection
	if err := h.db.First(&col, "id = ?", link.CollectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	if col.UserID == userID {
		c.JSON(http.StatusConflict, gin.H{"error": "You already own this collection"})
		return
	}

	// Already a collaborator: treat as idempotent success rather than 409.
	// The /join/:token page can fire twice (effect double-invocation in
	// strict mode, redirect-after-login flicker, or a user re-clicking the
	// share link because the collection didn't show up in their sidebar
	// from a stale list). Returning 200 with the collection lets the FE
	// finish the success path — refresh the sidebar, redirect to /, no
	// scary "Could not join" screen.
	var existingCollab models.CollectionShare
	if err := h.db.Where("collection_id = ? AND user_id = ?", link.CollectionID, userID).First(&existingCollab).Error; err == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"collection": col, "message": "Access already granted", "alreadyJoined": true}})
		return
	}

	collab := models.CollectionShare{
		ID:           uuid.New().String(),
		CollectionID: link.CollectionID,
		ShareToken:   link.ShareToken,
		UserID:       &userID,
		Role:         link.Role,
	}
	if err := h.db.Create(&collab).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join"})
		return
	}

	// Notify the existing recipient set (now includes the new joiner)
	// so every other client viewing this collection learns there's a
	// new collaborator and refreshes the "Shared with N people" badge.
	notifyCollectionMutated(h.db, c, link.CollectionID, realtime.EventCollectionShared)

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"collection": col, "message": "Joined successfully"}})
}

// ListCollaborators returns all collaborators of a collection.
func (h *CollectionShareHandler) ListCollaborators(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	if !canAccessCollection(h.db, collectionID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	type collabResponse struct {
		models.CollectionShare
		UserName  string `json:"userName"`
		UserEmail string `json:"userEmail"`
	}

	var shares []models.CollectionShare
	h.db.Where("collection_id = ? AND user_id IS NOT NULL", collectionID).Find(&shares)

	var result []collabResponse
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
			CollectionShare: s,
			UserName:        name,
			UserEmail:       email,
		})
	}

	// Also get the share link token
	var link models.CollectionShare
	var shareToken string
	if err := h.db.Where("collection_id = ? AND user_id IS NULL", collectionID).First(&link).Error; err == nil {
		shareToken = link.ShareToken
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"collaborators": result, "shareToken": shareToken}})
}

// RemoveCollaborator removes a user from a shared collection.
func (h *CollectionShareHandler) RemoveCollaborator(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")
	targetUserID := c.Param("userId")

	// Only owner can remove
	var col models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", collectionID, userID).First(&col).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can remove collaborators"})
		return
	}

	h.db.Where("collection_id = ? AND user_id = ?", collectionID, targetUserID).Delete(&models.CollectionShare{})

	// Fan-out to the (now-shrunk) recipient set AND the removed user so
	// their sidebar loses the collection without a manual refresh.
	recipients, everyone := realtime.CollectionRecipients(h.db, collectionID)
	recipients = append(recipients, targetUserID)
	notifyRT(h.db, c, realtime.Event{
		Type:         realtime.EventCollectionUnshared,
		CollectionID: collectionID,
		Recipients:   recipients,
		Everyone:     everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Collaborator removed"}})
}

// LeaveShare lets a collaborator remove themselves from a shared collection.
func (h *CollectionShareHandler) LeaveShare(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	// Reject if caller is the owner — owners should use Delete/Revoke, not Leave.
	var col models.Collection
	if err := h.db.First(&col, "id = ?", collectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	if col.UserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Owners cannot leave their own collection"})
		return
	}

	res := h.db.Where("collection_id = ? AND user_id = ?", collectionID, userID).Delete(&models.CollectionShare{})
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "You are not a collaborator on this collection"})
		return
	}

	// Tell the leaver (so their sidebar removes the row) AND remaining
	// recipients (so the badge updates).
	recipients, everyone := realtime.CollectionRecipients(h.db, collectionID)
	recipients = append(recipients, userID)
	notifyRT(h.db, c, realtime.Event{
		Type:         realtime.EventCollectionUnshared,
		CollectionID: collectionID,
		Recipients:   recipients,
		Everyone:     everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Left shared collection"}})
}

// RevokeShare disables sharing for a collection.
func (h *CollectionShareHandler) RevokeShare(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	var col models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", collectionID, userID).First(&col).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can revoke sharing"})
		return
	}

	// Snapshot recipients BEFORE the delete so the now-revoked
	// collaborators still hear that they've lost access.
	recipients, everyone := realtime.CollectionRecipients(h.db, collectionID)

	h.db.Where("collection_id = ?", collectionID).Delete(&models.CollectionShare{})

	notifyRT(h.db, c, realtime.Event{
		Type:         realtime.EventCollectionUnshared,
		CollectionID: collectionID,
		Recipients:   recipients,
		Everyone:     everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Sharing revoked"}})
}
