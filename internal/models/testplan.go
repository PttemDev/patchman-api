package models

import (
	"time"
)

type TestPlan struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	Name        string    `gorm:"type:varchar(500);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	Status      string    `gorm:"type:varchar(20);default:'draft'" json:"status"` // draft/active/completed
	TestCaseIDs string    `gorm:"type:text" json:"testCaseIds"`                    // JSON array of uuids
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type TestRun struct {
	ID          string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	TestPlanID  string     `gorm:"type:varchar(36);index;not null" json:"testPlanId"`
	// TestFolderID is set (and TestPlanID left "") for a "Run all" of a
	// test-case folder. NULL for plan runs and every pre-folder row.
	TestFolderID *string   `gorm:"type:varchar(36);index" json:"testFolderId,omitempty"`
	UserID      string     `gorm:"type:varchar(36);index;not null" json:"userId"`
	Environment string     `gorm:"type:varchar(255)" json:"environment"`
	Status      string     `gorm:"type:varchar(20);default:'running'" json:"status"` // running/completed
	Results     string     `gorm:"type:text" json:"results"`                          // JSON [{testCaseId, status, notes, duration}]
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
}
