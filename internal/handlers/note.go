package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notesearch"
	"patchman-be/internal/realtime"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// NoteHandler is the REST surface of the Notes knowledge base: a tree of
// markdown pages and folders, shareable at any level (see ResolveNoteRole).
// Content writes go through applyNoteContent (note_service.go).
type NoteHandler struct {
	db *gorm.DB
	// Agent memory workers (note_cards.go); nil until StartCardWorkers.
	kick chan struct{}
	uses chan []string
	// Card splitting with the person's AI provider (note_atomize.go); nil
	// until SetLLM — remember then makes one card per call without a model.
	cipher  *services.TokenCipher
	llmHTTP *http.Client
	// waiters: inbox id → closed when the worker has filed that memory, so
	// remember() can answer with the card it became.
	waiters sync.Map
	// projectMu serializes creating a project folder (two remembers for a
	// new project at once must not make two).
	projectMu sync.Mutex
}

func NewNoteHandler(db *gorm.DB) *NoteHandler { return &NoteHandler{db: db} }

// noteMeta is the sidebar shape: no body (pages can be large).
type noteMeta struct {
	ID         string  `json:"id"`
	UserID     string  `json:"userId"`
	ParentID   *string `json:"parentId"`
	Type       string  `json:"type"`
	Title      string  `json:"title"`
	Slug       string  `json:"slug"`
	Icon       string  `json:"icon,omitempty"`
	SortOrder  int     `json:"sortOrder"`
	Visibility string  `json:"visibility"`
	Version    int64   `json:"version"`
	Summary    string  `json:"summary,omitempty"`
	Source     string  `json:"source,omitempty"`
	Stale      bool    `json:"stale,omitempty"`
	Agent      string  `json:"agent,omitempty"`
	Memory     bool    `json:"memory,omitempty"`
	Project    string  `json:"project,omitempty"`
	// Sharing at a glance (tree): who a public node is shared with, and —
	// on a shared-with-me root — whose it is.
	SharedWith []noteShareTag `json:"sharedWith,omitempty"`
	OwnerName  string         `json:"ownerName,omitempty"`
	UpdatedBy  string         `json:"updatedBy"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	// Shared-list extras.
	Role     string `json:"role,omitempty"`
	Shared   bool   `json:"shared,omitempty"`
	Everyone bool   `json:"everyone,omitempty"` // reachable through an "everyone" grant
}

var noteMetaCols = []string{"id", "user_id", "parent_id", "type", "title", "slug", "icon", "sort_order", "visibility", "version", "summary", "source", "stale", "agent", "memory", "project", "updated_by", "created_at", "updated_at"}

func toNoteMeta(n models.Note) noteMeta {
	return noteMeta{ID: n.ID, UserID: n.UserID, ParentID: n.ParentID, Type: n.Type, Title: n.Title, Slug: n.Slug,
		Icon: n.Icon, SortOrder: n.SortOrder, Visibility: n.Visibility, Version: n.Version,
		Summary: n.Summary, Source: n.Source, Stale: n.Stale, Agent: n.Agent, Memory: n.Memory, Project: n.Project, UpdatedBy: n.UpdatedBy,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt}
}

// List returns the caller's own tree (every node they own), metadata only.
func (h *NoteHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var rows []models.Note
	if err := h.db.Select(noteMetaCols).Where("user_id = ?", userID).
		Order("sort_order ASC, created_at ASC").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list notes"})
		return
	}
	out := make([]noteMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, toNoteMeta(r))
	}
	annotateSharing(h.db, out)
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// noteShareTag is one grant on a node: a team, a person or everyone.
type noteShareTag struct {
	Kind string `json:"kind"` // team | user | everyone
	Name string `json:"name,omitempty"`
	Role string `json:"role"` // editor | viewer
}

// annotateSharing fills SharedWith on the public nodes of a list in one
// query (names looked up once each). Anyone who can read a node may see its
// grants — the share dialog already shows them.
func annotateSharing(db *gorm.DB, out []noteMeta) {
	var ids []string
	at := map[string][]int{}
	for i, m := range out {
		if m.Visibility == "public" {
			ids = append(ids, m.ID)
			at[m.ID] = append(at[m.ID], i)
		}
	}
	if len(ids) == 0 {
		return
	}
	var grants []models.NotePublicGrant
	for start := 0; start < len(ids); start += 500 {
		var part []models.NotePublicGrant
		db.Where("note_id IN ?", ids[start:min(start+500, len(ids))]).Order("subject_kind, created_at").Find(&part)
		grants = append(grants, part...)
	}
	labels := &WhiteboardSharingHandler{db: db}
	names := map[string]string{}
	for _, g := range grants {
		key := g.SubjectKind
		if g.SubjectID != nil {
			key += ":" + *g.SubjectID
		}
		name, ok := names[key]
		if !ok {
			if g.SubjectKind != "everyone" {
				name = labels.subjectLabel(g.SubjectKind, g.SubjectID)
			}
			names[key] = name
		}
		if g.SubjectKind != "everyone" && name == "" {
			continue // a deleted team or user
		}
		for _, i := range at[g.NoteID] {
			out[i].SharedWith = append(out[i].SharedWith, noteShareTag{Kind: g.SubjectKind, Name: name, Role: g.Role})
		}
	}
}

// ListShared returns the trees other people shared with the caller — by a
// user or team grant, or with everyone (those spaces show up for every user
// automatically, flagged `everyone`). Each share root comes back with
// parentId=null and its whole subtree; a node's role is the max of its
// ancestors' grants and its own.
func (h *NoteHandler) ListShared(c *gin.Context) {
	userID := c.GetString("userID")
	out := []noteMeta{}
	if userID == "" {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	type grantRow struct {
		NoteID      string
		Role        string
		SubjectKind string
	}
	var grants []grantRow
	h.db.Table("note_public_grants g").
		Select("g.note_id AS note_id, g.role AS role, g.subject_kind AS subject_kind").
		Joins("JOIN notes n ON n.id = g.note_id AND n.deleted_at IS NULL").
		Where("n.visibility = 'public' AND n.user_id <> ?", userID).
		Where("(g.subject_kind = 'user' AND g.subject_id = ?)"+
			" OR (g.subject_kind = 'everyone')"+
			" OR (g.subject_kind = 'team' AND g.subject_id IN (?)"+
			" AND EXISTS (SELECT 1 FROM teams t WHERE t.id = g.subject_id AND t.deleted_at IS NULL))",
			userID, h.db.Table("team_members").Select("team_id").Where("user_id = ?", userID)).
		Order("n.created_at ASC").
		Scan(&grants)

	granted := map[string]Role{}
	everyoneAt := map[string]bool{}
	var order []string
	for _, g := range grants {
		if _, ok := granted[g.NoteID]; !ok {
			order = append(order, g.NoteID)
		}
		if r := roleFromShareString(g.Role); roleRank[r] > roleRank[granted[g.NoteID]] {
			granted[g.NoteID] = r
		}
		if g.SubjectKind == "everyone" {
			everyoneAt[g.NoteID] = true
		}
	}
	// A grant nested inside another shared subtree isn't a separate root.
	isRoot := func(id string) bool {
		cur := id
		for i := 0; i < 64; i++ {
			var p struct{ ParentID *string }
			if h.db.Table("notes").Select("parent_id").Where("id = ? AND deleted_at IS NULL", cur).Scan(&p).Error != nil || p.ParentID == nil {
				return true
			}
			cur = *p.ParentID
			if _, ok := granted[cur]; ok {
				return false
			}
		}
		return true
	}
	seen := map[string]bool{}
	var walk func(id string, inherited Role, everyone, root bool)
	walk = func(id string, inherited Role, everyone, root bool) {
		if seen[id] {
			return
		}
		var n models.Note
		if err := h.db.Select(noteMetaCols).Where("id = ?", id).First(&n).Error; err != nil {
			return
		}
		seen[id] = true
		role := inherited
		if r, ok := granted[id]; ok && roleRank[r] > roleRank[role] {
			role = r
		}
		everyone = everyone || everyoneAt[id]
		m := toNoteMeta(n)
		if root {
			m.ParentID = nil
		}
		m.Role, m.Shared, m.Everyone = string(role), true, everyone
		out = append(out, m)
		var kids []models.Note
		h.db.Select("id").Where("parent_id = ?", id).Order("sort_order ASC, created_at ASC").Find(&kids)
		for _, k := range kids {
			walk(k.ID, role, everyone, false)
		}
	}
	for _, id := range order {
		if isRoot(id) {
			walk(id, NoRole, false, true)
		}
	}
	annotateSharing(h.db, out)
	owners := map[string]string{}
	for i := range out {
		if out[i].ParentID != nil {
			continue
		}
		name, ok := owners[out[i].UserID]
		if !ok {
			name = userDisplayName(h.db, out[i].UserID)
			owners[out[i].UserID] = name
		}
		out[i].OwnerName = name
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type noteCrumb struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Get returns the full note plus the caller's role, a breadcrumb (as far up
// as the caller can see) and the last editor's display name.
func (h *NoteHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id, msg := h.resolveNoteRef(userID, c.Param("id"))
	if msg != "" {
		c.JSON(http.StatusNotFound, gin.H{"error": msg})
		return
	}
	role := ResolveNoteRole(h.db, userID, id)
	var n models.Note
	if !role.AtLeast(ViewerRole) || h.db.Where("id = ?", id).First(&n).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.full(n, role, userID)})
}

func (h *NoteHandler) full(n models.Note, role Role, userID string) gin.H {
	crumbs := []noteCrumb{}
	cur := n.ParentID
	for i := 0; i < 64 && cur != nil; i++ {
		var p models.Note
		if h.db.Select("id", "title", "parent_id").Where("id = ?", *cur).First(&p).Error != nil || !canReadNote(h.db, userID, p.ID) {
			break
		}
		crumbs = append([]noteCrumb{{ID: p.ID, Title: p.Title}}, crumbs...)
		cur = p.ParentID
	}
	return gin.H{
		"id": n.ID, "userId": n.UserID, "parentId": n.ParentID, "type": n.Type, "title": n.Title,
		"slug": n.Slug, "icon": n.Icon, "body": n.Body, "sortOrder": n.SortOrder, "visibility": n.Visibility,
		"version": n.Version, "createdBy": n.CreatedBy, "updatedBy": n.UpdatedBy,
		"summary": n.Summary, "source": n.Source, "confidence": n.Confidence, "stale": n.Stale, "supersededBy": n.SupersededBy,
		"memory": n.Memory, "agent": n.Agent, "project": n.Project,
		"updatedByName": userDisplayName(h.db, n.UpdatedBy), "createdByName": userDisplayName(h.db, n.CreatedBy),
		"createdAt": n.CreatedAt, "updatedAt": n.UpdatedAt,
		"role": string(role), "breadcrumb": crumbs,
	}
}

type createNoteInput struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parentId"`
	Type     string  `json:"type"` // page (default) | folder | card | hub
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	// Cards: a one-line summary (≤200 characters, not just the title) and
	// how sure the writer is (0–1, default 1).
	Summary    string   `json:"summary"`
	Confidence *float64 `json:"confidence"`
	Icon       string   `json:"icon"`
	SortOrder  *int     `json:"sortOrder"`
	// Memory makes a new folder an agent memory folder.
	Memory bool `json:"memory"`
}

// Create adds a page or folder. Under a parent the caller needs editor there,
// and the new node joins that tree (owner inherited); otherwise it is a new
// space of the caller's own.
func (h *NoteHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in createNoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Untitled"
	}
	if len(in.Body) > noteBodyMax {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Note body is too large (1 MB max)"})
		return
	}
	typ := "page"
	switch in.Type {
	case "folder", "card", "hub":
		typ = in.Type
	}
	summary := strings.TrimSpace(in.Summary)
	if msg := checkCardSummary(title, summary); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	owner, workspace := userID, ""
	var parentID *string
	if in.ParentID != nil && strings.TrimSpace(*in.ParentID) != "" {
		pid := strings.TrimSpace(*in.ParentID)
		var parent models.Note
		if h.db.Select("id", "user_id", "workspace_id").Where("id = ?", pid).First(&parent).Error != nil || !canEditNote(h.db, userID, pid) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You can't add pages here"})
			return
		}
		owner, workspace, parentID = parent.UserID, parent.WorkspaceID, &pid
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.New().String()
	}
	sortOrder := 0
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	} else {
		var maxOrder *int
		q := h.db.Model(&models.Note{}).Where("user_id = ?", owner)
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
	n := models.Note{
		ID: id, UserID: owner, WorkspaceID: workspace, ParentID: parentID, Type: typ, SortOrder: sortOrder,
		Title: title, Slug: uniqueNoteSlug(h.db, owner, parentID, title, id), Icon: in.Icon, Body: in.Body,
		Visibility: "private", Version: 1, CreatedBy: userID, UpdatedBy: userID,
	}
	if typ == "folder" {
		n.Body = ""
		n.Memory = in.Memory
	}
	if typ == "card" || typ == "hub" {
		n.Summary, n.Source, n.Confidence = summary, "human", 1
		if noteSource(c) == noteSourceMCP {
			n.Source, n.Agent = "agent", noteAgent(c)
		}
		if in.Confidence != nil && *in.Confidence >= 0 && *in.Confidence <= 1 {
			n.Confidence = *in.Confidence
		}
	}
	if err := h.db.Create(&n).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save note"})
		return
	}
	recordNoteRevision(h.db, &n, userID, noteSource(c))
	notesearch.Index(h.db, n.ID, n.Title, n.Summary, n.Body)
	syncBodyLinks(h.db, n.ID, n.Body, userID)
	notifyNote(h.db, c, n.ID, realtime.EventNoteCreated)
	c.JSON(http.StatusCreated, gin.H{"data": h.full(n, ResolveNoteRole(h.db, userID, n.ID), userID)})
}

