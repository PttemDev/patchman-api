package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Agent memory folders and their scope, per-person memory settings, and the
// suggestion inbox. A person can have several memories at once: their own
// (personal) and any memory folder a team, a colleague or everyone shares
// with them. recall reads all of them; remember writes to the personal one
// unless told otherwise.

// ── scope ────────────────────────────────────────────────────────────────

// memScope is how far a note reaches, as one person sees it.
type memScope struct {
	Kind  string   `json:"kind"`            // personal | team | shared | everyone
	Teams []string `json:"teams,omitempty"` // team names (kind team)
	Owner string   `json:"owner,omitempty"` // the owner's name when it isn't the viewer (kind shared)
}

// Label is the tag recall prints after a hit ("" for personal).
func (s memScope) Label() string {
	switch s.Kind {
	case "team":
		return "team: " + strings.Join(s.Teams, ", ")
	case "everyone":
		return "everyone"
	case "shared":
		if s.Owner != "" {
			return "shared by " + s.Owner
		}
		return "shared"
	}
	return ""
}

// scopeFacts are the sharing facts of a node with all its ancestors. Grants
// count only on public nodes — the same gate access resolution applies.
type scopeFacts struct {
	owner    string
	teams    []string // team ids
	everyone bool
	users    bool
}

// scopeResolver memoizes facts per node, so a recall over many cards of one
// folder walks that folder's ancestors once.
type scopeResolver struct {
	db    *gorm.DB
	facts map[string]scopeFacts
	teams map[string]string // team id → name ("" for a deleted team)
}

func newScopeResolver(db *gorm.DB) *scopeResolver {
	return &scopeResolver{db: db, facts: map[string]scopeFacts{}, teams: map[string]string{}}
}

func (r *scopeResolver) factsOf(id string, depth int) scopeFacts {
	if f, ok := r.facts[id]; ok {
		return f
	}
	var node struct {
		UserID      string
		Visibility  string
		WorkspaceID string
		ParentID    *string
	}
	r.db.Table("notes").Select("user_id, visibility, workspace_id, parent_id").Where("id = ? AND deleted_at IS NULL", id).Scan(&node)
	f := scopeFacts{owner: node.UserID}
	if node.ParentID != nil && *node.ParentID != "" && depth < 64 {
		p := r.factsOf(*node.ParentID, depth+1)
		f.teams = append(f.teams, p.teams...)
		f.everyone, f.users = p.everyone, p.users
	}
	if node.Visibility == "public" {
		var grants []struct {
			SubjectKind string
			SubjectID   *string
		}
		r.db.Table("note_public_grants").Select("subject_kind, subject_id").Where("note_id = ?", id).Scan(&grants)
		for _, g := range grants {
			switch g.SubjectKind {
			case "team":
				if g.SubjectID != nil {
					f.teams = append(f.teams, *g.SubjectID)
				}
			case "everyone":
				f.everyone = true
			case "user":
				f.users = true
			}
		}
	}
	if node.WorkspaceID != "" {
		var teamID *string
		if r.db.Table("workspaces").Select("team_id").Where("id = ?", node.WorkspaceID).Scan(&teamID).Error == nil && teamID != nil && *teamID != "" {
			f.teams = append(f.teams, *teamID)
		}
	}
	r.facts[id] = f
	return f
}

func (r *scopeResolver) teamName(id string) string {
	if n, ok := r.teams[id]; ok {
		return n
	}
	var name string
	r.db.Table("teams").Select("name").Where("id = ? AND deleted_at IS NULL", id).Scan(&name)
	r.teams[id] = name
	return name
}

// scope is the note's reach as viewer sees it: a team share wins (that is
// what makes a memory "the team's"), then everyone, then someone else's or
// a share with specific people; otherwise it is the viewer's own.
func (r *scopeResolver) scope(viewer, id string) memScope {
	f := r.factsOf(id, 0)
	var teams []string
	for _, t := range dedupe(f.teams) {
		if n := r.teamName(t); n != "" {
			teams = append(teams, n)
		}
	}
	sort.Strings(teams)
	switch {
	case len(teams) > 0:
		return memScope{Kind: "team", Teams: teams}
	case f.everyone:
		return memScope{Kind: "everyone"}
	case f.owner != viewer:
		return memScope{Kind: "shared", Owner: userDisplayName(r.db, f.owner)}
	case f.users:
		return memScope{Kind: "shared"}
	}
	return memScope{Kind: "personal"}
}

// ── memory folders ───────────────────────────────────────────────────────

