package models

import (
	"time"
)

type MockServer struct {
	ID        string      `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID    string      `gorm:"type:varchar(36);index;not null" json:"userId"`
	Name      string      `gorm:"type:varchar(255);not null" json:"name"`
	Prefix    string      `gorm:"type:varchar(255);uniqueIndex;not null" json:"prefix"`
	IsActive  bool        `gorm:"default:true" json:"isActive"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	Routes    []MockRoute `gorm:"foreignKey:MockServerID" json:"routes,omitempty"`
}

type MockRoute struct {
	ID              string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	MockServerID    string    `gorm:"type:varchar(36);index;not null" json:"mockServerId"`
	Method          string    `gorm:"type:varchar(10);not null" json:"method"`
	Path            string    `gorm:"type:varchar(500);not null" json:"path"`
	StatusCode      int       `gorm:"default:200" json:"statusCode"`
	ResponseHeaders string    `gorm:"type:text" json:"responseHeaders"` // JSON
	ResponseBody    string    `gorm:"type:text" json:"responseBody"`
	Delay           int       `gorm:"default:0" json:"delay"` // ms
	IsActive        bool      `gorm:"default:true" json:"isActive"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}
