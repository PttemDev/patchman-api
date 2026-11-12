package models

import (
	"time"
)

// History's (user_id, created_at) index serves the admin roster's per-user
// "last activity" (MAX(created_at) WHERE user_id = ? → one index probe).
type History struct {
	ID              string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID          string    `gorm:"type:varchar(36);index;index:idx_histories_user_created,priority:1;not null" json:"userId"`
	Method          string    `gorm:"type:varchar(10);not null" json:"method"`
	URL             string    `gorm:"type:text;not null" json:"url"`
	StatusCode      int       `json:"statusCode"`
	Duration        int64     `json:"duration"` // milliseconds
	RequestHeaders  string    `gorm:"type:text" json:"requestHeaders"`  // JSON
	RequestBody     string    `gorm:"type:text" json:"requestBody"`
	ResponseHeaders string    `gorm:"type:text" json:"responseHeaders"` // JSON
	ResponseBody    string    `gorm:"type:text" json:"responseBody"`
	ResponseSize    int64     `json:"responseSize"`
	CreatedAt       time.Time `gorm:"index:idx_histories_user_created,priority:2" json:"createdAt"`
}
