package models

import "time"

// MCPToken is a personal access token used by external MCP clients
// (Claude, MCP Inspector, etc.) to authenticate against the /api/mcp
// Streamable-HTTP endpoint. The raw token is shown to the user exactly
// once at creation time; only its SHA-256 hash is persisted, so a DB
// leak can't be replayed against the MCP surface.
//
// A token maps 1:1 to a User. Every MCP tool call performed with this
// token runs as that user — it can do exactly what the user can do by
// hand, nothing more. Revoking is a hard delete of the row.
type MCPToken struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// Name is a human label set at creation ("Claude Desktop", "laptop").
	Name string `gorm:"type:varchar(255)" json:"name"`
	// TokenHash is the hex SHA-256 of the raw token. Never exposed.
	TokenHash string `gorm:"type:varchar(64);uniqueIndex;not null" json:"-"`
	// Prefix is the first few characters of the raw token, kept so the UI
	// can show "pmcp_ab12…" in the token list without storing the secret.
	Prefix     string     `gorm:"type:varchar(16)" json:"prefix"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	// ClientName is the agent that last connected with this token, as it
	// named itself on MCP initialize ("Claude Code"); shown on what it writes.
	ClientName string    `gorm:"type:varchar(64)" json:"clientName,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	// Scopes restricts which tool GROUPS this token may use (comma-separated
	// group keys — see internal/mcpscope). NULL means "all groups", so every
	// pre-existing token keeps full access after the column is added, and a
	// newly minted token defaults to all tools until the user narrows it.
	// Serialised as `scopes`: null → all, "" → none, "db,http" → those groups.
	Scopes *string `gorm:"type:text" json:"scopes"`
}

func (MCPToken) TableName() string { return "mcp_tokens" }
