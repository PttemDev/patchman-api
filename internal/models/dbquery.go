package models

import "time"

// DBConnection is a saved credential set for a remote database. The
// password is stored AES-256-GCM-encrypted in `password_enc`; the
// plaintext is only decrypted at the very last moment when handing the
// DSN to the driver. All read paths strip / mask the field — the
// dedicated `POST /api/db-connections/:id/reveal-password` endpoint is
// the only way to surface plaintext, and that hop is audit-logged.
//
// `kind` decides which driver opens the session: 'postgres', 'mysql',
// or 'mongo'. Kind-specific knobs (sslmode, applicationName,
// authSource, tls config) live in `options_json` so we can extend
// per-driver without churning the schema.
type DBConnection struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string    `gorm:"type:varchar(255);not null" json:"name"`
	Kind        string    `gorm:"type:varchar(20);not null" json:"kind"`
	Host        string    `gorm:"type:varchar(255);not null" json:"host"`
	Port        int       `gorm:"not null" json:"port"`
	Database    string    `gorm:"type:varchar(255);not null" json:"database"`
	Username    string    `gorm:"type:varchar(255);not null" json:"username"`
	// Encrypted password ciphertext, base64(nonce || ciphertext || tag)
	// per services.TokenCipher. Hidden from JSON entirely so a careless
	// Preload + json.Marshal never leaks it.
	PasswordEnc string `gorm:"type:text;not null" json:"-"`
	// ConnectionStringEnc lets power users override the auto-built DSN.
	// Encrypted the same way as PasswordEnc since it usually carries the
	// password inline (postgres://user:pass@host:port/db?...). Empty
	// means "build DSN from Host/Port/Username/Password/Database".
	ConnectionStringEnc string `gorm:"type:text" json:"-"`
	// JSON blob for kind-specific options (sslmode, applicationName,
	// authMechanism, etc.). Stored as text so SQLite + Postgres can
	// share the column type.
	OptionsJSON string `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	// PrimaryScope is the user-chosen default backend ('local' | 'remote')
	// for this connection. Both BEs hold the same value via the FE's
	// dual-write so the toolbar Local/Remote toggle defaults consistently
	// across machines. Empty string = unknown (legacy rows pre-migration);
	// the FE falls back to 'remote' in that case.
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	// AllowWrite is the per-connection master switch for DML. When false
	// (the default) the execute path refuses every write/DDL statement for
	// EVERYONE — owner and co-owner included — so a connection behaves
	// exactly as it did in v1. When true, a well-formed write statement is
	// permitted, but only for callers who can manage the connection (owner
	// or a 'write' co-owner grant); read-grantees can never write. The
	// structural / `;`-injection guards in ValidateReadOnly still apply
	// regardless of this flag. AutoMigrate adds the column with default 0.
	AllowWrite bool      `gorm:"not null;default:false" json:"allowWrite"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// DBConnectionGrant — delegated access on a connection. `role='read'`
// grants query-only (SELECT/find) access; `role='write'` grants full
// co-owner access (edit / delete / reveal-password / manage-grants, and
// write queries when the connection's AllowWrite switch is on). The
// `granted_by` column doubles as audit trail metadata.
type DBConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"` // 'user' | 'team'
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (DBConnectionGrant) TableName() string { return "db_connection_grants" }

// DBQueryHistory is the audit + recall ledger for every executed
// query. `delegated_owner` is set when the query ran against a
// connection the caller doesn't own — populated with the connection's
// owner so admins can ask "what queries went through Alice's
// credentials, and who fired them?" in a single index scan. `error` is
// non-null when the driver returned a failure; the row still inserts so
// the audit trail survives both happy + sad paths.
type DBQueryHistory struct {
	ID             string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string  `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string  `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Kind           string  `gorm:"type:varchar(20);not null" json:"kind"`
	QueryText      string  `gorm:"type:text;not null" json:"queryText"`
	// First 2KB of QueryText — used by listing endpoints so the
	// history dropdown doesn't pull megabytes of stored SELECTs.
	QueryExcerpt  string    `gorm:"type:text;not null" json:"queryExcerpt"`
	RowCount      int       `gorm:"default:0" json:"rowCount"`
	ExecMs        int       `gorm:"default:0" json:"execMs"`
	RowsTruncated bool      `gorm:"default:false" json:"rowsTruncated"`
	ErrorText     string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt    time.Time `gorm:"index" json:"executedAt"`
}

func (DBQueryHistory) TableName() string { return "db_query_history" }

// SavedDBQuery is a named query stored against a specific connection
// so power users can re-run their daily-summary / refund-check style
// snippets without retyping. Workspace-scoped + owner-bound today;
// team-share support is on the v2 roadmap (mirror Collection grants).
type SavedDBQuery struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID       string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID  string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Name         string    `gorm:"type:varchar(255);not null" json:"name"`
	QueryText    string    `gorm:"type:text;not null" json:"queryText"`
	// Folder path like "reports/daily" — slashes denote nesting in the
	// FE tree. Empty string = root. Kept as a plain string so a future
	// rename only touches the children of a folder, not row identities.
	Folder string `gorm:"type:varchar(255);default:''" json:"folder"`
	// Comma-separated tags (Postgres doesn't get an array type in shared
	// SQLite/PG sources). FE splits / joins.
	Tags      string    `gorm:"type:text;default:''" json:"tags"`
	SortOrder int       `gorm:"default:0" json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (SavedDBQuery) TableName() string { return "saved_db_queries" }
