package models

import "time"

// EnvironmentShare mirrors CollectionShare's two-row pattern:
//
//   - One row with UserID == nil holds the share link itself
//     (token + role) — created when the owner clicks "Share". Anyone
//     with the token can join.
//   - Subsequent rows with UserID != nil track the joined collaborators.
//     Their Role is what the owner assigned.
//
// We mirror collection's shape rather than introducing a generic
// `shares` table now. When a third resource (mock servers, test plans)
// wants the same workflow, that's the right time to refactor.
type EnvironmentShare struct {
	ID            string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	EnvironmentID string    `gorm:"type:varchar(36);index;not null" json:"environmentId"`
	ShareToken    string    `gorm:"type:varchar(64);index;not null" json:"shareToken"`
	UserID        *string   `gorm:"type:varchar(36);index" json:"userId"` // nil = share link row; set = collaborator
	// Role is one of EnvRoleViewer / EnvRoleEditor. Owner is implicit
	// (= environment.user_id) and never appears here. The token-row
	// carries the default role new joiners pick up.
	Role      string    `gorm:"type:varchar(20);default:'viewer'" json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

const (
	EnvRoleViewer = "viewer"
	EnvRoleEditor = "editor"
	EnvRoleOwner  = "owner" // synthetic — never stored, returned in API responses
)

// EnvironmentActivation lets each user pick their own active environment
// per workspace. Replaces Environment.IsActive (which only worked when
// every env was single-owner) so that sharing an env doesn't make the
// joiner's activation flip the owner's, and vice versa.
//
// One row per (UserID, WorkspaceID). Setting "no workspace" ties the
// activation to the global bucket. Activating a different env replaces
// the row instead of inserting a second one.
type EnvironmentActivation struct {
	ID            string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID        string    `gorm:"type:varchar(36);uniqueIndex:idx_user_ws;not null" json:"userId"`
	WorkspaceID   string    `gorm:"type:varchar(36);uniqueIndex:idx_user_ws" json:"workspaceId"`
	EnvironmentID string    `gorm:"type:varchar(36);index;not null" json:"environmentId"`
	UpdatedAt     time.Time `json:"updatedAt"`
}
