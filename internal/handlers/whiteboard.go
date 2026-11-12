package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// WhiteboardHandler is the CRUD surface for Excalidraw whiteboards. Owner-scoped
// on every query (user_id = ?), no secrets, no cipher — the scene is opaque JSON
// stored verbatim. Sharing/grants are a later phase; for now a whiteboard is
// visible only to its owner. Mirrors the ES connection handler shape minus the
// cipher/registry/DTO layer.
type WhiteboardHandler struct {
	db *gorm.DB
}

func NewWhiteboardHandler(db *gorm.DB) *WhiteboardHandler {
	return &WhiteboardHandler{db: db}
}

type createWhiteboardInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name" binding:"required"`
	Scene       string  `json:"scene"`
	WorkspaceID string  `json:"workspaceId"`
	ParentID    *string `json:"parentId"`
	Type        string  `json:"type"` // "whiteboard" (default) or "folder"
	SortOrder   *int    `json:"sortOrder"`
}

type updateWhiteboardInput struct {
	Name  *string `json:"name"`
	Scene *string `json:"scene"` // nil = keep existing
	// ParentID reparents (drag into a folder / to root). A non-nil pointer to an
	// empty string means "move to root" (parent_id = NULL); a pointer to an id
	// means nest under that folder. nil = leave parent unchanged.
	ParentID  *string `json:"parentId"`
	SortOrder *int    `json:"sortOrder"`
}

