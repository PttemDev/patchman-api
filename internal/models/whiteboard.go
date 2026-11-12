package models

import "time"

// Whiteboard is a per-user Excalidraw scene. The drawing itself (elements +
// appState + embedded files) is stored verbatim as a JSON string in SceneJSON;
// the backend never interprets it. No secrets, so no cipher — SceneJSON is a
// plain text column like ESConnection.Nodes / SavedDBQuery.QueryText.
type Whiteboard struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	// ParentID nests boards/folders into a self-referential tree, exactly like
	// Collection.ParentID — NULL = root. Type is "whiteboard" or "folder"; a
	// folder carries no scene, only children. SortOrder gives sibling ordering
	// (drag-drop persists it). Mirrors Collection's foldering fields.
	ParentID  *string   `gorm:"type:varchar(36);index" json:"parentId"`
	Type      string    `gorm:"type:varchar(20);default:'whiteboard'" json:"type"`
	SortOrder int       `gorm:"default:0" json:"sortOrder"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	SceneJSON string    `gorm:"type:text;default:'{}'" json:"scene"`
	// Visibility gates sharing exactly like Collection.Visibility: 'private'
	// (owner only) or 'public' (grants apply). Shared on the node it's set on;
	// descendants inherit via the role resolver walking up ParentID.
	Visibility string    `gorm:"type:varchar(20);default:'private'" json:"visibility"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (Whiteboard) TableName() string { return "whiteboards" }

// WhiteboardPublicGrant mirrors CollectionPublicGrant: a share of a public
// whiteboard/folder to a specific user, a team, or everyone (signed-in), at
// viewer or editor role. Gated on the parent Whiteboard.Visibility='public'.
type WhiteboardPublicGrant struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	WhiteboardID string   `gorm:"type:varchar(36);index:idx_wpg_board_role" json:"whiteboardId"`
	Role        string    `gorm:"type:varchar(20);index:idx_wpg_board_role" json:"role"`        // 'editor' | 'viewer'
	SubjectKind string    `gorm:"type:varchar(20)" json:"subjectKind"`                          // 'user' | 'team' | 'everyone'
	SubjectID   *string   `gorm:"type:varchar(36);index" json:"subjectId,omitempty"`            // NULL for everyone
	CreatedAt   time.Time `json:"createdAt"`
}

func (WhiteboardPublicGrant) TableName() string { return "whiteboard_public_grants" }

// WhiteboardPublicLink is a NO-ACCOUNT share link: anyone holding the token can
// open the board without signing in. Only the SHA-256 hash of the token is
// stored (like MCPToken); the plaintext is shown once at creation. Role is
// 'viewer' (read-only) or 'editor' (anonymous read-write — a real abuse surface,
// so it's opt-in per link). Revocable by deleting the row.
type WhiteboardPublicLink struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	WhiteboardID string    `gorm:"type:varchar(36);index" json:"whiteboardId"`
	UserID       string    `gorm:"type:varchar(36);index;not null" json:"userId"` // link creator (owner)
	TokenHash    string    `gorm:"type:varchar(64);uniqueIndex" json:"-"`         // sha256 hex, for anon resolve
	// Token is the plaintext link token, kept so the OWNER can re-copy a link
	// made earlier (a share link is a capability URL meant to be copied
	// repeatedly, like a GitHub/Notion share link — not a write-once secret).
	// Returned ONLY to the owner via ListLinks; never on the anonymous path.
	Token        string    `gorm:"type:varchar(64)" json:"-"`
	Prefix       string    `gorm:"type:varchar(16)" json:"prefix"`                // first chars, for display
	Role         string    `gorm:"type:varchar(20)" json:"role"`                  // 'viewer' | 'editor'
	CreatedAt    time.Time `json:"createdAt"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
}

func (WhiteboardPublicLink) TableName() string { return "whiteboard_public_links" }

// WhiteboardDiagram is a Mermaid diagram QUEUED for a board (by an MCP client's
// draw_on_whiteboard tool). The BE can't render Mermaid → Excalidraw (that's a
// browser-only conversion), so the tool enqueues here and the FE drains + draws
// it onto the scene the next time the board is open. Rows are deleted on drain.
type WhiteboardDiagram struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	WhiteboardID string    `gorm:"type:varchar(36);index" json:"whiteboardId"`
	Mermaid      string    `gorm:"type:text" json:"mermaid"`
	Title        string    `gorm:"type:varchar(255)" json:"title,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (WhiteboardDiagram) TableName() string { return "whiteboard_diagrams" }