type updateNoteInput struct {
	Title       *string `json:"title"`
	Body        *string `json:"body"`
	BaseVersion *int64  `json:"baseVersion"` // required with title/body
	Icon        *string `json:"icon"`
	// ParentID: "" moves to the root (owner only), an id nests under it.
	ParentID  *string `json:"parentId"`
	SortOrder *int    `json:"sortOrder"`
	// Cards.
	Summary *string `json:"summary"`
	Stale   *bool   `json:"stale"`
	// Memory turns a folder into an agent memory folder or back (owner only).
	Memory *bool `json:"memory"`
}

// canRestructure: the owner always; an editor only inside a shared subtree
// (never the share root itself — they can't move/delete what was shared).
func (h *NoteHandler) canRestructure(userID string, n *models.Note) bool {
	if n.UserID == userID {
		return true
	}
	return n.ParentID != nil && canEditNote(h.db, userID, n.ID) && canEditNote(h.db, userID, *n.ParentID)
}

// isNoteDescendant reports whether candidate lies inside nodeID's subtree.
func (h *NoteHandler) isNoteDescendant(nodeID, candidate string) bool {
	cur := candidate
	for i := 0; i < 64 && cur != ""; i++ {
		if cur == nodeID {
			return true
		}
		var p struct{ ParentID *string }
		if h.db.Table("notes").Select("parent_id").Where("id = ?", cur).Scan(&p).Error != nil || p.ParentID == nil {
			return false
		}
		cur = *p.ParentID
	}
	return false
}

