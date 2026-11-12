package models

import "time"

// Team is a named group of users that can be referenced from a sharing
// grant (collection_public_grants, environment_public_grants). Teams are
// asset-free — they only hold people, not workspaces or collections.
// Any registered user may create a team; the creator is implicitly the
// first admin and is stored both as Team.CreatedBy and as a TeamMember
// row with role='admin' so membership queries stay uniform.
type Team struct {
	ID          string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	Name        string     `gorm:"type:varchar(255);not null" json:"name"`
	Description string     `gorm:"type:text" json:"description"`
	CreatedBy   string     `gorm:"type:varchar(36);not null;index" json:"createdBy"`
	Creator     User       `gorm:"foreignKey:CreatedBy" json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `gorm:"index" json:"-"`
}

// TeamMember pairs a user with a team and assigns a team-internal role
// (admin can manage the roster + rename; member can only see). This is
// the *new* TeamMember — the legacy struct under workspace.go that used
// to live at table `team_members` is dropped by the pre-AutoMigrate
// cleanup in `database.Init` so this model can claim the same table name
// with the new schema.
type TeamMember struct {
	TeamID   string    `gorm:"type:varchar(36);primaryKey" json:"teamId"`
	UserID   string    `gorm:"type:varchar(36);primaryKey;index" json:"userId"`
	Role     string    `gorm:"type:varchar(20);not null;default:'member'" json:"role"` // admin, member
	JoinedAt time.Time `json:"joinedAt"`
	User     User      `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// TableName pins the table so a future struct rename doesn't silently
// migrate to a new table.
func (TeamMember) TableName() string { return "team_members" }
