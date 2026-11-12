package models

import "time"

// GitConnection is a saved Git provider credential set (v1: GitLab). It rides
// the same connection-spine as the other tools (DB/Kafka/S3/Redis/ES/K8s): the
// secret material (a PAT / token / SSH key, present only when authKind != oauth)
// is stored AES-256-GCM-encrypted (services.TokenCipher) and never returned by
// any read path — the owner-only reveal endpoint is the sole plaintext egress
// and it is audit-logged.
//
// Connections dual-write across BEs like the other tools (FE-generated UUID;
// each BE re-encrypts the secret with its own cipher — the ciphertext itself is
// NOT dual-written). Remote reads authenticate as the CALLER's linked GitLab
// account (see git_remote.go), not the connection owner. Local working-tree ops
// (git_ops.go) are embedded-only.
type GitConnection struct {
	ID            string    `gorm:"primaryKey;type:varchar(36)" json:"id"`       // FE-generated UUID
	UserID        string    `gorm:"type:varchar(36);index" json:"userId"`
	Name          string    `gorm:"type:varchar(200)" json:"name"`
	Provider      string    `gorm:"type:varchar(32)" json:"provider"`            // "gitlab"
	BaseURL       string    `gorm:"type:varchar(500)" json:"baseUrl"`
	AuthKind      string    `gorm:"type:varchar(24)" json:"authKind"`            // "oauth" | "pat" | "ssh"
	CredentialEnc string    `gorm:"type:text" json:"-"`                          // ciphered PAT/token/SSH (authKind != oauth)
	OptionsJSON   string    `gorm:"type:text" json:"-"`
	PrimaryScope  string    `gorm:"type:varchar(16);default:remote" json:"primaryScope"` // 'local'|'remote'
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (GitConnection) TableName() string { return "git_connections" }

// GitConnectionGrant — delegated access on a Git connection. role='read' covers
// the browse/read surface; role='write' covers push/commit. The primary key is
// the (connection, subject) triple — a subject holds at most one grant per
// connection, and re-granting upserts the role.
type GitConnectionGrant struct {
	ConnectionID string `gorm:"primaryKey;type:varchar(36)" json:"connectionId"`
	SubjectKind  string `gorm:"primaryKey;type:varchar(8)" json:"subjectKind"` // 'user'|'team'
	SubjectID    string `gorm:"primaryKey;type:varchar(36)" json:"subjectId"`
	Role         string `gorm:"type:varchar(8)" json:"role"` // 'read'|'write'
}

func (GitConnectionGrant) TableName() string { return "git_connection_grants" }

// GitRepo is a managed local clone (desktop only) — one blobless working tree
// under the app-data dir, keyed by an FE-generated UUID and bound to a
// connection + GitLab project.
type GitRepo struct {
	ID            string     `gorm:"primaryKey;type:varchar(36)" json:"id"` // FE-generated UUID
	ConnectionID  string     `gorm:"type:varchar(36);index" json:"connectionId"`
	UserID        string     `gorm:"type:varchar(36);index" json:"userId"`
	ProjectRef    string     `gorm:"type:varchar(500)" json:"projectRef"` // gitlab project id or path
	ProjectName   string     `gorm:"type:varchar(300)" json:"projectName"`
	RemoteURL     string     `gorm:"type:varchar(600)" json:"remoteUrl"` // https clone url
	LocalPath     string     `gorm:"type:text" json:"localPath"`
	DefaultBranch string     `gorm:"type:varchar(255)" json:"defaultBranch"`
	CloneKind     string     `gorm:"type:varchar(16);default:blobless" json:"cloneKind"`
	LastFetchedAt *time.Time `json:"lastFetchedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

func (GitRepo) TableName() string { return "git_repos" }

// GitActivityHistory is the audit ledger for every git op (remote read, reveal,
// clone, commit, push, ...). Mirrors K8sActivityHistory. The explicit
// TableName keeps GORM from pluralising to git_activity_histories — Wave 2's
// contract audits into git_activity_history.
type GitActivityHistory struct {
	ID           string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index" json:"connectionId"`
	UserID       string    `gorm:"type:varchar(36)" json:"userId"`
	Op           string    `gorm:"type:varchar(32)" json:"op"`
	DetailJSON   string    `gorm:"type:text" json:"detail"`
	OK           bool      `json:"ok"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (GitActivityHistory) TableName() string { return "git_activity_history" }