func (h *NoteHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var in updateNoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role := ResolveNoteRole(h.db, userID, id)
	var n models.Note
	if !role.AtLeast(ViewerRole) || h.db.Where("id = ?", id).First(&n).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	if !role.AtLeast(EditorRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this note"})
		return
	}

	// Structure first (it doesn't touch the version).
	if in.ParentID != nil || in.SortOrder != nil {
		if !h.canRestructure(userID, &n) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You can't move this note"})
			return
		}
		updates := map[string]any{}
		if in.SortOrder != nil {
			updates["sort_order"] = *in.SortOrder
		}
		if in.ParentID != nil {
			target := strings.TrimSpace(*in.ParentID)
			if target == "" {
				if n.UserID != userID {
					c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can move this to the top level"})
					return
				}
				updates["parent_id"] = nil
			} else {
				var p models.Note
				if target == id || h.db.Select("id", "user_id").Where("id = ?", target).First(&p).Error != nil ||
					p.UserID != n.UserID || !canEditNote(h.db, userID, target) || h.isNoteDescendant(id, target) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid parent"})
					return
				}
				updates["parent_id"] = target
			}
		}
		if err := h.db.Model(&models.Note{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "move note"})
			return
		}
		notifyNote(h.db, c, id, realtime.EventNoteMoved)
	}
	// An agent (MCP) doesn't rewrite a card a person wrote: the change is filed
	// as a suggestion for them.
	if noteSource(c) == noteSourceMCP && n.Type == "card" && n.Source == "human" &&
		(in.Title != nil || in.Body != nil || in.Summary != nil || in.Stale != nil) {
		h.suggest(n, "edit", userID, noteAgent(c), gin.H{"title": in.Title, "body": in.Body, "summary": in.Summary, "stale": in.Stale})
		c.JSON(http.StatusAccepted, gin.H{"data": gin.H{"suggested": true, "hint": "written by a person — your change was added to the suggestions for them to accept"}})
		return
	}
	if in.Icon != nil {
		h.db.Model(&models.Note{}).Where("id = ?", id).Update("icon", *in.Icon)
	}
	if in.Memory != nil {
		if n.Type != "folder" || !isNoteOwner(h.db, userID, id) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only the owner can make a folder a memory folder"})
			return
		}
		h.db.Model(&models.Note{}).Where("id = ?", id).Update("memory", *in.Memory)
		if in.Title == nil && in.Body == nil && in.Summary == nil && in.Stale == nil {
			notifyNote(h.db, c, id, realtime.EventNoteUpdated)
		}
	}
	if in.Summary != nil || in.Stale != nil {
		updates := map[string]any{}
		if in.Summary != nil {
			s := strings.TrimSpace(*in.Summary)
			title := n.Title
			if in.Title != nil {
				title = strings.TrimSpace(*in.Title)
			}
			if msg := checkCardSummary(title, s); msg != "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": msg})
				return
			}
			updates["summary"] = s
			n.Summary = s
		}
		if in.Stale != nil {
			updates["stale"] = *in.Stale
			if !*in.Stale {
				updates["superseded_by"] = nil
			}
		}
		h.db.Model(&models.Note{}).Where("id = ?", id).Updates(updates)
		notesearch.Index(h.db, n.ID, n.Title, n.Summary, n.Body)
		if in.Title == nil && in.Body == nil {
			notifyNote(h.db, c, id, realtime.EventNoteUpdated)
		}
	}
	if in.Title != nil || in.Body != nil {
		if in.BaseVersion == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "baseVersion is required when changing the title or body"})
			return
		}
		if in.Title != nil {
			t := strings.TrimSpace(*in.Title)
			if t == "" {
				t = "Untitled"
			}
			in.Title = &t
		}
		if in.Body != nil && len(*in.Body) > noteBodyMax {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Note body is too large (1 MB max)"})
			return
		}
		if n.Type == "folder" {
			in.Body = nil
		}
		conflict, err := applyNoteContent(h.db, c, &n, in.Title, in.Body, *in.BaseVersion, userID, noteSource(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save note"})
			return
		}
		if conflict {
			c.JSON(http.StatusConflict, gin.H{"error": "This note was changed by someone else", "current": h.full(n, role, userID)})
			return
		}
	} else if in.ParentID == nil && in.SortOrder == nil && in.Icon != nil {
		notifyNote(h.db, c, id, realtime.EventNoteUpdated)
	}
	h.db.Where("id = ?", id).First(&n)
	c.JSON(http.StatusOK, gin.H{"data": h.full(n, role, userID)})
}

