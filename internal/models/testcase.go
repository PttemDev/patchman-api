package models

import (
	"time"
)

type TestCase struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	// FolderID places the case in a TestCaseFolder; NULL (the default for
	// every pre-folder row) means the root of the Cases list.
	FolderID    *string `gorm:"type:varchar(36);index" json:"folderId"`
	Title       string  `gorm:"type:varchar(500);not null" json:"title"`
	Description string  `gorm:"type:text" json:"description"`
	Priority    string  `gorm:"type:varchar(5);default:'P2'" json:"priority"`      // P0-P4
	Type        string  `gorm:"type:varchar(20);default:'functional'" json:"type"` // functional/integration/performance/security
	Tags        string  `gorm:"type:text" json:"tags"`                             // comma-separated
	// CR + QA Hub links (P2): the Jira issue and the MR ("<linkId>:<iid>")
	// this case tests. Empty = not linked.
	JiraKey   string    `gorm:"type:varchar(50);index" json:"jiraKey,omitempty"`
	MRRef     string    `gorm:"type:varchar(80);index" json:"mrRef,omitempty"`
	Steps     string    `gorm:"type:text" json:"steps"`                         // JSON [{stepNo, type, …}]; untyped = manual step
	Status    string    `gorm:"type:varchar(20);default:'draft'" json:"status"` // draft/active/deprecated
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TestCaseFolder groups test cases in the QA "Cases" list. Folders nest via
// ParentID (NULL = top level) and are owned by one user, exactly like the
// cases they hold (TestCase access is user_id-scoped; there is no
// workspace/sharing layer on test cases, so none on their folders either).
type TestCaseFolder struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string   `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	ParentID    *string   `gorm:"type:varchar(36);index" json:"parentId"`
	Name        string    `gorm:"type:varchar(255);not null" json:"name"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r TestCase) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r TestCaseFolder) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }
