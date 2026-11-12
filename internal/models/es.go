package models

import "time"

// ESConnection is a saved Elasticsearch/OpenSearch endpoint credential set.
// Mirrors RedisConnection / DBConnection: all three secret forms (basic-auth
// password, "id:api_key" pair, bearer/service token) are stored
// AES-256-GCM-encrypted (services.TokenCipher) and never returned by any read
// path — the dedicated reveal endpoint is the only plaintext egress and it is
// audit-logged.
//
// Nodes is a comma-joined base-URL seed list; CloudID is an optional Elastic
// Cloud id that decodes to a node URL. Flavor is elasticsearch | opensearch |
// auto. AuthKind selects which secret is used. TLS knobs that don't warrant a
// column (CA cert PEM, insecure-skip-verify, SHA-256 cert fingerprint pin,
// client cert/key for mTLS, default index) live in OptionsJSON so we extend
// without schema churn.
type ESConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Comma-separated base URLs, e.g. "https://h1:9200,https://h2:9200".
	Nodes string `gorm:"type:text" json:"nodes"`
	// Optional Elastic Cloud id (decodes to a node URL).
	CloudID string `gorm:"type:text" json:"cloudId,omitempty"`
	// elasticsearch | opensearch | auto (default auto).
	Flavor string `gorm:"type:varchar(20);not null;default:'auto'" json:"flavor"`
	// none | basic | apiKey | bearer | kibanaProxy. kibanaProxy routes queries
	// through Kibana's Console proxy (POST /api/console/proxy) using the user's
	// Kibana access — Username+PasswordEnc (basic) or CookieEnc (SSO session
	// cookie) — with no direct ES cluster credentials. Its knobs (kibanaUrl,
	// kibanaAuthMode, kibanaSpace) live in OptionsJSON.
	AuthKind string `gorm:"type:varchar(20);not null;default:'none'" json:"authKind"`
	// Basic-auth username (plaintext).
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`
	// Encrypted basic-auth password, base64(nonce||ciphertext||tag). JSON-hidden.
	PasswordEnc string `gorm:"type:text" json:"-"`
	// Encrypted "id:api_key" base64 pair (AES-GCM). JSON-hidden.
	ApiKeyEnc string `gorm:"type:text" json:"-"`
	// Encrypted bearer/service token (AES-GCM). JSON-hidden.
	BearerEnc string `gorm:"type:text" json:"-"`
	// Encrypted Kibana session cookie (the pasted `sid` value) for the
	// kibanaProxy cookie auth mode (AES-GCM). JSON-hidden.
	CookieEnc string `gorm:"type:text" json:"-"`
	TLS       bool   `gorm:"not null;default:false" json:"tls"`
	// caCert(PEM), insecureSkipVerify, caFingerprint, clientCert, clientKey,
	// defaultIndex.
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (ESConnection) TableName() string { return "es_connections" }

// ESConnectionGrant — delegated access on an Elasticsearch connection.
// role='read' means cluster/index browse + read requests (search/count/cat/…);
// role='write' adds mutating requests (bulk/index/delete/reindex/…). Mirrors
// RedisConnectionGrant.
type ESConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (ESConnectionGrant) TableName() string { return "es_connection_grants" }

// ESActivityHistory is the audit ledger for every request forwarded to an
// Elasticsearch endpoint. Action ∈ cluster | indices | mapping | request.
// Method + Path capture the forwarded HTTP call. DelegatedOwner is set when
// the actor is a grant holder (not the connection owner). Mirrors
// RedisActivityHistory.
type ESActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Method         string    `gorm:"type:varchar(10)" json:"method,omitempty"`
	Path           string    `gorm:"type:varchar(1024)" json:"path,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (ESActivityHistory) TableName() string { return "es_activity_history" }
