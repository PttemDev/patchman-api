package models

import "time"

// GraylogConnection is a saved Graylog server. PatchMan reads it through
// the Graylog REST API (<url>/api): streams, log search (with a message
// count histogram) and the dashboards / saved searches list.
//
// AuthKind:
//   - "token": a Graylog access token (sent as Basic <token>:token);
//   - "basic": username + password (Basic).
//
// The token / password is AES-256-GCM encrypted (services.TokenCipher) and
// never returned by a read path. TLS knobs live in OptionsJSON. Stateless
// credentials → dual-written like the other tools.
type GraylogConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Web URL of the Graylog server, e.g. https://graylog.example.com (the
	// API is under /api).
	URL      string `gorm:"type:text;not null" json:"url"`
	AuthKind string `gorm:"type:varchar(10);not null;default:'token'" json:"authKind"`
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`
	// Encrypted access token (token) or password (basic). JSON-hidden.
	SecretEnc string `gorm:"type:text" json:"-"`
	// insecureSkipVerify, caCert (PEM).
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (GraylogConnection) TableName() string { return "graylog_connections" }

// GraylogConnectionGrant — delegated access on a Graylog connection. Graylog
// is read-only in PatchMan, so role='read' and role='write' both read; the
// role is kept for parity with the other tools' sharing UI.
type GraylogConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (GraylogConnectionGrant) TableName() string { return "graylog_connection_grants" }

// GraylogActivityHistory audits searches (who searched what, on whose
// connection) — log data can be sensitive.
type GraylogActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Target         string    `gorm:"type:varchar(1024)" json:"target,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (GraylogActivityHistory) TableName() string { return "graylog_activity_history" }
