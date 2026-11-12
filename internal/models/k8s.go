package models

import "time"

// K8sConnection is a saved Kubernetes cluster credential set (a whole
// kubeconfig YAML). Mirrors the connection-spine of the Redis / ES tools: the
// secret material (the kubeconfig, which carries client certs / tokens / exec
// plugin configs) is stored AES-256-GCM-encrypted (services.TokenCipher) and
// never returned by any read path — the dedicated owner-only reveal endpoint is
// the sole plaintext egress and it is audit-logged.
//
// The connection is dual-written across BEs like the other tools (FE-generated
// UUID; each BE re-encrypts the kubeconfig with its own cipher — the ciphertext
// itself is NOT dual-written), but live cluster ops are single-routed to a
// reachable BE.
type K8sConnection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// Whole kubeconfig YAML, AES-GCM-encrypted, base64(nonce||ciphertext||tag).
	// JSON-hidden — never leaves the process except via RevealSecret.
	KubeconfigEnc string `gorm:"type:text" json:"-"`
	// Selected context name (defaults to the kubeconfig current-context).
	DefaultContext string `gorm:"type:varchar(255)" json:"defaultContext,omitempty"`
	// Cached NON-secret context list [{name,cluster,server,namespace}] so the FE
	// can populate the context picker without a cluster call.
	ContextsJSON string `gorm:"type:text;default:'[]'" json:"-"`
	// kubeconfig | token (v1: kubeconfig only).
	AuthMode string `gorm:"type:varchar(20);not null;default:'kubeconfig'" json:"authMode"`
	// insecureSkipVerify(bool), allowedNamespaces([]string),
	// disableExecPlugins(bool, default true).
	OptionsJSON  string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (K8sConnection) TableName() string { return "k8s_connections" }

// K8sConnectionGrant — delegated access on a Kubernetes connection. role='read'
// covers all v1 read/list/get/logs surfaces; role='write' is reserved for the
// v2 exec/edit-apply/quick-action surface. Mirrors ESConnectionGrant.
type K8sConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (K8sConnectionGrant) TableName() string { return "k8s_connection_grants" }

// K8sActivityHistory is the audit ledger for every cluster op. Action ∈
// contexts | version | resources | resource | logs. Context/Namespace/Kind/Name
// capture the target. DelegatedOwner is set when the actor is a grant holder
// (not the connection owner). Mirrors ESActivityHistory.
type K8sActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Context        string    `gorm:"type:varchar(255)" json:"context,omitempty"`
	Namespace      string    `gorm:"type:varchar(255)" json:"namespace,omitempty"`
	Kind           string    `gorm:"type:varchar(64)" json:"kind,omitempty"`
	Name           string    `gorm:"type:varchar(255)" json:"name,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (K8sActivityHistory) TableName() string { return "k8s_activity_history" }
