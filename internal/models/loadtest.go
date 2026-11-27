package models

import "time"

type LoadTest struct {
	ID     string `json:"id" gorm:"primaryKey;type:varchar(36)"`
	UserID string `json:"userId" gorm:"type:varchar(36);not null;index"`
	// WorkspaceID shares the row with a workspace's team (CR_QA_RFC O5);
	// nil = personal, which is every row created before it existed.
	WorkspaceID *string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
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

// QAOwner: who owns the row and which workspace shares it (handlers/qa_access.go).
func (r LoadTest) QAOwner() (string, *string) { return r.UserID, r.WorkspaceID }
