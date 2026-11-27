package models

import (
	"time"
)

type SharedStep struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string   `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Title       string    `gorm:"type:varchar(500);not null" json:"title"`
	Steps       string    `gorm:"type:text" json:"steps"`      // JSON array of {stepNo, action, expected}
	UsageCount  int       `gorm:"default:0" json:"usageCount"` // Number of test cases referencing this shared step
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r SharedStep) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }
