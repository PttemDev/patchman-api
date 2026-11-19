package models

import "time"

// GCPCredential is one Google Cloud login that several tools share
// (GOOGLE_CLOUD_RFC.md ADR-1): Cloud Storage, Pub/Sub, Cloud Logging and GKE
// clusters all reference it, so the key / account is entered once.
//
// Kind:
//   - "service_account": the key JSON, encrypted in SecretEnc;
//   - "adc": `gcloud auth application-default login` on this computer
//     (desktop only — the credentials never leave the machine, SecretEnc empty);
//   - "oauth": a linked Google account; SecretEnc holds its refresh token.
//
// ReadWrite opts the credential in to writes (upload / delete objects,
// publish messages); every write also asks for allowWrite on the call.
type GCPCredential struct {
	ID        string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID    string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	Name      string    `gorm:"type:varchar(255);not null" json:"name"`
	Kind      string    `gorm:"type:varchar(20);not null" json:"kind"`
	SecretEnc string    `gorm:"type:text" json:"-"`
	ProjectID string    `gorm:"type:varchar(255)" json:"projectId,omitempty"`
	Account   string    `gorm:"type:varchar(255)" json:"account,omitempty"`
	Scopes    string    `gorm:"type:text" json:"scopes,omitempty"`
	ReadWrite bool      `gorm:"not null;default:false" json:"readWrite"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (GCPCredential) TableName() string { return "gcp_credentials" }
