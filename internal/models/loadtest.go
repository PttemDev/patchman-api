package models

import "time"

type LoadTest struct {
	ID          string     `json:"id" gorm:"primaryKey;type:varchar(36)"`
	UserID      string     `json:"userId" gorm:"type:varchar(36);not null;index"`
	Name        string     `json:"name" gorm:"not null"`
	Description string     `json:"description"`
	Status      string     `json:"status" gorm:"default:'pending'"` // pending, running, completed, failed, cancelled
	Config      string     `json:"config" gorm:"type:text"`         // JSON LoadTestConfig
	Results     string     `json:"results" gorm:"type:text"`        // JSON LoadTestResults
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}
