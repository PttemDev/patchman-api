package models

import "time"

// JiraConnection is a saved Jira site (CR_QA_P0_PLAN.md P0.b / O1).
// PatchMan only reads it in P0: issues, search, fields.
//
// Flavor / AuthKind:
//   - "cloud" + "email_token": Basic email:apiToken, REST /rest/api/3;
//   - "datacenter" + "pat": Bearer personal access token, REST /rest/api/2.
//
// The API token / PAT is AES-256-GCM encrypted (services.TokenCipher) and
// never returned by a read path. OptionsJSON holds acFieldId (the
// acceptance-criteria custom field) and defaultJql. Stateless credentials →
// dual-written like the other tools (the FE generates the id).
type JiraConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Site URL, e.g. https://acme.atlassian.net or https://jira.example.com
	// (Data Center may have a context path).
	BaseURL  string `gorm:"type:text;not null" json:"baseUrl"`
	Flavor   string `gorm:"type:varchar(20);not null;default:'cloud'" json:"flavor"`
	AuthKind string `gorm:"type:varchar(20);not null;default:'email_token'" json:"authKind"`
	// Account email (Cloud only).
	Email string `gorm:"type:varchar(255)" json:"email,omitempty"`
	// Encrypted API token (Cloud) or PAT (Data Center). JSON-hidden.
	CredentialEnc string    `gorm:"type:text" json:"-"`
	OptionsJSON   string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope  string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (JiraConnection) TableName() string { return "jira_connections" }

// JiraConnectionGrant — delegated access on a Jira connection. PatchMan only
// reads Jira in P0, so role='read' and role='write' both read; the role is
// kept for parity with the other tools' sharing UI.
type JiraConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (JiraConnectionGrant) TableName() string { return "jira_connection_grants" }

// JiraActivityHistory audits reads through a connection (who searched or
// opened what, on whose connection). Target is the JQL or issue key —
// never a credential.
type JiraActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Target         string    `gorm:"type:varchar(1024)" json:"target,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (JiraActivityHistory) TableName() string { return "jira_activity_history" }
