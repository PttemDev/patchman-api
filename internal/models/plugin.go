package models

import "time"

// Plugin is an installed plugin (PLUGIN_MARKETPLACE_RFC §6, §15). The
// install is per backend, not per user. ManifestJSON is the validated
// manifest; the wasm lives in PluginBlob so list queries never load it.
type Plugin struct {
	ID           string    `gorm:"type:varchar(128);primaryKey" json:"id"`
	Version      string    `gorm:"type:varchar(64);not null" json:"version"`
	Name         string    `gorm:"type:varchar(255);not null" json:"name"`
	Publisher    string    `gorm:"type:varchar(255)" json:"publisher"`
	ManifestJSON string    `gorm:"type:text;not null" json:"-"`
	WasmSHA256   string    `gorm:"type:varchar(64);not null" json:"wasmSha256"`
	Source       string    `gorm:"type:varchar(20);not null" json:"source"` // dev (P3); registry later
	SourcePath   string    `gorm:"type:text" json:"sourcePath,omitempty"`
	SignerKeyID  string    `gorm:"type:varchar(32)" json:"signerKeyId,omitempty"`
	Enabled      bool      `gorm:"not null;default:true" json:"enabled"`
	InstalledBy  string    `gorm:"type:varchar(36)" json:"installedBy"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (Plugin) TableName() string { return "plugins" }

// PluginBlob holds the plugin's backend.wasm.
type PluginBlob struct {
	PluginID string `gorm:"type:varchar(128);primaryKey"`
	Wasm     []byte `gorm:"not null"`
}

func (PluginBlob) TableName() string { return "plugin_blobs" }

// PluginUIFile is one file of a plugin's custom UI bundle (ui/ in the
// package), served by /plugin-ui with the plugin UI CSP.
type PluginUIFile struct {
	PluginID string `gorm:"type:varchar(128);primaryKey"`
	Path     string `gorm:"type:varchar(512);primaryKey"`
	Data     []byte `gorm:"not null"`
}

func (PluginUIFile) TableName() string { return "plugin_ui_files" }

// PluginTrustedKey is a publisher key whose signed packages may be
// installed (RFC §18). Adding or removing one needs a fresh step-up.
type PluginTrustedKey struct {
	KeyID     string    `gorm:"type:varchar(32);primaryKey" json:"keyId"`
	Name      string    `gorm:"type:varchar(120);not null" json:"name"`
	PublicKey string    `gorm:"type:varchar(64);not null" json:"publicKey"`
	AddedBy   string    `gorm:"type:varchar(36)" json:"addedBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func (PluginTrustedKey) TableName() string { return "plugin_trusted_keys" }

// PluginConnection is one configured instance of a plugin. The FE generates
// the id so both BEs of a dual-write share it. Secret fields (the manifest's
// connection.secretFields) are stored only in SecretsEnc, a TokenCipher-
// encrypted JSON object; ConfigJSON never holds one.
type PluginConnection struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	PluginID     string    `gorm:"type:varchar(128);index;not null" json:"pluginId"`
	UserID       string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID  string    `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name         string    `gorm:"type:varchar(255);not null" json:"name"`
	ConfigJSON   string    `gorm:"type:text;not null;default:'{}'" json:"-"`
	SecretsEnc   string    `gorm:"type:text" json:"-"`
	AllowWrite   bool      `gorm:"not null;default:false" json:"allowWrite"`
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (PluginConnection) TableName() string { return "plugin_connections" }

// PluginConnectionGrant — delegated access on a plugin connection.
// role='read' runs non-write ops; role='write' adds write ops.
type PluginConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (PluginConnectionGrant) TableName() string { return "plugin_connection_grants" }

// PluginActivity is the audit ledger: every op call, reveal, install and
// uninstall. Kept after uninstall (it is the audit trail).
type PluginActivity struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	PluginID       string    `gorm:"type:varchar(128);index;not null" json:"pluginId"`
	PluginVersion  string    `gorm:"type:varchar(64)" json:"pluginVersion"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index" json:"connectionId,omitempty"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"` // op | reveal | install | uninstall | enable | disable
	Op             string    `gorm:"type:varchar(64)" json:"op,omitempty"`
	Client         string    `gorm:"type:varchar(20)" json:"client,omitempty"` // "mcp" for agent calls
	DurationMs     int64     `json:"durationMs"`
	DetailJSON     string    `gorm:"type:text" json:"detail,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (PluginActivity) TableName() string { return "plugin_activity" }
