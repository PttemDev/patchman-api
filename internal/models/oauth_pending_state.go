package models

import "time"

// OAuthPendingState carries the in-flight context of an OAuth round
// trip — the random `state` we asked the provider to echo back, plus
// the PKCE code_verifier and the redirect_uri we picked at start time
// (which the callback has to use byte-for-byte during token exchange).
//
// Stored in the database rather than process memory because BE
// restarts (deploys, panics, even local `go run` cycles) used to wipe
// the in-flight map and surface "unknown or expired state — restart
// sign-in" to anyone who happened to be mid-flow. With this row the
// callback succeeds even if the BE restarted while the user was on
// the provider's consent screen.
//
// Used by both the SSO sign-in flow and the GitLab account-linking
// integration. Distinguished by Kind so a single rememberPending /
// takePending pair handles both call sites.
type OAuthPendingState struct {
	// State is the random opaque token the provider will echo back to
	// the callback. Doubles as the primary key and lookup key.
	State string `gorm:"type:varchar(128);primaryKey" json:"state"`
	// Kind separates SSO sign-in (kind="sso") from the per-user GitLab
	// account-linking flow (kind="git_integration") — same table,
	// different consumers.
	Kind string `gorm:"type:varchar(32);index;not null" json:"kind"`
	// Provider is the SSO provider id ("gitlab"). Empty for the
	// account-linking flow which only ever talks to GitLab.
	Provider string `gorm:"type:varchar(32)" json:"provider,omitempty"`
	// UserID is set for the integration flow so the callback can stamp
	// the linked tokens onto the right user. Empty for SSO sign-in
	// (the user doesn't exist yet).
	UserID string `gorm:"type:varchar(36);index" json:"userId,omitempty"`
	// CodeVerifier is the PKCE secret we generated at start; the
	// provider hashes its known code_challenge against this on
	// exchange.
	CodeVerifier string `gorm:"type:varchar(255)" json:"-"`
	// RedirectURI must match exactly what we sent at start; some
	// providers reject the exchange otherwise. Stored so a multi-
	// surface FE (web vs desktop deep-link) doesn't have to figure it
	// out again.
	RedirectURI string `gorm:"type:varchar(512)" json:"redirectUri"`
	CreatedAt   time.Time `gorm:"index" json:"createdAt"`
}

const (
	OAuthKindSSO            = "sso"
	OAuthKindGitIntegration = "git_integration"
)