// memoryFolders lists the memory folders userID can read: their own first,
// then the ones shared with them.
func (h *NoteHandler) memoryFolders(userID string) []models.Note {
	var own, others []models.Note
	h.db.Where("user_id = ? AND type = ? AND memory = ?", userID, "folder", true).Order("created_at ASC").Find(&own)
	h.db.Where("user_id <> ? AND type = ? AND memory = ?", userID, "folder", true).Order("created_at ASC").Limit(500).Find(&others)
	out := own
	for _, f := range others {
		if canReadNote(h.db, userID, f.ID) {
			out = append(out, f)
		}
	}
	return out
}

type memoryFolderView struct {
	ID          string   `json:"id"`
	ParentID    *string  `json:"parentId"`
	Title       string   `json:"title"`
	Icon        string   `json:"icon,omitempty"`
	OwnerID     string   `json:"ownerId"`
	OwnerName   string   `json:"ownerName"`
	Scope       memScope `json:"scope"`
	CanWrite    bool     `json:"canWrite"`
	IsOwner     bool     `json:"isOwner"`
	Cards       int64    `json:"cards"`
	Suggestions int64    `json:"suggestions"`
	TeamDefault bool     `json:"teamDefault,omitempty"`
	// Projects are the folders that group this memory's cards by project.
	Projects []memoryProjectView `json:"projects"`
}

type memoryProjectView struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project"`
	Cards   int64  `json:"cards"`
}

func (h *NoteHandler) memorySettings(userID string) models.NoteMemorySettings {
	s := models.NoteMemorySettings{UserID: userID}
	h.db.Where("user_id = ?", userID).First(&s)
	return s
}

// MemoryOverview lists the caller's memory folders with their scope, card
// and open-suggestion counts, plus their memory settings (GET /notes/memory).
func (h *NoteHandler) MemoryOverview(c *gin.Context) {
	userID := c.GetString("userID")
	settings := h.memorySettings(userID)
	r := newScopeResolver(h.db)
	out := []memoryFolderView{}
	for _, f := range h.memoryFolders(userID) {
		v := memoryFolderView{
			ID: f.ID, ParentID: f.ParentID, Title: f.Title, Icon: f.Icon, OwnerID: f.UserID, OwnerName: userDisplayName(h.db, f.UserID),
			Scope: r.scope(userID, f.ID), CanWrite: canEditNote(h.db, userID, f.ID), IsOwner: f.UserID == userID,
			TeamDefault: settings.TeamFolderID == f.ID,
		}
		v.Projects = []memoryProjectView{}
		var projects []models.Note
		h.db.Select("id", "title", "project").Where("parent_id = ? AND type = ? AND project <> ?", f.ID, "folder", "").
			Order("title ASC").Find(&projects)
		h.db.Model(&models.Note{}).Where("parent_id = ? AND type = ?", f.ID, "card").Count(&v.Cards)
		for _, p := range projects {
			pv := memoryProjectView{ID: p.ID, Title: p.Title, Project: p.Project}
			h.db.Model(&models.Note{}).Where("parent_id = ? AND type = ?", p.ID, "card").Count(&pv.Cards)
			v.Cards += pv.Cards
			v.Projects = append(v.Projects, pv)
		}
		if v.CanWrite {
			h.db.Model(&models.NoteSuggestion{}).Where("folder_id = ? AND status = ?", f.ID, "open").Count(&v.Suggestions)
		}
		out = append(out, v)
	}
	res := gin.H{"folders": out, "settings": settings}
	hasOwn := false
	for _, v := range out {
		hasOwn = hasOwn || v.IsOwner && v.Scope.Kind == "personal"
	}
	if !hasOwn {
		res["hint"] = "Your personal Agent Memory folder is created by your first remember()."
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// teamMemories are the memory folders shared with a team that userID can
// write to — their own shared ones included.
func (h *NoteHandler) teamMemories(userID string) ([]models.Note, *scopeResolver) {
	r := newScopeResolver(h.db)
	var out []models.Note
	for _, f := range h.memoryFolders(userID) {
		if r.scope(userID, f.ID).Kind == "team" && canEditNote(h.db, userID, f.ID) {
			out = append(out, f)
		}
	}
	return out, r
}

type memorySettingsInput struct {
	TeamFolderID  *string `json:"teamFolderId"`
	LLMProviderID *string `json:"llmProviderId"`
	LLMModel      *string `json:"llmModel"`
}

// UpdateMemorySettings saves the default team memory and the card-splitting
// model (PUT /notes/memory/settings). Empty values clear them.
func (h *NoteHandler) UpdateMemorySettings(c *gin.Context) {
	userID := c.GetString("userID")
	var in memorySettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s := h.memorySettings(userID)
	if in.TeamFolderID != nil {
		id := strings.TrimSpace(*in.TeamFolderID)
		if id != "" {
			cands, _ := h.teamMemories(userID)
			ok := false
			for _, f := range cands {
				ok = ok || f.ID == id
			}
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "That folder isn't a team memory you can write to"})
				return
			}
		}
		s.TeamFolderID = id
	}
	if in.LLMProviderID != nil {
		id := strings.TrimSpace(*in.LLMProviderID)
		if id != "" {
			var n int64
			h.db.Model(&models.LLMProvider{}).Where("id = ? AND user_id = ?", id, userID).Count(&n)
			if n == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "AI provider not found"})
				return
			}
		}
		s.LLMProviderID = id
	}
	if in.LLMModel != nil {
		s.LLMModel = strings.TrimSpace(*in.LLMModel)
	}
	s.UpdatedAt = time.Now()
	if err := h.db.Save(&s).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": s})
}

