package handlers

import (
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notesearch"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Every content change to a note — REST edits, MCP edits, restores, and (later)
// Git pulls — goes through applyNoteContent so version bumps, history, the
// search index and realtime fan-out can never drift apart.

const (
	noteBodyMax          = 1 << 20 // 1 MB of markdown
	noteRevisionCoalesce = 10 * time.Minute
	noteRevisionCap      = 500
	noteSourceEdit       = "edit"
	noteSourceMCP        = "mcp"
	noteSourceRestore    = "restore"
	mcpClientHeader      = "X-PatchMan-Client" // set by the MCP caller on every in-process / cloud call
	mcpClientHeaderValue = "mcp"
)

// noteSource labels a write for the history: an AI agent's edit (via MCP)
// is shown differently from a human's.
// agentHeader names the agent behind an MCP call (see mcp.AgentHeader).
const agentHeader = "X-PatchMan-Agent"

// noteAgent is the name of the AI agent making this request ("" for a
// person, or an agent that didn't name itself).
func noteAgent(c *gin.Context) string {
	if noteSource(c) != noteSourceMCP {
		return ""
	}
	a := strings.TrimSpace(c.GetHeader(agentHeader))
	if r := []rune(a); len(r) > 60 {
		a = string(r[:60])
	}
	return a
}

// projectHeader names the project an MCP agent works in (its working
// directory's name, sent by the stdio bridge).
const projectHeader = "X-PatchMan-Project"

// noteProject is the calling agent's project, "" for a person or when the
// agent didn't say.
func noteProject(c *gin.Context) string {
	if noteSource(c) != noteSourceMCP {
		return ""
	}
	p := strings.TrimSpace(c.GetHeader(projectHeader))
	if r := []rune(p); len(r) > 120 {
		p = string(r[:120])
	}
	return p
}

func noteSource(c *gin.Context) string {
	if c != nil && c.GetHeader(mcpClientHeader) == mcpClientHeaderValue {
		return noteSourceMCP
	}
	return noteSourceEdit
}

// applyNoteContent sets title and/or body iff the note is still at
// baseVersion (optimistic concurrency). conflict=true means someone saved in
// between; the caller answers 409 with the current note.
func applyNoteContent(db *gorm.DB, c *gin.Context, n *models.Note, title, body *string, baseVersion int64, actor, source string) (conflict bool, err error) {
	updates := map[string]any{
		"version":    gorm.Expr("version + 1"),
		"updated_by": actor,
		"updated_at": time.Now(),
	}
	if title != nil {
		updates["title"] = *title
	}
	if body != nil {
		updates["body"] = *body
	}
	res := db.Model(&models.Note{}).Where("id = ? AND version = ?", n.ID, baseVersion).Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		db.Where("id = ?", n.ID).First(n)
		return true, nil
	}
	if err := db.Where("id = ?", n.ID).First(n).Error; err != nil {
		return false, err
	}
	afterNoteContentChange(db, c, n, actor, source)
	return false, nil
}

// appendNoteBody appends text atomically (no baseVersion: an agent's dev-log
// append never clobbers a concurrent human edit).
func appendNoteBody(db *gorm.DB, c *gin.Context, n *models.Note, text, actor, source string) error {
	sep := ""
	if strings.TrimSpace(n.Body) != "" {
		sep = "\n\n"
	}
	res := db.Model(&models.Note{}).Where("id = ?", n.ID).Updates(map[string]any{
		"body":       gorm.Expr("body || ?", sep+text),
		"version":    gorm.Expr("version + 1"),
		"updated_by": actor,
		"updated_at": time.Now(),
	})
	if res.Error != nil {
		return res.Error
	}
	if err := db.Where("id = ?", n.ID).First(n).Error; err != nil {
		return err
	}
	afterNoteContentChange(db, c, n, actor, source)
	return nil
}

