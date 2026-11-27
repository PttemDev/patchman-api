package models

import (
	"time"
)

type TestPlan struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string   `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string    `gorm:"type:varchar(500);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	Status      string    `gorm:"type:varchar(20);default:'draft'" json:"status"` // draft/active/completed
	TestCaseIDs string    `gorm:"type:text" json:"testCaseIds"`                   // JSON array of uuids
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type TestRun struct {
	ID         string `gorm:"type:varchar(36);primaryKey" json:"id"`
	TestPlanID string `gorm:"type:varchar(36);index;not null" json:"testPlanId"`
	// TestFolderID is set (and TestPlanID left "") for a "Run all" of a
	// test-case folder. NULL for plan runs and every pre-folder row.
	TestFolderID *string `gorm:"type:varchar(36);index" json:"testFolderId,omitempty"`
	UserID       string  `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Environment string  `gorm:"type:varchar(255)" json:"environment"`
	// CR + QA Hub (P2): the MR ("<linkId>:<iid>") and Jira issue this run tested.
	MRRef       string     `gorm:"type:varchar(80);index" json:"mrRef,omitempty"`
	JiraKey     string     `gorm:"type:varchar(50)" json:"jiraKey,omitempty"`
	Status      string     `gorm:"type:varchar(20);default:'running'" json:"status"` // running/completed
	Results     string     `gorm:"type:text" json:"results"`                         // JSON [{testCaseId, status, notes, duration}]
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r TestPlan) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r TestRun) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }
