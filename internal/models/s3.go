package models

import "time"

// S3Connection is a saved S3-compatible object-storage credential set
// (AWS S3, MinIO, Ceph RGW, Wasabi, R2, B2, DigitalOcean Spaces, …). The
// secret access key is AES-256-GCM-encrypted (services.TokenCipher) and
// never returned by a read path; the reveal endpoint is the only egress.
//
// Provider-specific knobs (KMS key, default SSE, custom signing region)
// live in `options_json`.
type S3Connection struct {
	ID          string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID      string `gorm:"type:varchar(36);index;not null" json:"userId"`
	WorkspaceID string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	Name        string `gorm:"type:varchar(255);not null" json:"name"`
	// aws | minio | ceph | wasabi | r2 | b2 | spaces | gcs | oracle | custom
	Provider string `gorm:"type:varchar(30);not null;default:'aws'" json:"provider"`
	// Endpoint override for non-AWS / on-prem. Empty = AWS default.
	Endpoint       string `gorm:"type:varchar(500)" json:"endpoint,omitempty"`
	Region         string `gorm:"type:varchar(100);not null;default:'us-east-1'" json:"region"`
	AccessKeyID    string `gorm:"type:varchar(255)" json:"accessKeyId,omitempty"`
	SecretKeyEnc   string `gorm:"type:text" json:"-"`
	SessionTokenEnc string `gorm:"type:text" json:"-"`
	// On-prem storage (MinIO/Ceph) needs path-style addressing; virtual-
	// hosted (AWS) uses false.
	ForcePathStyle bool `gorm:"default:false" json:"forcePathStyle"`
	// Accept self-signed TLS (on-prem). Off by default.
	InsecureSkipVerify bool      `gorm:"default:false" json:"insecureSkipVerify"`
	OptionsJSON        string    `gorm:"type:text;default:'{}'" json:"options,omitempty"`
	PrimaryScope       string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func (S3Connection) TableName() string { return "s3_connections" }

// S3ConnectionGrant — delegated access. read='browse+get', write='put/
// delete/copy'. Mirrors DBConnectionGrant / KafkaConnectionGrant.
type S3ConnectionGrant struct {
	ID           string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ConnectionID string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	SubjectKind  string    `gorm:"type:varchar(20);not null" json:"subjectKind"`
	SubjectID    string    `gorm:"type:varchar(36);not null;index" json:"subjectId"`
	Role         string    `gorm:"type:varchar(20);not null;default:'read'" json:"role"`
	GrantedBy    string    `gorm:"type:varchar(36);not null" json:"grantedBy"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (S3ConnectionGrant) TableName() string { return "s3_connection_grants" }

// S3ActivityHistory audits mutating/reading actions.
// action ∈ get | put | delete | copy | presign | create_folder.
type S3ActivityHistory struct {
	ID             string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID         string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	DelegatedOwner *string   `gorm:"type:varchar(36);index" json:"delegatedOwner,omitempty"`
	ConnectionID   string    `gorm:"type:varchar(36);index;not null" json:"connectionId"`
	Action         string    `gorm:"type:varchar(30);not null" json:"action"`
	Bucket         string    `gorm:"type:varchar(255)" json:"bucket,omitempty"`
	ObjectKey      string    `gorm:"type:text" json:"objectKey,omitempty"`
	ErrorText      string    `gorm:"type:text" json:"error,omitempty"`
	ExecutedAt     time.Time `gorm:"index" json:"executedAt"`
}

func (S3ActivityHistory) TableName() string { return "s3_activity_history" }
