package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// WhiteboardSharingHandler carries the share surface for whiteboards: public
// visibility + user/team/everyone grants (mirror of CollectionSharingHandler)
// PLUS no-account public links (viewer/editor). Grants gate signed-in users;
// links gate anonymous viewers by an opaque token whose sha256 hash is stored.
type WhiteboardSharingHandler struct {
	db *gorm.DB
}

func NewWhiteboardSharingHandler(db *gorm.DB) *WhiteboardSharingHandler {
	return &WhiteboardSharingHandler{db: db}
}

func wbHashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ── Visibility ──────────────────────────────────────────────────────────────

type wbVisibilityInput struct {
	Visibility string `json:"visibility" binding:"required"` // 'public' | 'private'
}

func (h *WhiteboardSharingHandler) SetVisibility(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isWhiteboardOwner(h.db, userID, id) {
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
	tx := h.db.Begin()
	if err := tx.Model(&models.Whiteboard{}).Where("id = ?", id).Update("visibility", in.Visibility).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update visibility"})
		return
	}
	// Going private revokes every grant + link — the board is owner-only again.
	if in.Visibility == "private" {
		tx.Where("whiteboard_id = ?", id).Delete(&models.WhiteboardPublicGrant{})
		tx.Where("whiteboard_id = ?", id).Delete(&models.WhiteboardPublicLink{})
	}
	tx.Commit()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"visibility": in.Visibility}})
}

// ── Grants ──────────────────────────────────────────────────────────────────

type wbGrantDTO struct {
	ID          string  `json:"id"`
	Role        string  `json:"role"`
	SubjectKind string  `json:"subjectKind"`
	SubjectID   *string `json:"subjectId,omitempty"`
	Label       string  `json:"label"`
}

func (h *WhiteboardSharingHandler) ListGrants(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canAccessWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	var grants []models.WhiteboardPublicGrant
	h.db.Where("whiteboard_id = ?", id).Order("subject_kind, created_at").Find(&grants)
	out := make([]wbGrantDTO, 0, len(grants))
	for _, g := range grants {
		out = append(out, wbGrantDTO{ID: g.ID, Role: g.Role, SubjectKind: g.SubjectKind, SubjectID: g.SubjectID, Label: h.subjectLabel(g.SubjectKind, g.SubjectID)})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *WhiteboardSharingHandler) subjectLabel(kind string, subjectID *string) string {
	switch kind {
	case "everyone":
		return "Everyone"
	case "user":
		if subjectID != nil {
			var u struct{ Email, Name string }
			if h.db.Table("users").Select("email, name").Where("id = ?", *subjectID).Scan(&u).Error == nil {
				if u.Name != "" {
					return u.Name
				}
				return u.Email
			}
		}
	case "team":
		if subjectID != nil {
			var name string
			if h.db.Table("teams").Select("name").Where("id = ?", *subjectID).Scan(&name).Error == nil && name != "" {
				return name
			}
		}
	}
	return ""
}

type wbAddGrantInput struct {
	Role        string  `json:"role" binding:"required"`        // 'editor' | 'viewer'
	SubjectKind string  `json:"subjectKind" binding:"required"` // 'user' | 'team' | 'everyone'
	SubjectID   *string `json:"subjectId"`
}

func (h *WhiteboardSharingHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canEditWhiteboard(h.db, userID, id) {
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
	// Board must be public for a grant to take effect.
	var vis string
	h.db.Table("whiteboards").Select("visibility").Where("id = ?", id).Scan(&vis)
	if vis != "public" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Make the board shared (public) before adding grants"})
		return
	}

	var everyoneCount int64
	h.db.Model(&models.WhiteboardPublicGrant{}).
		Where("whiteboard_id = ? AND role = ? AND subject_kind = 'everyone'", id, in.Role).Count(&everyoneCount)

	tx := h.db.Begin()
	if in.SubjectKind == "everyone" {
		tx.Where("whiteboard_id = ? AND role = ? AND subject_kind <> 'everyone'", id, in.Role).Delete(&models.WhiteboardPublicGrant{})
		if everyoneCount > 0 {
			tx.Commit()
			var existing models.WhiteboardPublicGrant
			h.db.Where("whiteboard_id = ? AND role = ? AND subject_kind = 'everyone'", id, in.Role).First(&existing)
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	} else {
		if everyoneCount > 0 {
			tx.Rollback()
			c.JSON(http.StatusConflict, gin.H{"error": "Remove 'everyone' before adding a specific subject"})
			return
		}
		var existing models.WhiteboardPublicGrant
		if tx.Where("whiteboard_id = ? AND role = ? AND subject_kind = ? AND subject_id = ?", id, in.Role, in.SubjectKind, *in.SubjectID).First(&existing).Error == nil {
			tx.Commit()
			c.JSON(http.StatusOK, gin.H{"data": existing})
			return
		}
	}
	grant := models.WhiteboardPublicGrant{ID: uuid.New().String(), WhiteboardID: id, Role: in.Role, SubjectKind: in.SubjectKind, SubjectID: in.SubjectID}
	if err := tx.Create(&grant).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "add grant"})
		return
	}
	tx.Commit()
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *WhiteboardSharingHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	grantID := c.Param("grantId")
	if !canEditWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	h.db.Where("id = ? AND whiteboard_id = ?", grantID, id).Delete(&models.WhiteboardPublicGrant{})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "removed"}})
}