type appendNoteInput struct {
	Text string `json:"text" binding:"required"`
}

// Append adds text at the end of a page atomically — for agents keeping a log
// in a note without racing human edits.
func (h *NoteHandler) Append(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var in appendNoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role := ResolveNoteRole(h.db, userID, id)
	var n models.Note
	if !role.AtLeast(ViewerRole) || h.db.Where("id = ?", id).First(&n).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	if !role.AtLeast(EditorRole) || n.Type == "folder" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Can't append to this note"})
		return
	}
	if len(n.Body)+len(in.Text) > noteBodyMax {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Note body is too large (1 MB max)"})
		return
	}
	if err := appendNoteBody(h.db, c, &n, in.Text, userID, noteSource(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save note"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.full(n, role, userID)})
}

// Delete soft-deletes a node and its subtree (recoverable; search drops it).
func (h *NoteHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var n models.Note
	if !canReadNote(h.db, userID, id) || h.db.Where("id = ?", id).First(&n).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	if !h.canRestructure(userID, &n) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't delete this note"})
		return
	}
	ids := []string{id}
	for frontier := []string{id}; len(frontier) > 0 && len(ids) < 10000; {
		var kids []string
		h.db.Model(&models.Note{}).Where("parent_id IN ?", frontier).Pluck("id", &kids)
		ids = append(ids, kids...)
		frontier = kids
	}
	// Recipients are resolved before the rows disappear from normal queries.
	recipients, everyone := realtime.NoteRecipients(h.db, id)
	if err := h.db.Where("id IN ?", ids).Delete(&models.Note{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete note"})
		return
	}
	notesearch.Remove(h.db, ids)
	notifyRT(h.db, c, realtime.Event{Type: realtime.EventNoteDeleted, NoteID: id, Recipients: recipients, Everyone: everyone})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": len(ids)}})
}

// Revisions lists the page history (newest first, no bodies).
func (h *NoteHandler) Revisions(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canReadNote(h.db, userID, id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	var revs []models.NoteRevision
	h.db.Select("id", "note_id", "version", "title", "author_id", "source", "git_commit_sha", "created_at", "updated_at").
		Where("note_id = ?", id).Order("created_at DESC").Limit(200).Find(&revs)
	names := map[string]string{}
	out := make([]gin.H, 0, len(revs))
	for _, r := range revs {
		if _, ok := names[r.AuthorID]; !ok {
			names[r.AuthorID] = userDisplayName(h.db, r.AuthorID)
		}
		out = append(out, gin.H{"id": r.ID, "version": r.Version, "title": r.Title, "authorId": r.AuthorID,
			"authorName": names[r.AuthorID], "source": r.Source, "gitCommitSha": r.GitCommitSHA,
			"createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *NoteHandler) Revision(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var r models.NoteRevision
	if !canReadNote(h.db, userID, id) || h.db.Where("id = ? AND note_id = ?", c.Param("revId"), id).First(&r).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Revision not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": r})
}

// Restore makes an old revision the current content (a new revision, so the
// restore itself can be undone).
func (h *NoteHandler) Restore(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	role := ResolveNoteRole(h.db, userID, id)
	var n models.Note
	var r models.NoteRevision
	if !role.AtLeast(ViewerRole) || h.db.Where("id = ?", id).First(&n).Error != nil ||
		h.db.Where("id = ? AND note_id = ?", c.Param("revId"), id).First(&r).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Revision not found"})
		return
	}
	if !role.AtLeast(EditorRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this note"})
		return
	}
	conflict, err := applyNoteContent(h.db, c, &n, &r.Title, &r.Body, n.Version, userID, noteSourceRestore)
	if err != nil || conflict {
		c.JSON(http.StatusConflict, gin.H{"error": "The note changed while restoring — try again"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.full(n, role, userID)})
}

// Backlinks lists notes the caller can read that link to this one.
func (h *NoteHandler) Backlinks(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canReadNote(h.db, userID, id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	// Every note with a link here, of any type (body links included — they
	// are mirrored into note_links on save).
	var links []models.NoteLink
	h.db.Where("dst_note_id = ? AND src_note_id <> ?", id, id).Order("created_at DESC").Limit(200).Find(&links)
	out := []gin.H{}
	seen := map[string]bool{}
	for _, l := range links {
		if seen[l.SrcNoteID+"|"+l.Type] {
			continue
		}
		seen[l.SrcNoteID+"|"+l.Type] = true
		var r models.Note
		if h.db.Select("id", "title", "icon", "type").Where("id = ?", l.SrcNoteID).First(&r).Error != nil || !canReadNote(h.db, userID, r.ID) {
			continue
		}
		out = append(out, gin.H{"id": r.ID, "title": r.Title, "icon": r.Icon, "type": r.Type, "linkType": l.Type})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// Search is full-text search over the notes the caller can read.
func (h *NoteHandler) Search(c *gin.Context) {
	userID := c.GetString("userID")
	q := strings.TrimSpace(c.Query("q"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	out := []gin.H{}
	if q == "" {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	ids, err := notesearch.Candidates(h.db, q, 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search notes"})
		return
	}
	terms := notesearch.Terms(q)
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		var n models.Note
		if h.db.Select("id", "user_id", "parent_id", "type", "title", "icon", "body", "updated_at").Where("id = ?", id).First(&n).Error != nil {
			continue
		}
		if n.UserID != userID && !canReadNote(h.db, userID, id) {
			continue
		}
		crumbs := []string{}
		cur := n.ParentID
		for i := 0; i < 4 && cur != nil; i++ {
			var p models.Note
			if h.db.Select("id", "title", "parent_id").Where("id = ?", *cur).First(&p).Error != nil {
				break
			}
			crumbs = append([]string{p.Title}, crumbs...)
			cur = p.ParentID
		}
		out = append(out, gin.H{"id": n.ID, "title": n.Title, "type": n.Type, "icon": n.Icon,
			"snippet": notesearch.Snippet(n.Body, terms, 160), "breadcrumb": crumbs, "updatedAt": n.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// checkCardSummary answers a summary that can't be used: too long, or just
// the title again (it would waste recall's budget).
func checkCardSummary(title, summary string) string {
	if summary == "" {
		return ""
	}
	if len([]rune(summary)) > 200 {
		return "summary is too long (200 characters max) — keep it to one sentence"
	}
	if strings.EqualFold(summary, strings.TrimSpace(title)) {
		return "summary must say more than the title"
	}
	return ""
}
