package models

import "time"

// KafkaConnection is a saved Kafka cluster credential set. Mirrors
// DBConnection: the SASL password is stored AES-256-GCM-encrypted in
// `sasl_password_enc` (services.TokenCipher) and never returned by any
// read path — the dedicated reveal endpoint is the only plaintext egress
// and it is audit-logged.
//
// Cluster-specific knobs that don't warrant a column (custom client id,
// CA cert PEM, insecure-skip-verify for on-prem self-signed TLS, request
// timeout) live in `options_json` so we extend without schema churn.
type KafkaConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Comma-separated host:port list, e.g. "b1:9092,b2:9092,b3:9092".
	BootstrapServers string `gorm:"type:text;not null" json:"bootstrapServers"`
	// PLAINTEXT | SSL | SASL_PLAINTEXT | SASL_SSL
	SecurityProtocol string `gorm:"type:varchar(30);not null;default:'PLAINTEXT'" json:"securityProtocol"`
	// PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512 (only when protocol is SASL_*)
	SASLMechanism string `gorm:"type:varchar(30)" json:"saslMechanism,omitempty"`
	SASLUsername  string `gorm:"type:varchar(255)" json:"saslUsername,omitempty"`
	// Encrypted SASL password, base64(nonce||ciphertext||tag). JSON-hidden.
	SASLPasswordEnc string `gorm:"type:text" json:"-"`
	// tls (caCert, insecureSkipVerify), clientId, requestTimeoutMs, ...
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (KafkaConnection) TableName() string { return "kafka_connections" }

// KafkaConnectionGrant — delegated access on a Kafka connection.
// role='read' means browse topics/groups + consume (peek); role='write'
// adds publish + topic create/alter/delete. Mirrors DBConnectionGrant.
type KafkaConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (KafkaConnectionGrant) TableName() string { return "kafka_connection_grants" }

// KafkaActivityHistory is the audit ledger for every mutating/consuming
// action against a cluster. `action` ∈ consume | produce | create_topic |
// alter_config | add_partitions | delete_topic. `detail_json` carries the
// action-specific payload (topic, fetch mode, config diff).
type KafkaActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Topic          string    `gorm:"type:varchar(255)" json:"topic,omitempty"`
	DetailJSON     string    `gorm:"type:text" json:"detail,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (KafkaActivityHistory) TableName() string { return "kafka_activity_history" }
