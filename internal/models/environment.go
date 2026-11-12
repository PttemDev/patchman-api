package models

import (
	"time"
)

type Environment struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID is nullable: empty means the env is global (visible in every
	// workspace); a value means the env is scoped to just that workspace.
	// Legacy rows created before this field existed stay global by default.
	WorkspaceID string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string    `gorm:"type:varchar(255);not null" json:"name"`
	Variables   string    `gorm:"type:text" json:"variables"` // JSON [{key, value, enabled, secret}]
	IsActive    bool      `gorm:"default:false" json:"isActive"`
	// Visibility mirrors Collection.Visibility — see that field's comment
	// for the model. Existing rows default to 'private' on AutoMigrate.
	Visibility string    `gorm:"type:varchar(20);not null;default:'private'" json:"visibility"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// EnvironmentPublicGrant is the env-side parity of CollectionPublicGrant
// — same shape, same role enum, same subject-kind enum. See that model's
// comment for the semantics. Secret variables in a public environment
// remain reveal-restricted (only editors/owner can reveal secrets;
// viewers always see them masked).
type EnvironmentPublicGrant struct {
	ID            string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	EnvironmentID string    `gorm:"type:varchar(36);not null;index:idx_epg_env_role" json:"environmentId"`
	Role          string    `gorm:"type:varchar(20);not null;index:idx_epg_env_role" json:"role"`
	SubjectKind   string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID     *string   `gorm:"type:varchar(36);index" json:"subjectId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (EnvironmentPublicGrant) TableName() string { return "environment_public_grants" }
