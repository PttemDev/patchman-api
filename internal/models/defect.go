package models

import (
	"time"
)

type Defect struct {
	ID               string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID           string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	Title            string    `gorm:"type:varchar(500);not null" json:"title"`
	Description      string    `gorm:"type:text" json:"description"`
	Status           string    `gorm:"type:varchar(20);default:'open'" json:"status"`     // open, in_progress, fixed, verified, closed, reopened
	Priority         string    `gorm:"type:varchar(5);default:'P2'" json:"priority"`      // P0, P1, P2, P3, P4
	Severity         string    `gorm:"type:varchar(20);default:'minor'" json:"severity"`  // critical, major, minor, trivial
	AssigneeID       string    `gorm:"type:varchar(36)" json:"assigneeId"`
	TestCaseID       string    `gorm:"type:varchar(36)" json:"testCaseId"`
	TestRunID        string    `gorm:"type:varchar(36)" json:"testRunId"`
	Environment      string    `gorm:"type:varchar(255)" json:"environment"`
	StepsToReproduce string    `gorm:"type:text" json:"stepsToReproduce"`
	Tags             string    `gorm:"type:text" json:"tags"` // JSON array
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}