// ── suggestions ──────────────────────────────────────────────────────────

type suggestionView struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	NoteID        string         `json:"noteId"`
	NoteTitle     string         `json:"noteTitle"`
	NoteType      string         `json:"noteType"`
	FolderID      string         `json:"folderId"`
	FolderTitle   string         `json:"folderTitle,omitempty"`
	Payload       map[string]any `json:"payload"`
	Current       gin.H          `json:"current,omitempty"` // edit / merge: the note as it is now
	Target        gin.H          `json:"target,omitempty"`  // link / supersede: the other end
	CreatedBy     string         `json:"createdBy"`
	CreatedByName string         `json:"createdByName"`
	Agent         string         `json:"agent,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
}

// Suggestions lists the open suggestions on notes the caller can edit
// (GET /notes/suggestions[?folderId=]).
func (h *NoteHandler) Suggestions(c *gin.Context) {
	userID := c.GetString("userID")
	q := h.db.Where("status = ?", "open")
	if f := c.Query("folderId"); f != "" {
		q = q.Where("folder_id = ?", f)
	}
	var rows []models.NoteSuggestion
	q.Order("created_at DESC").Limit(500).Find(&rows)
	out := []suggestionView{}
	folders := map[string]string{}
	for _, s := range rows {
		var n models.Note
		if h.db.Where("id = ?", s.NoteID).First(&n).Error != nil || !canEditNote(h.db, userID, n.ID) {
			continue
		}
		v := suggestionView{ID: s.ID, Kind: s.Kind, NoteID: n.ID, NoteTitle: n.Title, NoteType: n.Type, FolderID: s.FolderID,
			CreatedBy: s.CreatedBy, CreatedByName: userDisplayName(h.db, s.CreatedBy), Agent: s.Agent, CreatedAt: s.CreatedAt}
		_ = json.Unmarshal([]byte(s.Payload), &v.Payload)
		if s.FolderID != "" {
			if _, ok := folders[s.FolderID]; !ok {
				var t string
				h.db.Table("notes").Select("title").Where("id = ?", s.FolderID).Scan(&t)
				folders[s.FolderID] = t
			}
			v.FolderTitle = folders[s.FolderID]
		}
		switch s.Kind {
		case "edit", "merge":
			v.Current = gin.H{"title": n.Title, "summary": n.Summary, "body": n.Body, "version": n.Version}
		case "link", "supersede":
			ref, _ := v.Payload["dst"].(string)
			if s.Kind == "supersede" {
				ref, _ = v.Payload["by"].(string)
			}
			if strings.HasPrefix(ref, "patchman://") {
				v.Target = gin.H{"uri": ref}
			} else if ref != "" {
				var t models.Note
				if h.db.Select("id", "title", "type").Where("id = ?", ref).First(&t).Error == nil && canReadNote(h.db, userID, t.ID) {
					v.Target = gin.H{"id": t.ID, "title": t.Title, "type": t.Type}
				}
			}
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type acceptInput struct {
	// Overrides for edit / merge: what to save instead of the suggested text.
	Title   *string `json:"title"`
	Summary *string `json:"summary"`
	Body    *string `json:"body"`
}

// openSuggestion loads an open suggestion the caller may decide on.
func (h *NoteHandler) openSuggestion(c *gin.Context, userID string) (models.NoteSuggestion, models.Note, bool) {
	var s models.NoteSuggestion
	var n models.Note
	if h.db.Where("id = ? AND status = ?", c.Param("id"), "open").First(&s).Error != nil ||
		h.db.Where("id = ?", s.NoteID).First(&n).Error != nil || !canReadNote(h.db, userID, n.ID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Suggestion not found"})
		return s, n, false
	}
	if !canEditNote(h.db, userID, n.ID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this note"})
		return s, n, false
	}
	return s, n, true
}

// decide closes a suggestion; the conditional update makes a double click
// (or two people at once) decide it only once.
func (h *NoteHandler) decide(s models.NoteSuggestion, status, userID string) bool {
	res := h.db.Model(&models.NoteSuggestion{}).Where("id = ? AND status = ?", s.ID, "open").
		Updates(map[string]any{"status": status, "decided_by": userID})
	if res.RowsAffected == 0 {
		return false
	}
	notifyNote(h.db, nil, s.NoteID, realtime.EventNoteSuggestion)
	return true
}

// AcceptSuggestion applies a suggestion (POST /notes/suggestions/:id/accept).
// Edits and merges save on top of the note as it is now — the person saw the
// current text next to the suggestion and may have adjusted it.
func (h *NoteHandler) AcceptSuggestion(c *gin.Context) {
	userID := c.GetString("userID")
	var in acceptInput
	_ = c.ShouldBindJSON(&in)
	s, n, ok := h.openSuggestion(c, userID)
	if !ok {
		return
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(s.Payload), &p)
	str := func(k string) *string {
		if v, ok := p[k].(string); ok {
			return &v
		}
		return nil
	}
	switch s.Kind {
	case "link":
		dst, typ := "", ""
		if v := str("dst"); v != nil {
			dst = *v
		}
		if v := str("type"); v != nil {
			typ = *v
		}
		// A person accepted it, so the note's own rule (no agent edits on a
		// person's note) is satisfied; the edge still records the agent's idea.
		r := h.addLink(c, userID, false, "", n, dst, typ, 1)
		if r.err != "" {
			c.JSON(r.status, gin.H{"error": r.err})
			return
		}
		if id, _ := r.data["id"].(string); id != "" {
			h.db.Model(&models.NoteLink{}).Where("id = ?", id).Updates(map[string]any{"source": "agent", "created_by": s.CreatedBy})
		}
	case "supersede":
		by := ""
		if v := str("by"); v != nil {
			by = *v
		}
		h.db.Model(&models.Note{}).Where("id = ?", n.ID).Updates(map[string]any{"stale": true, "superseded_by": by})
		notifyNote(h.db, c, n.ID, realtime.EventNoteUpdated)
	case "edit", "merge":
		title, body, summary := in.Title, in.Body, in.Summary
		if s.Kind == "edit" {
			if title == nil {
				title = str("title")
			}
			if body == nil {
				body = str("body")
			}
			if summary == nil {
				summary = str("summary")
			}
		} else if body == nil {
			text := ""
			if v := str("text"); v != nil {
				text = *v
			}
			merged := strings.TrimRight(n.Body, "\n")
			if merged != "" {
				merged += "\n\n"
			}
			merged += text
			body = &merged
		}
		if summary != nil {
			t := n.Title
			if title != nil {
				t = *title
			}
			if msg := checkCardSummary(t, strings.TrimSpace(*summary)); msg != "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": msg})
				return
			}
			h.db.Model(&models.Note{}).Where("id = ?", n.ID).Update("summary", strings.TrimSpace(*summary))
			n.Summary = strings.TrimSpace(*summary)
		}
		if s.Kind == "edit" {
			if v, ok := p["stale"].(bool); ok {
				h.db.Model(&models.Note{}).Where("id = ?", n.ID).Update("stale", v)
			}
		}
		if title != nil || body != nil {
			if body != nil && len(*body) > noteBodyMax {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Note body is too large (1 MB max)"})
				return
			}
			if _, err := applyNoteContent(h.db, c, &n, title, body, n.Version, userID, noteSourceEdit); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "save note"})
				return
			}
		} else {
			afterNoteContentChange(h.db, c, &n, userID, noteSourceEdit)
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown suggestion kind"})
		return
	}
	h.decide(s, "accepted", userID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// RejectSuggestion dismisses a suggestion (POST /notes/suggestions/:id/reject).
func (h *NoteHandler) RejectSuggestion(c *gin.Context) {
	userID := c.GetString("userID")
	s, _, ok := h.openSuggestion(c, userID)
	if !ok {
		return
	}
	h.decide(s, "rejected", userID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