// ── Public no-account links ───────────────────────────────────────────────────

type wbLinkInput struct {
	Role string `json:"role"` // 'viewer' (default) | 'editor'
}

type wbLinkDTO struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Token      string     `json:"token,omitempty"` // returned ONCE on create
}

func (h *WhiteboardSharingHandler) CreateLink(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isWhiteboardOwner(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can create links"})
		return
	}
	var in wbLinkInput
	_ = c.ShouldBindJSON(&in)
	role := in.Role
	if role != "editor" {
		role = "viewer"
	}
	// A public link only functions while the board is shared (public).
	var vis string
	h.db.Table("whiteboards").Select("visibility").Where("id = ?", id).Scan(&vis)
	if vis != "public" {
		h.db.Model(&models.Whiteboard{}).Where("id = ?", id).Update("visibility", "public")
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token"})
		return
	}
	raw := "wbl_" + hex.EncodeToString(buf)
	link := models.WhiteboardPublicLink{
		ID: uuid.New().String(), WhiteboardID: id, UserID: userID,
		TokenHash: wbHashToken(raw), Token: raw, Prefix: raw[:12], Role: role,
	}
	if err := h.db.Create(&link).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create link"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": wbLinkDTO{ID: link.ID, Role: link.Role, Prefix: link.Prefix, CreatedAt: link.CreatedAt, Token: raw}})
}

func (h *WhiteboardSharingHandler) ListLinks(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isWhiteboardOwner(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	var links []models.WhiteboardPublicLink
	h.db.Where("whiteboard_id = ?", id).Order("created_at DESC").Find(&links)
	out := make([]wbLinkDTO, 0, len(links))
	for _, l := range links {
		// Return the plaintext token so the owner can re-copy the link URL (this
		// endpoint is owner-only).
		out = append(out, wbLinkDTO{ID: l.ID, Role: l.Role, Prefix: l.Prefix, CreatedAt: l.CreatedAt, LastUsedAt: l.LastUsedAt, Token: l.Token})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *WhiteboardSharingHandler) RevokeLink(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	linkID := c.Param("linkId")
	if !isWhiteboardOwner(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	h.db.Where("id = ? AND whiteboard_id = ?", linkID, id).Delete(&models.WhiteboardPublicLink{})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "revoked"}})
}

// ── Anonymous access by link token (mounted WITHOUT auth) ─────────────────────

func (h *WhiteboardSharingHandler) resolveLink(token string) (*models.WhiteboardPublicLink, *models.Whiteboard) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil
	}
	var link models.WhiteboardPublicLink
	if err := h.db.Where("token_hash = ?", wbHashToken(token)).First(&link).Error; err != nil {
		return nil, nil
	}
	var wb models.Whiteboard
	if err := h.db.Where("id = ?", link.WhiteboardID).First(&wb).Error; err != nil {
		return nil, nil
	}
	// A link is only live while the board stays shared.
	if wb.Visibility != "public" {
		return nil, nil
	}
	return &link, &wb
}

// linkCovers reports whether a link on `root` reaches board `boardID`: the
// root board itself, or — for a folder link — any board nested under it, at
// any depth. Sharing cascades from a folder (see ResolveWhiteboardRole), so a
// descendant's own visibility doesn't matter. Returns the board row.
func (h *WhiteboardSharingHandler) linkCovers(root *models.Whiteboard, boardID string) (*models.Whiteboard, bool) {
	if boardID == root.ID {
		return root, root.Type != "folder"
	}
	if root.Type != "folder" || boardID == "" {
		return nil, false
	}
	var wb models.Whiteboard
	if err := h.db.Where("id = ?", boardID).First(&wb).Error; err != nil || wb.Type == "folder" {
		return nil, false
	}
	cur := wb.ParentID
	for i := 0; i < 64 && cur != nil; i++ {
		if *cur == root.ID {
			return &wb, true
		}
		var parent *string
		if err := h.db.Table("whiteboards").Select("parent_id").Where("id = ?", *cur).Scan(&parent).Error; err != nil {
			return nil, false
		}
		cur = parent
	}
	return nil, false
}

// wbPublicItem is one node of a folder link's tree — structure only, no scene.
type wbPublicItem struct {
	ID        string    `json:"id"`
	ParentID  *string   `json:"parentId"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	SortOrder int       `json:"sortOrder"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// folderItems lists every node under a folder, breadth-first (capped).
func (h *WhiteboardSharingHandler) folderItems(folderID string) []wbPublicItem {
	out := []wbPublicItem{}
	frontier := []string{folderID}
	for depth := 0; depth < 64 && len(frontier) > 0 && len(out) < 5000; depth++ {
		var level []wbPublicItem
		h.db.Table("whiteboards").
			Select("id, parent_id, name, type, sort_order, updated_at").
			Where("parent_id IN ?", frontier).
			Order("sort_order, name").
			Scan(&level)
		frontier = frontier[:0]
		for _, it := range level {
			out = append(out, it)
			if it.Type == "folder" {
				frontier = append(frontier, it.ID)
			}
		}
	}
	return out
}

func (h *WhiteboardSharingHandler) touchLink(id string) {
	h.db.Model(&models.WhiteboardPublicLink{}).Where("id = ?", id).Update("last_used_at", time.Now())
}

func publicBoardJSON(wb *models.Whiteboard, role string) gin.H {
	return gin.H{
		"id": wb.ID, "name": wb.Name, "type": "whiteboard", "scene": wb.SceneJSON, "role": role,
		"updatedAt": wb.UpdatedAt,
	}
}

// PublicGet resolves a token for a viewer with no account. A board link
// returns the scene + the link's role; a folder link returns the folder's
// tree (`items`, no scenes) — each board is then opened through
// PublicGetBoard. Bumps last_used_at.
func (h *WhiteboardSharingHandler) PublicGet(c *gin.Context) {
	link, wb := h.resolveLink(c.Param("token"))
	if link == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Link not found or revoked"})
		return
	}
	h.touchLink(link.ID)
	if wb.Type == "folder" {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"id": wb.ID, "name": wb.Name, "type": "folder", "role": link.Role,
			"items": h.folderItems(wb.ID),
		}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": publicBoardJSON(wb, link.Role)})
}

// PublicGetBoard opens one board under a folder link.
func (h *WhiteboardSharingHandler) PublicGetBoard(c *gin.Context) {
	link, root := h.resolveLink(c.Param("token"))
	if link == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Link not found or revoked"})
		return
	}
	wb, ok := h.linkCovers(root, c.Param("boardId"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Board not found in this link"})
		return
	}
	h.touchLink(link.ID)
	c.JSON(http.StatusOK, gin.H{"data": publicBoardJSON(wb, link.Role)})
}

type wbPublicUpdateInput struct {
	Scene *string `json:"scene"`
}

// PublicUpdate lets an anonymous holder of an EDITOR link save the scene of
// the linked board; PublicUpdateBoard does the same for a board under a
// folder link. This is the anonymous read-write surface — gated solely by
// the token's role.
func (h *WhiteboardSharingHandler) PublicUpdate(c *gin.Context) {
	h.publicSave(c, "")
}

func (h *WhiteboardSharingHandler) PublicUpdateBoard(c *gin.Context) {
	h.publicSave(c, c.Param("boardId"))
}

func (h *WhiteboardSharingHandler) publicSave(c *gin.Context, boardID string) {
	link, root := h.resolveLink(c.Param("token"))
	if link == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Link not found or revoked"})
		return
	}
	if boardID == "" {
		boardID = root.ID
	}
	wb, ok := h.linkCovers(root, boardID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Board not found in this link"})
		return
	}
	if link.Role != "editor" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This link is read-only"})
		return
	}
	// Bound the UNAUTHENTICATED write — a scene with embedded images is large but
	// not unbounded; 25MB is generous and keeps a token holder from streaming an
	// arbitrarily huge body into a text column.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 25<<20)
	var in wbPublicUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Scene != nil {
		if err := h.db.Model(&models.Whiteboard{}).Where("id = ?", wb.ID).Update("scene_json", *in.Scene).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save"})
			return
		}
	}
	h.touchLink(link.ID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "saved"}})
}
