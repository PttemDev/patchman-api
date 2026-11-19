package models

import "time"

// UserSetting is one per-user preference the backend must enforce (so it
// applies to the app AND to AI agents), stored as JSON under a key — e.g.
// "bigquery" → {maxBytes, askBytes}.
type UserSetting struct {
	UserID    string    `gorm:"type:varchar(36);primaryKey" json:"userId"`
	Key       string    `gorm:"type:varchar(64);primaryKey" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (UserSetting) TableName() string { return "user_settings" }
