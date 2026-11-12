package models

import (
	"time"
)

type JUnitImportResult struct {
	ID         string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID     string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	TestPlanID string    `gorm:"type:varchar(36);index" json:"testPlanId"`
	FileName   string    `gorm:"type:varchar(500)" json:"fileName"`
	TotalTests int       `json:"totalTests"`
	Passed     int       `json:"passed"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	Errors     int       `json:"errors"`
	Duration   float64   `json:"duration"`
	Suites     string    `gorm:"type:text" json:"suites"` // JSON detail of parsed suites
	ImportedAt time.Time `json:"importedAt"`
	CreatedAt  time.Time `json:"createdAt"`
}
