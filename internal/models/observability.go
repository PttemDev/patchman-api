package models

import "time"

// ObservabilityConnection is a saved APM / observability backend PatchMan
// reads the live service map from: Coroot, Prometheus (coroot-node-agent,
// Istio, Linkerd, OpenTelemetry/Tempo service graph, Hubble metrics),
// Jaeger, Kiali, SigNoz, Datadog. Provider picks the adapter in internal/apm.
//
// AuthKind ∈ none | basic | bearer | apikey | session | login | datadog;
// which ones a provider accepts is the adapter's call. All secrets live in
// SecretsEnc — one AES-256-GCM encrypted JSON object {password, token,
// apiKey, appKey, session} — and are never returned by a read path.
type ObservabilityConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	Provider    string `gorm:"type:varchar(30);not null" json:"provider"`
	// Base URL including any sub path (Datadog: the site's API host).
	URL        string `gorm:"type:text;not null" json:"url"`
	AuthKind   string `gorm:"type:varchar(20);not null;default:'none'" json:"authKind"`
	Username   string `gorm:"type:varchar(255)" json:"username,omitempty"`
	SecretsEnc string `gorm:"type:text" json:"-"`
	// Default scope the map opens with: Coroot project, Datadog / SigNoz env,
	// Kiali namespaces (comma list).
	Scope string `gorm:"type:varchar(512)" json:"scope,omitempty"`
	// caCert(PEM), insecureSkipVerify, headerName (apikey), provider knobs.
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (ObservabilityConnection) TableName() string { return "observability_connections" }

// ObservabilityConnectionGrant — delegated access on an observability
// connection. Everything PatchMan does with these backends is read-only, so
// read and write grant the same.
type ObservabilityConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (ObservabilityConnectionGrant) TableName() string { return "observability_connection_grants" }

// ObservabilityActivityHistory is the audit ledger for calls made on a
// user's behalf. Action ∈ test | scopes | map | node.
type ObservabilityActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Target         string    `gorm:"type:varchar(1024)" json:"target,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (ObservabilityActivityHistory) TableName() string { return "observability_activity_history" }