// List returns the owner's whiteboards WITHOUT the scene body — the sidebar file
// list only needs metadata, and scenes can be large. The FE builds the tree from
// parentId; ordering is sort_order then created_at (drag-drop persists it). The
// full scene comes from Get when a board is opened.
func (h *WhiteboardHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var rows []models.Whiteboard
	if err := h.db.
		Select("id", "user_id", "workspace_id", "parent_id", "type", "sort_order", "name", "visibility", "created_at", "updated_at").
		Where("user_id = ?", userID).
		Order("sort_order ASC, created_at ASC").
		Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list whiteboards"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// ListShared returns whiteboards/folders shared WITH the current user via a
// direct user or team grant (not "everyone" — those are link-discoverable, not
// auto-listed). A shared folder is expanded to its whole subtree so the FE can
// render it as a tree; every node carries the share-root's role, and share-root
// nodes get parentId=null so they sit at the top of the "Shared with me" view.
func (h *WhiteboardHandler) ListShared(c *gin.Context) {
	userID := c.GetString("userID")
	out := []gin.H{}
	if userID == "" {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	type grantRow struct {
		WhiteboardID string
		Role         string
	}
	var grants []grantRow
	h.db.Table("whiteboard_public_grants g").
		Select("g.whiteboard_id as whiteboard_id, g.role as role").
		Joins("JOIN whiteboards w ON w.id = g.whiteboard_id").
		Where("w.visibility = 'public' AND w.user_id <> ?", userID).
		Where("(g.subject_kind = 'user' AND g.subject_id = ?)"+
			" OR (g.subject_kind = 'team' AND g.subject_id IN (?)"+
			" AND EXISTS (SELECT 1 FROM teams t WHERE t.id = g.subject_id AND t.deleted_at IS NULL))",
			userID, h.db.Table("team_members").Select("team_id").Where("user_id = ?", userID)).
		Order("w.created_at ASC").
		Scan(&grants)

	// Preserve a deterministic root order (created_at from the query above); a Go
	// map range would reshuffle "Shared with me" on every refresh.
	roleByRoot := map[string]Role{}
	orderedRoots := make([]string, 0, len(grants))
	for _, g := range grants {
		r := roleFromShareString(g.Role)
		if _, seenRoot := roleByRoot[g.WhiteboardID]; !seenRoot {
			orderedRoots = append(orderedRoots, g.WhiteboardID)
		}
		if roleRank[r] > roleRank[roleByRoot[g.WhiteboardID]] {
			roleByRoot[g.WhiteboardID] = r
		}
	}

	seen := map[string]bool{}
	var addSubtree func(id string, role Role, isRoot bool)
	addSubtree = func(id string, role Role, isRoot bool) {
		if seen[id] {
			return
		}
		var n models.Whiteboard
		if err := h.db.
			Select("id", "user_id", "workspace_id", "parent_id", "type", "sort_order", "name", "visibility", "created_at", "updated_at").
			Where("id = ?", id).First(&n).Error; err != nil {
			return
		}
		seen[id] = true
		var parentID any
		if !isRoot && n.ParentID != nil {
			parentID = *n.ParentID
		}
		out = append(out, gin.H{
			"id": n.ID, "name": n.Name, "scene": "", "workspaceId": n.WorkspaceID,
			"parentId": parentID, "type": n.Type, "sortOrder": n.SortOrder,
			"visibility": n.Visibility, "createdAt": n.CreatedAt, "updatedAt": n.UpdatedAt,
			"role": string(role), "shared": true, "ownerId": n.UserID,
		})
		var kids []models.Whiteboard
		h.db.Select("id").Where("parent_id = ?", id).Find(&kids)
		for _, k := range kids {
			addSubtree(k.ID, role, false)
		}
	}
	for _, rootID := range orderedRoots {
		addSubtree(rootID, roleByRoot[rootID], true)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *WhiteboardHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	// Owner OR anyone the share resolver grants at least viewer (a grant on this
	// node or any ancestor folder).
	if !canAccessWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Whiteboard not found"})
		return
	}
	var row models.Whiteboard
	if err := h.db.Where("id = ?", id).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Whiteboard not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": row})
}

func (h *WhiteboardHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createWhiteboardInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	scene := strings.TrimSpace(input.Scene)
	if scene == "" {
		scene = "{}"
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	typ := input.Type
	if typ != "folder" {
		typ = "whiteboard"
	}
	// Validate a parent folder belongs to this user and is actually a folder.
	parentID := h.validParent(userID, input.ParentID)
	sortOrder := 0
	if input.SortOrder != nil {
		sortOrder = *input.SortOrder
	} else {
		// No explicit order (e.g. an MCP-created board) → append after the
		// parent's current children instead of defaulting to 0, which would
		// sort it before every FE-created sibling.
		var maxOrder *int
		q := h.db.Model(&models.Whiteboard{}).Where("user_id = ?", userID)
		if parentID == nil {
			q = q.Where("parent_id IS NULL")
		} else {
			q = q.Where("parent_id = ?", *parentID)
		}
		q.Select("MAX(sort_order)").Scan(&maxOrder)
		if maxOrder != nil {
			sortOrder = *maxOrder + 1
		}
	}
	row := models.Whiteboard{
		ID:          id,
		UserID:      userID,
		WorkspaceID: input.WorkspaceID,
		ParentID:    parentID,
		Type:        typ,
		SortOrder:   sortOrder,
		Name:        input.Name,
		SceneJSON:   scene,
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save whiteboard"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": row})
}

// validParent returns a sanitised parent pointer: nil (root) when the input is
// nil or empty, or the id only if it names one of this user's folders. Anything
// else (missing, not-a-folder, someone else's) falls back to root so a bad
// reparent can never smuggle a board under a foreign node.
func (h *WhiteboardHandler) validParent(userID string, in *string) *string {
	if in == nil {
		return nil
	}
	pid := strings.TrimSpace(*in)
	if pid == "" {
		return nil
	}
	var parent models.Whiteboard
	if err := h.db.Select("id", "type").Where("id = ? AND user_id = ?", pid, userID).First(&parent).Error; err != nil {
		return nil
	}
	if parent.Type != "folder" {
		return nil
	}
	return &pid
}

// isDescendantOf reports whether ancestorID lies within nodeID's subtree — i.e.
// walking parent_id upward from ancestorID reaches nodeID (capped). The reparent
// guard uses it to reject moving a folder INTO its own descendant (a cycle).
func (h *WhiteboardHandler) isDescendantOf(userID, nodeID, ancestorID string) bool {
	cur := ancestorID
	for i := 0; i < 64 && cur != ""; i++ {
		if cur == nodeID {
			return true
		}
		var row models.Whiteboard
		if err := h.db.Select("parent_id").Where("id = ? AND user_id = ?", cur, userID).First(&row).Error; err != nil {
			return false
		}
		if row.ParentID == nil {
			return false
		}
		cur = *row.ParentID
	}
	return false
}

func (h *WhiteboardHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var input updateWhiteboardInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var row models.Whiteboard
	if err := h.db.Where("id = ?", id).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Whiteboard not found"})
		return
	}
	owner := isWhiteboardOwner(h.db, userID, id)
	// Content edits (name / scene) need editor; a shared editor collaborator
	// qualifies. Structural moves (parent / sort) reorganise the OWNER's tree,
	// so they stay owner-only.
	if !owner && !canEditWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	if !owner && (input.ParentID != nil || input.SortOrder != nil) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can move this board"})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Scene != nil {
		updates["scene_json"] = *input.Scene
	}
	if input.SortOrder != nil {
		updates["sort_order"] = *input.SortOrder
	}
	if input.ParentID != nil {
		newParent := strings.TrimSpace(*input.ParentID)
		if newParent == "" {
			updates["parent_id"] = nil // move to root
		} else if newParent != id && !h.isDescendantOf(userID, id, newParent) {
			// Only nest under one of this user's folders, and never under self
			// or a descendant (cycle).
			if p := h.validParent(userID, &newParent); p != nil {
				updates["parent_id"] = *p
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent folder"})
				return
			}
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent folder"})
			return
		}
	}
	if len(updates) > 0 {
		if err := h.db.Model(&row).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update whiteboard"})
			return
		}
	}
	// Re-read so the response carries fresh timestamps + persisted scene.
	h.db.Where("id = ?", id).First(&row)
	c.JSON(http.StatusOK, gin.H{"data": row})
}

