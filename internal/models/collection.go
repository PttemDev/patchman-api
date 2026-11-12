package models

import (
	"time"
)

type Collection struct {
	ID          string       `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string       `gorm:"type:varchar(36);index;not null" json:"userId"`
	// WorkspaceID is nullable so legacy collections (created before the
	// workspace bridge) stay readable; new rows always carry a workspace.
	WorkspaceID string       `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string       `gorm:"type:varchar(255);not null" json:"name"`
	Description string       `gorm:"type:text" json:"description"`
	ParentID    *string      `gorm:"type:varchar(36);index" json:"parentId"`
	Type        string       `gorm:"type:varchar(20);default:'collection'" json:"type"` // collection or folder
	// Visibility controls whether non-owner users can discover this
	// collection. 'private' means only the owner + explicit CollectionShare
	// rows; 'public' opens it up to the editors/viewers grant lists in
	// collection_public_grants. Defaults to 'private' so AutoMigrate of
	// existing rows keeps current behavior. Top-level only — child
	// folders inherit their root collection's visibility, the column is
	// still stamped but unused for ParentID != nil.
	Visibility  string       `gorm:"type:varchar(20);not null;default:'private'" json:"visibility"`
	BaseURL     string       `gorm:"type:text" json:"baseUrl,omitempty"`                // base URL prepended to relative request paths
	SourceURL   string       `gorm:"type:text" json:"sourceUrl,omitempty"`              // original import URL (swagger/openapi)
	SourceType  string       `gorm:"type:varchar(20)" json:"sourceType,omitempty"`      // swagger, postman, json, ""
	LastSynced  *time.Time   `json:"lastSynced,omitempty"`
	SortOrder   int          `gorm:"default:0" json:"sortOrder"`
	// GitItemID — Postman `_postman_id` of the corresponding folder/root in
	// a GitLab-imported collection. Lets the push path match folders across
	// roundtrips so unmodelled fields (event, variable) survive.
	GitItemID string         `gorm:"type:varchar(64);index" json:"gitItemId,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Children  []Collection   `gorm:"foreignKey:ParentID" json:"children,omitempty"`
	Requests  []SavedRequest `gorm:"foreignKey:CollectionID" json:"requests,omitempty"`
}

type CollectionShare struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	CollectionID string    `gorm:"type:varchar(36);index;not null" json:"collectionId"`
	ShareToken   string    `gorm:"type:varchar(64);index;not null" json:"shareToken"`
	UserID       *string   `gorm:"type:varchar(36);index" json:"userId"`    // nil = the share link row; set = collaborator
	Role         string    `gorm:"type:varchar(20);default:'editor'" json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CollectionPublicGrant rows back the "Editors" / "Viewers" lists on a
// public collection. One row per (collection, role, subject). Subject is
// keyed by `subject_kind` + `subject_id`:
//
//	subject_kind='user'      → subject_id = users.id
//	subject_kind='team'      → subject_id = teams.id
//	subject_kind='everyone'  → subject_id = NULL (singleton)
//
// resolveCollectionRole consults this table after the owner check and
// the legacy CollectionShare path; the highest-matching role wins
// (editor > viewer). Only rows on a Collection with Visibility='public'
// are honored; flipping a collection back to private deletes all of its
// grant rows in the same transaction (see PATCH /visibility handler).
type CollectionPublicGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	CollectionID string    `gorm:"type:varchar(36);not null;index:idx_cpg_collection_role" json:"collectionId"`
	Role         string    `gorm:"type:varchar(20);not null;index:idx_cpg_collection_role" json:"role"`        // editor, viewer
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`                               // user, team, everyone
	SubjectID    *string   `gorm:"type:varchar(36);index" json:"subjectId,omitempty"`                          // NULL when subjectKind='everyone'
	CreatedAt    time.Time `json:"createdAt"`
}

func (CollectionPublicGrant) TableName() string { return "collection_public_grants" }
