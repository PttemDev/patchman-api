package models

import (
	"time"
)

type TestCaseVersion struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	TestCaseID  string    `gorm:"type:varchar(36);index;not null" json:"testCaseId"`
	Version     int       `gorm:"not null" json:"version"`
	Title       string    `gorm:"type:varchar(500)" json:"title"`
	Description string    `gorm:"type:text" json:"description"`
	Steps       string    `gorm:"type:text" json:"steps"`
	Priority    string    `gorm:"type:varchar(5)" json:"priority"`
	Tags        string    `gorm:"type:text" json:"tags"`
	ChangedBy   string    `gorm:"type:varchar(36);not null" json:"changedBy"`
	ChangeNote  string    `gorm:"type:varchar(500)" json:"changeNote"`
	CreatedAt   time.Time `json:"createdAt"`
}
