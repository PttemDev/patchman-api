package models

import "time"

// LLMProvider is a saved LLM endpoint the user can send prompts to — an
// OpenAI-compatible server (OpenAI, Ollama, LM Studio, vLLM, together, …) or an
// Anthropic Messages endpoint. It mirrors DBConnection's storage model: an
// FE-supplied UUID PK so the same row can be dual-written to the local sidecar
// AND the remote BE, and a single encrypted secret (the API key) that is
// hidden from JSON entirely.
//
// The API key is AES-256-GCM ciphertext (services.TokenCipher, the shared
// dbCipher) so it is encrypted at rest on BOTH backends — including the desktop
// sidecar, whose key is auto-provisioned next to the SQLite file. It is
// decrypted only server-side, just before proxying a request to the provider,
// and via the owner-only reveal endpoint; it never reaches the browser.
type LLMProvider struct {
	ID     string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID string `gorm:"type:varchar(36);index;not null" json:"userId"`
	Name   string `gorm:"type:varchar(255);not null" json:"name"`
	// Kind selects the wire protocol: "openai" (Chat Completions, SSE) or
	// "anthropic" (Messages, SSE). Local runtimes (Ollama/LM Studio/vLLM)
	// speak the OpenAI protocol, so they use "openai".
	Kind string `gorm:"type:varchar(20);not null" json:"kind"`
	// BaseURL is the API root, e.g. https://api.openai.com/v1,
	// http://127.0.0.1:11434/v1, or https://api.anthropic.com. Trailing
	// path is normalized by the handler.
	BaseURL string `gorm:"type:varchar(512);not null" json:"baseUrl"`
	// Model is the default model id used when a chat request doesn't
	// override it (e.g. gpt-4o-mini, claude-3-5-sonnet-latest, llama3.1).
	Model string `gorm:"type:varchar(255);not null;default:''" json:"model"`
	// APIKeyEnc is the encrypted API key, base64(nonce||ciphertext||tag)
	// per services.TokenCipher. Hidden from JSON so a careless Marshal
	// never leaks it. May be empty for a keyless local runtime.
	APIKeyEnc string `gorm:"type:text" json:"-"`
	// PrimaryScope is the user-chosen default backend ('local' | 'remote')
	// for this provider, kept identical on both BEs via the FE dual-write.
	PrimaryScope string    `gorm:"type:varchar(10);default:''" json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}
