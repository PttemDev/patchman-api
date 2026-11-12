package handlers

import (
	"net/http"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// NoteSharingHandler mirrors WhiteboardSharingHandler for notes: visibility
// plus user / team / everyone grants on any node (cascading to its subtree).
// The FE drives it through the shared PublicSharingPanel (kind 'note').
type NoteSharingHandler struct {
	db *gorm.DB
}

func NewNoteSharingHandler(db *gorm.DB) *NoteSharingHandler { return &NoteSharingHandler{db: db} }

// notifyShareChange tells everyone who could see the note before AND after a
// sharing change, so a revoked user's sidebar drops the space too.
func (h *NoteSharingHandler) notifyShareChange(c *gin.Context, id string, before []string, beforeAll bool) {
	after, afterAll := realtime.NoteRecipients(h.db, id)
	set := map[string]struct{}{}
	for _, u := range append(before, after...) {
		set[u] = struct{}{}
	}
	all := make([]string, 0, len(set))
	for u := range set {
		all = append(all, u)
	}
	if beforeAll || afterAll {
		all = nil // a broadcast reaches everyone anyway
	}
	notifyRT(h.db, c, realtime.Event{Type: realtime.EventNoteShared, NoteID: id, Recipients: all, Everyone: beforeAll || afterAll})
}

func (h *NoteSharingHandler) SetVisibility(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isNoteOwner(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can change sharing"})
		return
	}
	var in wbVisibilityInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Visibility != "public" && in.Visibility != "private" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "visibility must be 'public' or 'private'"})
		return
	}
	before, beforeAll := realtime.NoteRecipients(h.db, id)
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Note{}).Where("id = ?", id).Update("visibility", in.Visibility).Error; err != nil {
			return err
		}
		if in.Visibility == "private" {
			return tx.Where("note_id = ?", id).Delete(&models.NotePublicGrant{}).Error
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update visibility"})
		return
	}
	h.notifyShareChange(c, id, before, beforeAll)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": in.Visibility}})
}

func (h *NoteSharingHandler) ListGrants(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canReadNote(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	var grants []models.NotePublicGrant
	h.db.Where("note_id = ?", id).Order("subject_kind, created_at").Find(&grants)
	labels := (&WhiteboardSharingHandler{db: h.db})
	out := make([]wbGrantDTO, 0, len(grants))
	for _, g := range grants {
		out = append(out, wbGrantDTO{ID: g.ID, Role: g.Role, SubjectKind: g.SubjectKind, SubjectID: g.SubjectID, Label: labels.subjectLabel(g.SubjectKind, g.SubjectID)})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// AddGrant: editors may share onward (like whiteboards); the node must be
// public. An "everyone" grant replaces specific grants of that role.
func (h *NoteSharingHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canEditNote(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	var in wbAddGrantInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Role != "editor" && in.Role != "viewer" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'editor' or 'viewer'"})
		return
	}
	if in.SubjectKind != "user" && in.SubjectKind != "team" && in.SubjectKind != "everyone" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid subjectKind"})
		return
	}
	if in.SubjectKind != "everyone" && (in.SubjectID == nil || *in.SubjectID == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectId is required for user/team grants"})
		return
	}
	var vis string
	h.db.Table("notes").Select("visibility").Where("id = ? AND deleted_at IS NULL", id).Scan(&vis)
	if vis != "public" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Make the note shared (public) before adding grants"})
		return
	}
	before, beforeAll := realtime.NoteRecipients(h.db, id)
	var everyoneCount int64
	h.db.Model(&models.NotePublicGrant{}).Where("note_id = ? AND role = ? AND subject_kind = 'everyone'", id, in.Role).Count(&everyoneCount)
	var result models.NotePublicGrant
	status := http.StatusOK
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if in.SubjectKind == "everyone" {
			if err := tx.Where("note_id = ? AND role = ? AND subject_kind <> 'everyone'", id, in.Role).Delete(&models.NotePublicGrant{}).Error; err != nil {
				return err
			}
			if everyoneCount > 0 {
				return tx.Where("note_id = ? AND role = ? AND subject_kind = 'everyone'", id, in.Role).First(&result).Error
			}
		} else {
			if everyoneCount > 0 {
				status = http.StatusConflict
				return nil
			}
			if tx.Where("note_id = ? AND role = ? AND subject_kind = ? AND subject_id = ?", id, in.Role, in.SubjectKind, *in.SubjectID).First(&result).Error == nil {
				return nil
			}
		}
		result = models.NotePublicGrant{ID: uuid.New().String(), NoteID: id, Role: in.Role, SubjectKind: in.SubjectKind, SubjectID: in.SubjectID}
		status = http.StatusCreated
		return tx.Create(&result).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "add grant"})
		return
	}
	if status == http.StatusConflict {
		c.JSON(http.StatusConflict, gin.H{"error": "Remove 'everyone' before adding a specific subject"})
		return
	}
	h.notifyShareChange(c, id, before, beforeAll)
	c.JSON(status, gin.H{"data": result})
}

func (h *NoteSharingHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canEditNote(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	before, beforeAll := realtime.NoteRecipients(h.db, id)
	h.db.Where("id = ? AND note_id = ?", c.Param("grantId"), id).Delete(&models.NotePublicGrant{})
	h.notifyShareChange(c, id, before, beforeAll)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "removed"}})
}
