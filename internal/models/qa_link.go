package models

import "time"

// QAProjectLink ties a Git project to a Jira project for the CR + QA Hub
// (CR_QA_P0_PLAN P0.b): MRs of the project are matched to issues of the Jira
// project by key (internal/qalink). Personal (WorkspaceID nil) or shared with
// a workspace, like the other QA rows.
type QAProjectLink struct {
	ID               string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID           string  `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID      *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	GitConnectionID  string  `gorm:"type:varchar(36);not null" json:"gitConnectionId"`
	GitProject       string  `gorm:"type:varchar(500);not null" json:"gitProject"` // GitLab id / group/repo, GitHub owner/name
	JiraConnectionID string  `gorm:"type:varchar(36)" json:"jiraConnectionId"`
	JiraProjectKey   string  `gorm:"type:varchar(50)" json:"jiraProjectKey"`
	KeyPattern       string  `gorm:"type:varchar(200)" json:"keyPattern"` // "" = PROJECT-123
	TestEnvironment  string  `gorm:"type:varchar(36)" json:"testEnvironmentId"`
	// GitLab webhook secret (SHA-256 hex); the secret itself is shown once.
	WebhookSecretHash string     `gorm:"type:varchar(64)" json:"-"`
	WebhookAt         *time.Time `json:"webhookAt,omitempty"` // last event received
	HasWebhook        bool       `gorm:"-" json:"hasWebhook"`
	// The caller's own connections for this link (QALinkConnectionMap),
	// filled on list; not stored here.
	MyGitConnectionID  string    `gorm:"-" json:"myGitConnectionId,omitempty"`
	MyJiraConnectionID string    `gorm:"-" json:"myJiraConnectionId,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// QAOwner: who owns the link and which workspace shares it.
func (l QAProjectLink) QAOwner() (string, *string) { return l.UserID, l.WorkspaceID }

// QASettings are the MR Radar thresholds of one QA space (CR_QA_P0_PLAN
// P0.c): a workspace, or a user's personal space (WorkspaceID nil). Zero /
// empty values turn their signal off (O6). No gorm defaults: a zero must
// stay zero.
type QASettings struct {
	ID              string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID          string  `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID     *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	SLAHours        int     `json:"slaHours"`
	LargeFiles      int     `json:"largeFiles"`
	SourcePatterns  string  `gorm:"type:text" json:"sourcePatterns"`
	TestPatterns    string  `gorm:"type:text" json:"testPatterns"`
	DisabledSignals string  `gorm:"type:varchar(500)" json:"disabledSignals"` // comma-separated kinds
	TestStatuses    string  `gorm:"type:varchar(500)" json:"testStatuses"`    // Jira status names, comma-separated
	MergedDays      int     `json:"mergedDays"`                               // look-back for merged MRs (0 = 7)
	DigestHour      *int    `json:"digestHour"`
	// ReviewGuide: the team's review rules (conventions, risky areas,
	// severity policy), given to every AI review / fix check (P4.a).
	ReviewGuide string `gorm:"type:text" json:"reviewGuide"`
	// Timezone of DigestHour (IANA); "" = UTC.
	Timezone string `gorm:"type:varchar(64)" json:"timezone"`
	// DigestSentDay: the local day the channel digest last went out.
	DigestSentDay string    `gorm:"type:varchar(10)" json:"-"` // local hour 0–23, nil = off
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (s QASettings) QAOwner() (string, *string) { return s.UserID, s.WorkspaceID }

// QAInboxSnooze hides one Inbox item for a user until a time.
type QAInboxSnooze struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID    string    `gorm:"type:varchar(36);uniqueIndex:idx_qa_snooze_user_ref;not null" json:"userId"`
	ItemRef   string    `gorm:"type:varchar(300);uniqueIndex:idx_qa_snooze_user_ref;not null" json:"itemRef"`
	Until     time.Time `json:"until"`
	CreatedAt time.Time `json:"createdAt"`
}

// QALinkConnectionMap lets one person read a shared QA link through their
// own Git / Jira connection instead of the link's (CR_QA_P0_PLAN P0.c):
// with their own token the radar knows who "me" is, so their reviews,
// stuck MRs and Jira test queue reach their Inbox. Empty field = the
// link's connection.
type QALinkConnectionMap struct {
	ID               string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	LinkID           string    `gorm:"type:varchar(36);uniqueIndex:idx_qa_link_map;not null" json:"linkId"`
	UserID           string    `gorm:"type:varchar(36);uniqueIndex:idx_qa_link_map;not null" json:"userId"`
	GitConnectionID  string    `gorm:"type:varchar(36)" json:"gitConnectionId"`
	JiraConnectionID string    `gorm:"type:varchar(36)" json:"jiraConnectionId"`
	UpdatedAt        time.Time `json:"updatedAt"`
}
