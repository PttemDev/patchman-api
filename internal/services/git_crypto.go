package services

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// TokenCipher encrypts/decrypts opaque short strings (OAuth access/refresh
// tokens) at column level using AES-256-GCM. The output is base64(nonce ||
// ciphertext || tag), so it round-trips through a TEXT column.
type TokenCipher struct {
	gcm cipher.AEAD
}

// NewTokenCipher takes the base64-encoded 32-byte master key from
// GITLAB_TOKEN_ENC_KEY. Anything other than 32 bytes after decoding is
// rejected — AES-256 is mandatory; a misconfigured length would silently
// downgrade if we accepted it.
func NewTokenCipher(b64Key string) (*TokenCipher, error) {
	if b64Key == "" {
		return nil, errors.New("token encryption key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("decode token enc key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("token enc key must be 32 bytes after base64 decode, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &TokenCipher{gcm: gcm}, nil
}

// Encrypt seals plaintext into base64(nonce || ciphertext || tag).
func (c *TokenCipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := append(nonce, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt. Tampered or wrong-key inputs return an error
// rather than garbage — GCM authenticates the ciphertext.
func (c *TokenCipher) Decrypt(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, sealed := raw[:ns], raw[ns:]
	out, err := c.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(out), nil
}
