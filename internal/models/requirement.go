package models

import (
	"time"
)

type Requirement struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	Title       string    `gorm:"type:varchar(500);not null" json:"title"`
	Description string    `gorm:"type:text" json:"description"`
	Type        string    `gorm:"type:varchar(20);default:'functional'" json:"type"`    // functional, non_functional, user_story, epic
	Priority    string    `gorm:"type:varchar(10);default:'medium'" json:"priority"`    // critical, high, medium, low
	Status      string    `gorm:"type:varchar(20);default:'draft'" json:"status"`       // draft, approved, implemented, deprecated
	ParentID    *string   `gorm:"type:varchar(36)" json:"parentId"`                     // Self-referencing for hierarchy
	ExternalRef string    `gorm:"type:varchar(255)" json:"externalRef"`                 // Jira/Linear reference
	Tags        string    `gorm:"type:text" json:"tags"`                                // JSON array
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type RequirementCoverage struct {
	ID            string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	RequirementID string    `gorm:"type:varchar(36);index;not null" json:"requirementId"`
	TestCaseID    string    `gorm:"type:varchar(36);index;not null" json:"testCaseId"`
	CreatedAt     time.Time `json:"createdAt"`
}
