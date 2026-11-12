package models

import "time"

// RabbitConnection is a saved RabbitMQ endpoint. PatchMan talks to two of
// the broker's interfaces with the same user:
//
//   - the management HTTP API (URL, usually :15672) for everything that can
//     be read or done over HTTP — overview, queues, exchanges, bindings,
//     connections, peek (get with requeue), publish, purge, create/delete;
//   - AMQP 0-9-1 (host/port from OptionsJSON, default the management host
//     on :5672) only where HTTP can't: reading stream queues by offset, the
//     live tail of an exchange and moving messages between queues.
//
// The password is AES-256-GCM encrypted (services.TokenCipher) and never
// returned by a read path. TLS knobs and the AMQP endpoint live in
// OptionsJSON. Stateless credentials → dual-written like the other tools.
type RabbitConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Management API base URL, e.g. https://rabbit.example.com:15672.
	URL      string `gorm:"type:text;not null" json:"url"`
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`
	// Encrypted password. JSON-hidden.
	PasswordEnc string `gorm:"type:text" json:"-"`
	// insecureSkipVerify, caCert(PEM), amqpHost, amqpPort, amqpTLS, defaultVhost.
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (RabbitConnection) TableName() string { return "rabbit_connections" }

// RabbitConnectionGrant — delegated access on a RabbitMQ connection.
// role='read' views everything, peeks (get with requeue) and tails;
// role='write' also publishes, purges, moves messages, creates/deletes
// queues, exchanges and bindings and closes client connections.
type RabbitConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (RabbitConnectionGrant) TableName() string { return "rabbit_connection_grants" }

// RabbitActivityHistory is the audit ledger for actions that touch the
// broker's state (peek included — it marks messages redelivered).
type RabbitActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Target         string    `gorm:"type:varchar(1024)" json:"target,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (RabbitActivityHistory) TableName() string { return "rabbit_activity_history" }
