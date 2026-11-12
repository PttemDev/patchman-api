package models

import (
	"time"
)

type SavedRequest struct {
	ID               string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	CollectionID     string    `gorm:"type:varchar(36);index;not null" json:"collectionId"`
	Name             string    `gorm:"type:varchar(255);not null" json:"name"`
	Method           string    `gorm:"type:varchar(10);not null;default:'GET'" json:"method"`
	URL              string    `gorm:"type:text;not null" json:"url"`
	Headers          string    `gorm:"type:text" json:"headers"`           // JSON
	Params           string    `gorm:"type:text" json:"params"`            // JSON
	PathParams       string    `gorm:"type:text" json:"pathParams"`        // JSON — {name} URL path variables + values
	Body             string    `gorm:"type:text" json:"body"`              // JSON {type, content}
	Auth             string    `gorm:"type:text" json:"auth"`              // JSON {type, ...}
	PreRequestScript string    `gorm:"type:text" json:"preRequestScript"`
	TestScript       string    `gorm:"type:text" json:"testScript"`
	SortOrder        int       `gorm:"default:0" json:"sortOrder"`
	// GitItemID carries the upstream Postman `_postman_id` for items that
	// originated from a GitLab-imported collection. It's how the push path
	// identifies which item in the rawSnapshot corresponds to which DB row,
	// so renames/reorders/edits can be applied without losing fields the
	// PatchMan model does not represent.
	GitItemID string    `gorm:"type:varchar(64);index" json:"gitItemId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
