package models

import (
	"time"

	"gorm.io/gorm"
)

// Note is a markdown page (or folder) in the Notes knowledge base. Nodes form a
// self-referential tree like Whiteboard: a root folder is a "space". A page may
// have child pages (Confluence-style), so ParentID can point at a page too.
//
// UserID is the OWNER OF THE TREE, inherited from the parent on create — a
// collaborator who adds a page inside someone's shared space doesn't own it;
// CreatedBy / UpdatedBy record who actually wrote it. Version bumps on every
// title/body change and backs optimistic concurrency (PATCH carries
// baseVersion). Soft-deleted so an accidental delete is recoverable.
type Note struct {
	ID          string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string  `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string  `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	ParentID    *string `gorm:"type:varchar(36);index" json:"parentId"`
	Type        string  `gorm:"type:varchar(16);default:'page'" json:"type"` // folder | page | card | hub
	SortOrder   int     `gorm:"default:0" json:"sortOrder"`
	Title       string  `gorm:"type:varchar(255);not null" json:"title"`
	// Slug is the stable path segment (used by Git sync); set once from the
	// title and unique among siblings — renaming the title doesn't change it.
	Slug string `gorm:"type:varchar(200);index" json:"slug"`
	Icon string `gorm:"type:varchar(16)" json:"icon,omitempty"`
	Body string `gorm:"type:text" json:"body"`
	// FrontMatter keeps YAML lines PatchMan doesn't model (from Git), so a
	// round-trip doesn't drop them.
	FrontMatter string `gorm:"type:text" json:"-"`
	Visibility  string `gorm:"type:varchar(20);default:'private'" json:"visibility"`
	Version     int64  `gorm:"default:1" json:"version"`
	// Knowledge-card fields (agent memory; empty on pages and folders). A
	// card is one fact / decision / root cause with a one-line Summary that
	// recall shows instead of the body. Source says who wrote it (human |
	// claude); Claude never changes a human's card without a suggestion.
	Summary      string  `gorm:"type:varchar(255)" json:"summary,omitempty"`
	Source       string  `gorm:"type:varchar(16)" json:"source,omitempty"` // human | agent
	Confidence   float64 `gorm:"default:1" json:"confidence,omitempty"`
	Stale        bool    `gorm:"default:false" json:"stale,omitempty"`
	SupersededBy *string `gorm:"type:varchar(36)" json:"supersededBy,omitempty"`
	// Agent names the AI agent that wrote a card ("Claude Code", "Cursor" —
	// what the MCP client calls itself); empty for a person's.
	Agent string `gorm:"type:varchar(64)" json:"agent,omitempty"`
	// Memory marks a folder as an agent memory folder: remember() files cards
	// in it and the UI lists it as cards. Its scope (personal / team / …)
	// follows from how it is shared.
	Memory bool `gorm:"default:false" json:"memory,omitempty"`
	// Project marks a folder inside a memory folder as one project's cards
	// (the key remember() files them by: a normalized directory/repo name).
	// The folder's title is the shown name and can be renamed freely.
	Project string `gorm:"type:varchar(64);index" json:"project,omitempty"`
	// UseCount is how often recall returned it (decays later); a small
	// ranking bonus. Written only by the recall writer, never bumps Version.
	UseCount  float64        `gorm:"default:0" json:"-"`
	CreatedBy string         `gorm:"type:varchar(36)" json:"createdBy"`
	UpdatedBy string         `gorm:"type:varchar(36)" json:"updatedBy"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Note) TableName() string { return "notes" }

// NoteRevision is the page history. The newest row mirrors the current
// content; consecutive edits by the same author/source within a short window
// coalesce into one row (see note_service.go).
type NoteRevision struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	NoteID       string    `gorm:"type:varchar(36);index:idx_note_rev_note_created,priority:1;not null" json:"noteId"`
	Version      int64     `json:"version"`
	Title        string    `gorm:"type:varchar(255)" json:"title"`
	Body         string    `gorm:"type:text" json:"body,omitempty"`
	AuthorID     string    `gorm:"type:varchar(36)" json:"authorId"`
	Source       string    `gorm:"type:varchar(20)" json:"source"` // edit | mcp | restore | git_pull | git_push | git_conflict
	GitCommitSHA string    `gorm:"type:varchar(64)" json:"gitCommitSha,omitempty"`
	CreatedAt    time.Time `gorm:"index:idx_note_rev_note_created,priority:2" json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (NoteRevision) TableName() string { return "note_revisions" }

// NotePublicGrant mirrors WhiteboardPublicGrant: a share of a public note or
// folder (cascading to its subtree) to a user, a team, or everyone.
type NotePublicGrant struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	NoteID      string    `gorm:"type:varchar(36);index:idx_note_grant,priority:1;not null" json:"noteId"`
	Role        string    `gorm:"type:varchar(20);index:idx_note_grant,priority:2;not null" json:"role"` // editor | viewer
	SubjectKind string    `gorm:"type:varchar(20);not null" json:"subjectKind"`                          // user | team | everyone
	SubjectID   *string   `gorm:"type:varchar(36);index" json:"subjectId,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (NotePublicGrant) TableName() string { return "note_public_grants" }

// NoteAttachment is an image pasted/dropped into a note, referenced from the
// markdown as ![alt](attachment:<id>). Stored in the DB (≤5MB, raster images
// only — never SVG, which can carry script).
type NoteAttachment struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	NoteID    string    `gorm:"type:varchar(36);index;not null" json:"noteId"`
	FileName  string    `gorm:"type:varchar(255)" json:"fileName"`
	Mime      string    `gorm:"type:varchar(64)" json:"mime"`
	Size      int64     `json:"size"`
	SHA256    string    `gorm:"type:varchar(64);index" json:"sha256"`
	Data      []byte    `json:"-"`
	CreatedBy string    `gorm:"type:varchar(36)" json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func (NoteAttachment) TableName() string { return "note_attachments" }

// NoteLink is a typed edge from a note to another note or to a PatchMan
// entity (DstURI, e.g. patchman://request/<id>). Exactly one of DstNoteID /
// DstURI is set; the other is "" (not NULL, so the unique index holds on
// Postgres too). Source "body" marks links derived from the markdown body,
// which are rewritten on every save.
type NoteLink struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	SrcNoteID string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_note_link,priority:1" json:"srcNoteId"`
	DstNoteID string    `gorm:"type:varchar(36);not null;default:'';uniqueIndex:idx_note_link,priority:2;index" json:"dstNoteId,omitempty"`
	DstURI    string    `gorm:"type:varchar(500);not null;default:'';uniqueIndex:idx_note_link,priority:3;index" json:"dstUri,omitempty"`
	Type      string    `gorm:"type:varchar(20);not null;uniqueIndex:idx_note_link,priority:4" json:"type"`
	Weight    float64   `gorm:"default:1" json:"weight"`
	Source    string    `gorm:"type:varchar(16)" json:"source"` // human | claude | body
	CreatedBy string    `gorm:"type:varchar(36)" json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func (NoteLink) TableName() string { return "note_links" }

// NoteSuggestion is a change Claude proposes instead of making it: merging a
// duplicate, linking or superseding a human's note. People accept or reject
// it in the Notes UI.
type NoteSuggestion struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	FolderID  string    `gorm:"type:varchar(36);index" json:"folderId"`
	NoteID    string    `gorm:"type:varchar(36);index" json:"noteId"`  // the note it is about
	Kind      string    `gorm:"type:varchar(16);not null" json:"kind"` // merge | link | supersede | promote
	Payload   string    `gorm:"type:text" json:"payload"`              // JSON
	Status    string    `gorm:"type:varchar(16);default:'open';index" json:"status"`
	CreatedBy string    `gorm:"type:varchar(36)" json:"createdBy"`
	Agent     string    `gorm:"type:varchar(64)" json:"agent,omitempty"` // the agent that proposed it
	DecidedBy string    `gorm:"type:varchar(36)" json:"decidedBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (NoteSuggestion) TableName() string { return "note_suggestions" }

// NoteInbox queues remember() calls: the caller gets an id back at once and
// a single worker turns the text into a card on the backend that holds it.
type NoteInbox struct {
	ID       string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID   string `gorm:"type:varchar(36);index;not null" json:"userId"`
	FolderID string `gorm:"type:varchar(36)" json:"folderId"`
	Text     string `gorm:"type:text" json:"text"`
	Context  string `gorm:"type:text" json:"context,omitempty"`
	Agent    string `gorm:"type:varchar(64)" json:"agent,omitempty"`
	Status   string `gorm:"type:varchar(16);default:'pending';index" json:"status"` // pending | processing | done | failed
	Attempts int    `gorm:"default:0" json:"attempts"`
	NoteID   string `gorm:"type:varchar(36)" json:"noteId,omitempty"` // the card it became (or the one it matched)
	// Outcome: created | duplicate (same text already there) | merge (same
	// title: a merge was suggested) | split (the model made several cards).
	Outcome   string    `gorm:"type:varchar(16)" json:"outcome,omitempty"`
	Error     string    `gorm:"type:text" json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (NoteInbox) TableName() string { return "note_inbox" }

// NoteMemorySettings is a person's agent memory setup: which team memory
// remember(scope=team) writes to when several are shared with them, and the
// model that splits remembered text into cards (none: one card per call).
type NoteMemorySettings struct {
	UserID        string    `gorm:"type:varchar(36);primaryKey" json:"-"`
	TeamFolderID  string    `gorm:"type:varchar(36)" json:"teamFolderId"`
	LLMProviderID string    `gorm:"type:varchar(36)" json:"llmProviderId"`
	LLMModel      string    `gorm:"type:varchar(255)" json:"llmModel"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (NoteMemorySettings) TableName() string { return "note_memory_settings" }
