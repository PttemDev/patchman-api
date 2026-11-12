package models

import "time"

// Workspace is owner-scoped, optionally bound to a Team. The legacy
// per-workspace member roster (admin/editor/viewer) is gone — sharing
// asset access still flows through Collection / Environment grants. The
// team binding here is a lightweight "this organizational container is
// visible to every member of team X" signal: the team members see the
// workspace in their selector and can drop their own collections into
// it, but per-collection ACL is untouched. To grant the team access to
// content inside, the owner still uses the collection/environment
// public-grants surface.
//
// See TEAMS_AND_PUBLIC_SHARING_RFC.md for the original design + the
// workspace-team-binding extension above.
type Workspace struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	Name        string    `gorm:"type:varchar(255);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	OwnerID     string    `gorm:"type:varchar(36);not null" json:"ownerId"`
	Owner       User      `gorm:"foreignKey:OwnerID" json:"-"`
	// TeamID nil = personal/unshared workspace. When set, every member
	// of that team sees the workspace in their selector. Read-only
	// reference — the team entity stays the single source of truth
	// for membership; we just point at it.
	TeamID    *string   `gorm:"type:varchar(36);index" json:"teamId,omitempty"`
	Type      string    `gorm:"type:varchar(20);default:'personal'" json:"type"` // 'personal' or 'team' — informational
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
