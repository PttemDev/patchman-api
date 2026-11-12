package models

import (
	"time"
)

type Comment struct {
	ID         string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID     string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	User       User      `gorm:"foreignKey:UserID" json:"user"`
	TargetType string    `gorm:"type:varchar(50);not null" json:"targetType"` // collection, request, test_case, defect, test_plan
	TargetID   string    `gorm:"type:varchar(36);index;not null" json:"targetId"`
	Content    string    `gorm:"type:text;not null" json:"content"`
	ParentID   *string   `gorm:"type:varchar(36)" json:"parentId"` // for replies
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
