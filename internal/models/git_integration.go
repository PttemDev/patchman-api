package models

import "time"

// UserGitIntegration links a PatchMan user to an external Git provider account
// (currently only "gitlab"). A user has at most one active integration per
// provider; reconnecting overwrites the row.
//
// Token columns hold ciphertext, not plaintext. The handler encrypts/decrypts
// with AES-GCM using GITLAB_TOKEN_ENC_KEY. See services/git_crypto.go.
type UserGitIntegration struct {
	UserID   string `gorm:"type:varchar(36);primaryKey" json:"userId"`
	Provider string `gorm:"type:varchar(32);primaryKey;default:'gitlab'" json:"provider"`

	// Public-ish identity surfaced back to the user so they know which
	// account they linked. Safe to expose in JSON.
	GitLabUserID   int64  `json:"gitlabUserId"`
	GitLabUsername string `gorm:"type:varchar(255)" json:"gitlabUsername"`
	GitLabName     string `gorm:"type:varchar(255)" json:"gitlabName"`
	GitLabAvatar   string `gorm:"type:text" json:"gitlabAvatar"`

	// Encrypted tokens (AES-GCM ciphertext, base64). Never returned in JSON.
	AccessTokenEnc  string `gorm:"type:text;not null" json:"-"`
	RefreshTokenEnc string `gorm:"type:text;not null" json:"-"`

	// Authoritative expiry from GitLab (created_at + expires_in).
	ExpiresAt time.Time `json:"expiresAt"`

	// Granted scopes — used to detect partial consents and re-prompt.
	Scopes string `gorm:"type:text" json:"scopes"`

	LinkedAt        time.Time `json:"linkedAt"`
	LastRefreshedAt time.Time `json:"lastRefreshedAt"`
}

// TableName overrides GORM's default pluralisation.
func (UserGitIntegration) TableName() string { return "user_git_integrations" }

// CollectionGitSource binds a Collection to a remote git file. Created by the
// "Import from GitLab" flow and used by Push to know where to write back.
//
// rawSnapshot holds the byte-original Postman JSON at fetch time so the push
// path can apply targeted mutations without losing fields PatchMan does not
// model. See GIT_COLLECTION_SYNC_SPEC.md §1.
type CollectionGitSource struct {
	CollectionID string `gorm:"type:varchar(36);primaryKey" json:"collectionId"`
	Provider     string `gorm:"type:varchar(32);default:'gitlab'" json:"provider"`

	// Where to read/write. project_id is whatever GitLab API accepts —
	// numeric ID or url-encoded "namespace/repo".
	BaseURL   string `gorm:"type:text" json:"baseUrl"`
	ProjectID string `gorm:"type:varchar(255)" json:"projectId"`
	FilePath  string `gorm:"type:text" json:"filePath"`
	Branch    string `gorm:"type:varchar(255)" json:"branch"`

	// SHAs we last observed — used to detect remote-side moves on push.
	LastSeenCommitSha string `gorm:"type:varchar(64)" json:"lastSeenCommitSha"`
	LastSeenBlobSha   string `gorm:"type:varchar(64)" json:"lastSeenBlobSha"`

	// Original JSON, byte-for-byte. Stored verbatim so push can patch it.
	RawSnapshot string `gorm:"type:text" json:"-"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (CollectionGitSource) TableName() string { return "collection_git_sources" }
