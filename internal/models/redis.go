package models

import "time"

// RedisConnection is a saved Redis endpoint credential set. Mirrors
// KafkaConnection / DBConnection: the auth password (and, for sentinel
// deployments, the separate sentinel-auth password) are stored
// AES-256-GCM-encrypted (services.TokenCipher) and never returned by any
// read path — the dedicated reveal endpoint is the only plaintext egress
// and it is audit-logged.
//
// Deployment shape is captured by Mode (standalone | sentinel | cluster);
// Address is a comma-joined seed list (a single host:port for standalone,
// the sentinel seeds for sentinel, the cluster seeds for cluster).
// Knobs that don't warrant a column (CA cert PEM, insecure-skip-verify for
// on-prem self-signed TLS, client name, replica routing, dial timeout, pool
// size) live in OptionsJSON so we extend without schema churn.
type RedisConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// "host:6379" for standalone, comma-joined seed list for sentinel/cluster.
	Address string `gorm:"type:text;not null" json:"address"`
	// standalone | sentinel | cluster
	Mode string `gorm:"type:varchar(20);not null;default:'standalone'" json:"mode"`
	// Required when mode=sentinel — the monitored primary's name.
	SentinelMaster string `gorm:"type:varchar(255)" json:"sentinelMaster,omitempty"`
	// ACL username; "" = legacy (requirepass) auth.
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`
	// Encrypted data-node password, base64(nonce||ciphertext||tag). JSON-hidden.
	PasswordEnc string `gorm:"type:text" json:"-"`
	// Encrypted sentinel-auth password (optional; sentinel mode). JSON-hidden.
	SentinelPasswordEnc string `gorm:"type:text" json:"-"`
	// Logical DB index (0..N). Ignored for cluster.
	DBIndex int  `gorm:"not null;default:0" json:"dbIndex"`
	TLS     bool `gorm:"not null;default:false" json:"tls"`
	// AllowWrite is the read-write master switch (the ⋯ menu), like
	// DBConnection's: off = nobody, owner included, can run a write.
	AllowWrite bool `gorm:"not null;default:false" json:"allowWrite"`
	// caCert, insecureSkipVerify, clientName, routeReadsToReplicas,
	// dialTimeoutMs, poolSize.
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (RedisConnection) TableName() string { return "redis_connections" }

// RedisConnectionGrant — delegated access on a Redis connection.
// role='read' means browse keys + read values + run read commands + peek
// pub/sub; role='write' adds mutating commands + DEL + set-TTL. Mirrors
// KafkaConnectionGrant.
type RedisConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (RedisConnectionGrant) TableName() string { return "redis_connection_grants" }

// RedisActivityHistory is the audit ledger for every mutating/inspecting
// action against a Redis endpoint. `action` ∈ scan | get | meta | command |
// pubsub_peek | delete | set_ttl | remove_element. `detail_json` carries the action-specific
// payload (command args, match pattern, ttl). DelegatedOwner is set when the
// actor is a grant holder (not the connection owner).
type RedisActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Key            string    `gorm:"type:varchar(512)" json:"key,omitempty"`
	DetailJSON     string    `gorm:"type:text" json:"detail,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (RedisActivityHistory) TableName() string { return "redis_activity_history" }
