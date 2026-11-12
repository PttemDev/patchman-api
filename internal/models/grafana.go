package models

import "time"

// GrafanaConnection is a saved Grafana endpoint + credential. Dashboards are
// rendered natively by PatchMan from Grafana's HTTP API (no iframe), so the
// credential is whatever Grafana's API accepts:
//
//   - token:   service-account / API token → Authorization: Bearer.
//   - basic:   username + password. HTTP Basic first; when Grafana refuses it
//     (auth.basic disabled) the registry logs in through POST /login and keeps
//     that session in memory, re-logging in on expiry.
//   - session: a pasted (web) or captured (desktop "Sign in with Grafana")
//     grafana_session cookie, for SSO-only users with no Grafana password. The
//     registry rotates it (POST /api/user/auth-tokens/rotate on Grafana 10+,
//     Set-Cookie capture on older) and writes the new value back here. A
//     session lives on ONE backend, so these rows are not dual-written.
//
// Secrets are AES-256-GCM encrypted (services.TokenCipher) and never returned
// by a read path. TLS knobs, orgId and the cookie name live in OptionsJSON.
type GrafanaConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Base URL including any sub path, e.g. https://grafana.example.com/grafana.
	URL string `gorm:"type:text;not null" json:"url"`
	// token | basic | session.
	AuthKind string `gorm:"type:varchar(20);not null;default:'token'" json:"authKind"`
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`
	// Encrypted service-account/API token. JSON-hidden.
	TokenEnc string `gorm:"type:text" json:"-"`
	// Encrypted password (basic). JSON-hidden.
	PasswordEnc string `gorm:"type:text" json:"-"`
	// Encrypted session cookie value (session). Rewritten on every rotation.
	SessionEnc       string     `gorm:"type:text" json:"-"`
	SessionRotatedAt *time.Time `json:"sessionRotatedAt,omitempty"`
	// caCert(PEM), insecureSkipVerify, orgId, cookieName.
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (GrafanaConnection) TableName() string { return "grafana_connections" }

// GrafanaConnectionGrant — delegated access on a Grafana connection.
// role='read' views dashboards and runs queries, with raw SQL limited to
// read-only statements; role='write' lifts that SQL limit. Mirrors
// ESConnectionGrant.
type GrafanaConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (GrafanaConnectionGrant) TableName() string { return "grafana_connection_grants" }

// GrafanaActivityHistory is the audit ledger for Grafana API calls made on a
// user's behalf. Action ∈ search | dashboard | variables | query | resource.
type GrafanaActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Target         string    `gorm:"type:varchar(1024)" json:"target,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (GrafanaActivityHistory) TableName() string { return "grafana_activity_history" }
