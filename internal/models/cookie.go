package models

import (
	"time"
)

type CookieJar struct {
	ID        string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID    string     `gorm:"type:varchar(36);index;not null" json:"userId"`
	Domain    string     `gorm:"type:varchar(255);not null" json:"domain"`
	Name      string     `gorm:"type:varchar(255);not null" json:"name"`
	Value     string     `gorm:"type:text" json:"value"`
	Path      string     `gorm:"type:varchar(255);default:'/'" json:"path"`
	Secure    bool       `gorm:"default:false" json:"secure"`
	HttpOnly  bool       `gorm:"default:false" json:"httpOnly"`
	SameSite  string     `gorm:"type:varchar(20)" json:"sameSite"`
	ExpiresAt *time.Time `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}
