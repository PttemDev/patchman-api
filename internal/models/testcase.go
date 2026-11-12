package models

import (
	"time"
)

type TestCase struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// FolderID places the case in a TestCaseFolder; NULL (the default for
	// every pre-folder row) means the root of the Cases list.
	FolderID    *string   `gorm:"type:varchar(36);index" json:"folderId"`
	Title       string    `gorm:"type:varchar(500);not null" json:"title"`
	Description string    `gorm:"type:text" json:"description"`
	Priority    string    `gorm:"type:varchar(5);default:'P2'" json:"priority"`   // P0-P4
	Type        string    `gorm:"type:varchar(20);default:'manual'" json:"type"`  // manual/automated
	Tags        string    `gorm:"type:text" json:"tags"`                          // JSON array
	Steps       string    `gorm:"type:text" json:"steps"`                         // JSON [{stepNo, action, expected}]
	Status      string    `gorm:"type:varchar(20);default:'draft'" json:"status"` // draft/active/deprecated
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TestCaseFolder groups test cases in the QA "Cases" list. Folders nest via
// ParentID (NULL = top level) and are owned by one user, exactly like the
// cases they hold (TestCase access is user_id-scoped; there is no
// workspace/sharing layer on test cases, so none on their folders either).
type TestCaseFolder struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID    string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	ParentID  *string   `gorm:"type:varchar(36);index" json:"parentId"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