func (h *WhiteboardHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	// Ensure the target exists and is owned before recursing.
	var row models.Whiteboard
	if err := h.db.Select("id").Where("id = ? AND user_id = ?", id, userID).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Whiteboard not found"})
		return
	}
	if err := h.deleteSubtree(userID, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete whiteboard"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

type enqueueDiagramInput struct {
	Mermaid string `json:"mermaid" binding:"required"`
	Title   string `json:"title"`
}

// EnqueueDiagram queues a Mermaid diagram for the board (the MCP draw tool calls
// this in-process as the user). It doesn't touch the scene — the FE drains the
// queue and renders it, since Mermaid→Excalidraw is a browser-only conversion.
// Requires editor access (queuing a draw is a write).
func (h *WhiteboardHandler) EnqueueDiagram(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isWhiteboardOwner(h.db, userID, id) && !canEditWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	var in enqueueDiagramInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row := models.WhiteboardDiagram{ID: uuid.New().String(), WhiteboardID: id, Mermaid: in.Mermaid, Title: in.Title}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "queue diagram"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": row.ID, "queued": true}})
}

// DrainDiagrams atomically returns AND removes the board's queued diagrams, so
// exactly one open client renders each (no double-insert across viewers). The
// FE converts each Mermaid onto the scene, which then autosaves. Editor-gated.
func (h *WhiteboardHandler) DrainDiagrams(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !isWhiteboardOwner(h.db, userID, id) && !canEditWhiteboard(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not allowed"})
		return
	}
	out := []gin.H{}
	err := h.db.Transaction(func(tx *gorm.DB) error {
		var rows []models.WhiteboardDiagram
		if err := tx.Where("whiteboard_id = ?", id).Order("created_at ASC").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, gin.H{"mermaid": r.Mermaid, "title": r.Title})
			ids = append(ids, r.ID)
		}
		return tx.Where("id IN ?", ids).Delete(&models.WhiteboardDiagram{}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "drain diagrams"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// deleteSubtree removes a node and, if it's a folder, all descendants — so
// deleting a folder never orphans the boards inside it. Depth-first, owner-scoped
// on every query. Mirrors collection.deleteChildren.
func (h *WhiteboardHandler) deleteSubtree(userID, id string) error {
	var children []models.Whiteboard
	if err := h.db.Select("id").Where("parent_id = ? AND user_id = ?", id, userID).Find(&children).Error; err != nil {
		return err
	}
	for _, ch := range children {
		if err := h.deleteSubtree(userID, ch.ID); err != nil {
			return err
		}
	}
	// Clean up the node's sharing rows + queued diagrams so nothing is orphaned.
	h.db.Where("whiteboard_id = ?", id).Delete(&models.WhiteboardPublicGrant{})
	h.db.Where("whiteboard_id = ?", id).Delete(&models.WhiteboardPublicLink{})
	h.db.Where("whiteboard_id = ?", id).Delete(&models.WhiteboardDiagram{})
	return h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.Whiteboard{}).Error
}