func afterNoteContentChange(db *gorm.DB, c *gin.Context, n *models.Note, actor, source string) {
	recordNoteRevision(db, n, actor, source)
	notesearch.Index(db, n.ID, n.Title, n.Summary, n.Body)
	syncBodyLinks(db, n.ID, n.Body, actor)
	notifyNote(db, c, n.ID, realtime.EventNoteUpdated)
}

// recordNoteRevision keeps the history: consecutive edits by the same author
// from the same source within the coalesce window update one row (an editing
// session, not every autosave); anything else starts a new revision.
func recordNoteRevision(db *gorm.DB, n *models.Note, actor, source string) {
	if n.Type == "folder" {
		return
	}
	var last models.NoteRevision
	if err := db.Where("note_id = ?", n.ID).Order("created_at DESC").First(&last).Error; err == nil &&
		last.AuthorID == actor && last.Source == source &&
		(source == noteSourceEdit || source == noteSourceMCP) &&
		time.Since(last.CreatedAt) < noteRevisionCoalesce {
		db.Model(&last).Updates(map[string]any{"version": n.Version, "title": n.Title, "body": n.Body})
		return
	}
	db.Create(&models.NoteRevision{
		ID: uuid.New().String(), NoteID: n.ID, Version: n.Version,
		Title: n.Title, Body: n.Body, AuthorID: actor, Source: source,
	})
	var count int64
	db.Model(&models.NoteRevision{}).Where("note_id = ?", n.ID).Count(&count)
	if count > noteRevisionCap {
		var old []string
		db.Model(&models.NoteRevision{}).Where("note_id = ? AND source IN ?", n.ID, []string{noteSourceEdit, noteSourceMCP}).
			Order("created_at ASC").Limit(int(count-noteRevisionCap)).Pluck("id", &old)
		if len(old) > 0 {
			db.Where("id IN ?", old).Delete(&models.NoteRevision{})
		}
	}
}

func notifyNote(db *gorm.DB, c *gin.Context, noteID, eventType string) {
	recipients, everyone := realtime.NoteRecipients(db, noteID)
	notifyRT(db, c, realtime.Event{Type: eventType, NoteID: noteID, Recipients: recipients, Everyone: everyone})
}

// noteSlug turns a title into a stable path segment (Git file names): ASCII
// lower-case, Turkish letters transliterated, everything else a dash.
func noteSlug(title string) string {
	r := strings.NewReplacer("ç", "c", "Ç", "c", "ğ", "g", "Ğ", "g", "ı", "i", "İ", "i",
		"ö", "o", "Ö", "o", "ş", "s", "Ş", "s", "ü", "u", "Ü", "u")
	s := slugify(r.Replace(title))
	if len(s) > 80 {
		s = strings.Trim(s[:80], "-")
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

// uniqueNoteSlug suffixes -2, -3… until no live sibling uses the slug.
func uniqueNoteSlug(db *gorm.DB, ownerID string, parentID *string, title, exceptID string) string {
	base := noteSlug(title)
	for i := 1; i < 1000; i++ {
		cand := base
		if i > 1 {
			cand = base + "-" + strconv.Itoa(i)
		}
		q := db.Model(&models.Note{}).Where("user_id = ? AND slug = ? AND id <> ?", ownerID, cand, exceptID)
		if parentID == nil {
			q = q.Where("parent_id IS NULL")
		} else {
			q = q.Where("parent_id = ?", *parentID)
		}
		var n int64
		q.Count(&n)
		if n == 0 {
			return cand
		}
	}
	return base + "-" + uuid.New().String()[:8]
}

func userDisplayName(db *gorm.DB, id string) string {
	if id == "" {
		return ""
	}
	var u struct{ Email, Name string }
	if db.Table("users").Select("email, name").Where("id = ?", id).Scan(&u).Error == nil {
		if u.Name != "" {
			return u.Name
		}
		return u.Email
	}
	return ""
}
