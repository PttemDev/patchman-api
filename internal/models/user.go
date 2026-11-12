package models

import (
	"time"
)

type User struct {
	ID       string `gorm:"type:varchar(36);primaryKey" json:"id"`
	Email    string `gorm:"type:varchar(255);uniqueIndex;not null" json:"email"`
	Password string `gorm:"type:varchar(255);not null" json:"-"`
	Name     string `gorm:"type:varchar(255)" json:"name"`
	// Role gates the admin endpoints (user list, role assignment, usage
	// dashboard). The default-admin email is enforced at boot time so the
	// product always has at least one human who can elevate others.
	Role string `gorm:"type:varchar(20);default:'user';not null" json:"role"`
	// SsoProvider + SsoLinkedAt mark the first time an external identity
	// provider was bound to this account. Used by the callback to surface
	// a one-time "we linked your existing account" notice — without this,
	// every SSO sign-in would either trigger the notice (annoying) or
	// hide the merge (confusing on the first time).
	SsoProvider string     `gorm:"type:varchar(32)" json:"ssoProvider,omitempty"`
	SsoLinkedAt *time.Time `json:"ssoLinkedAt,omitempty"`
	// LastLoginAt is stamped on each successful password/SSO login.
	// LastSeenAt / LastSeenVersion track the most recent time the client
	// called /auth/me and the app version it reported (X-Client-Version) —
	// surfaced on the admin dashboard for upgrade planning.
	LastLoginAt     *time.Time `json:"lastLoginAt,omitempty"`
	LastSeenAt      *time.Time `json:"lastSeenAt,omitempty"`
	LastSeenVersion string     `gorm:"type:varchar(32)" json:"lastSeenVersion,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)
