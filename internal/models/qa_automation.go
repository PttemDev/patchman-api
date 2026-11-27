package models

import "time"

// QAScheduledRun runs a test folder or plan on a schedule on the server
// (CR_QA_RFC P3, a non-LLM job). Creating it is the approval (O7). The run
// uses its creator's access, checked again at run time.
type QAScheduledRun struct {
	ID            string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID        string     `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID   *string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name          string     `gorm:"type:varchar(200)" json:"name"`
	TargetKind    string     `gorm:"type:varchar(10);not null" json:"targetKind"` // folder | plan
	TargetID      string     `gorm:"type:varchar(36);not null" json:"targetId"`
	EnvironmentID string     `gorm:"type:varchar(36)" json:"environmentId"`
	Days          string     `gorm:"type:varchar(20)" json:"days"` // "1,2,3,4,5" (Mon=1 … Sun=7); "" = every day
	Hour          int        `json:"hour"`
	Minute        int        `json:"minute"`
	Timezone      string     `gorm:"type:varchar(64)" json:"timezone"` // IANA, e.g. Europe/Istanbul
	Enabled       bool       `json:"enabled"`
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastRunID     string     `gorm:"type:varchar(36)" json:"lastRunId,omitempty"`
	LastStatus    string     `gorm:"type:varchar(20)" json:"lastStatus,omitempty"`
	LastError     string     `gorm:"type:text" json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

func (s QAScheduledRun) QAOwner() (string, *string) { return s.UserID, s.WorkspaceID }

// QARadarSnapshot is one day's team-level radar numbers of a QA space (for
// Reports): counts only — no MR titles, no per-person data. Taken when a
// member's radar is computed, at most once a day per space.
type QARadarSnapshot struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	SpaceKey    string    `gorm:"type:varchar(80);uniqueIndex:idx_qa_snap_day;not null" json:"spaceKey"` // ws:<id> | user:<id>
	Day         string    `gorm:"type:varchar(10);uniqueIndex:idx_qa_snap_day;not null" json:"day"`      // YYYY-MM-DD (UTC)
	OpenMRs     int       `json:"openMrs"`
	SignalsJSON string    `gorm:"type:text" json:"-"` // {"stuck":3,…}
	Merged      int       `json:"merged"`             // merged in the look-back window
	LeadMedianH *float64  `json:"leadMedianH"`
	LeadP90H    *float64  `json:"leadP90H"`
	CreatedAt   time.Time `json:"createdAt"`
}

// QAChannel is a Slack / Microsoft Teams incoming webhook of a QA space
// (O4): GitLab webhook events and the daily digest are posted there.
type QAChannel struct {
	ID          string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string     `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID *string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Kind        string     `gorm:"type:varchar(10);not null" json:"kind"` // slack | teams
	Name        string     `gorm:"type:varchar(200)" json:"name"`
	URLEnc      string     `gorm:"type:text" json:"-"`
	Events      string     `gorm:"type:varchar(300)" json:"events"` // comma: mr_opened,pipeline_failed,mr_approved,mr_merged,digest
	Enabled     bool       `json:"enabled"`
	LastError   string     `gorm:"type:text" json:"lastError,omitempty"`
	LastSentAt  *time.Time `json:"lastSentAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

func (c QAChannel) QAOwner() (string, *string) { return c.UserID, c.WorkspaceID }

// QASetupProposal is a CR + QA setup change an AI agent asked for over MCP
// when the client couldn't ask the user directly. It waits until the user
// applies or rejects it in the Hub; applying replays the stored request as
// the user. The request (which may carry a token or webhook URL) is
// encrypted.
type QASetupProposal struct {
	ID          string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID *string   `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Agent       string    `gorm:"type:varchar(60)" json:"agent"`
	Tool        string    `gorm:"type:varchar(60)" json:"tool"`
	Summary     string    `gorm:"type:text" json:"summary"`
	RequestEnc  string    `gorm:"type:text" json:"-"`                   // {method, path, query, body}
	Status      string    `gorm:"type:varchar(20);index" json:"status"` // pending | applied | rejected
	Result      string    `gorm:"type:text" json:"result,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
