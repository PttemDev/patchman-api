package models

import (
	"time"
)

type SharedStep struct {
	ID         string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID     string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	Title      string    `gorm:"type:varchar(500);not null" json:"title"`
	Steps      string    `gorm:"type:text" json:"steps"`      // JSON array of {stepNo, action, expected}
	UsageCount int       `gorm:"default:0" json:"usageCount"` // Number of test cases referencing this shared step
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
