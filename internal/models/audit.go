package models

import (
	"time"
)

// AuditLog's (user_id, created_at) index serves the admin roster's
// per-user "last activity" (MAX(created_at) WHERE user_id = ? → one index
// probe) and the per-user usage scan.
type AuditLog struct {
	ID         string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID     string    `gorm:"type:varchar(36);index;index:idx_audit_logs_user_created,priority:1;not null" json:"userId"`
	Action     string    `gorm:"type:varchar(50);not null" json:"action"`   // create, update, delete, login, logout, export, import
	Resource   string    `gorm:"type:varchar(50);not null" json:"resource"` // collection, request, environment, test_case, etc.
	ResourceID string    `gorm:"type:varchar(36)" json:"resourceId"`
	Details    string    `gorm:"type:text" json:"details"` // JSON with old/new values
	IPAddress  string    `gorm:"type:varchar(45)" json:"ipAddress"`
	UserAgent  string    `gorm:"type:varchar(500)" json:"userAgent"`
	CreatedAt  time.Time `gorm:"index:idx_audit_logs_user_created,priority:2" json:"createdAt"`
}

type ActivityFeed struct {
	ID         string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID     string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	User       User      `gorm:"foreignKey:UserID" json:"user"`
	Action     string    `gorm:"type:varchar(50);not null" json:"action"`
	Resource   string    `gorm:"type:varchar(50);not null" json:"resource"`
	ResourceID string    `gorm:"type:varchar(36)" json:"resourceId"`
	Summary    string    `gorm:"type:varchar(500)" json:"summary"`
	CreatedAt  time.Time `json:"createdAt"`
}
