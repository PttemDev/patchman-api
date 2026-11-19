// Package pkgsig signs and verifies plugin packages (PLUGIN_MARKETPLACE_RFC
// §18). A package folder carries SIGNATURE.json: the plugin id and version,
// the sha256 of every file that belongs to the package, and an Ed25519
// signature over a canonical text form of those. Only listed files are
// installed, so nothing can be slipped in next to a signed package.
package pkgsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FileName is the signature file inside a package folder.
const FileName = "SIGNATURE.json"

// Format is the only signature format this build understands.
const Format = "pm-sig-v1"

// Signature is the content of SIGNATURE.json.
type Signature struct {
	Format    string            `json:"format"`
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	Files     map[string]string `json:"files"` // path → sha256 hex
	KeyID     string            `json:"keyId"`
	Signature string            `json:"signature"` // base64 Ed25519
}

// Payload is the exact byte string that is signed.
func Payload(id, version string, files map[string]string) []byte {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(Format + "\n" + id + "\n" + version + "\n")
	for _, p := range paths {
		b.WriteString(strings.ToLower(files[p]) + "  " + p + "\n")
	}
	return []byte(b.String())
}

// KeyID is a short, stable name for a public key: the first 16 hex chars of
// its sha256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKey reads a base64 (std or raw-url) Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, errors.New("not a base64 Ed25519 public key (32 bytes)")
}

// ParsePrivateKey reads a base64 Ed25519 private key (64-byte key or 32-byte seed).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	}
	return nil, errors.New("not a base64 Ed25519 private key")
}

// HashFile is the sha256 hex of a file's content.
func HashFile(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Sign builds a signature over files (path → content).
func Sign(priv ed25519.PrivateKey, id, version string, files map[string][]byte) *Signature {
	hashes := make(map[string]string, len(files))
	for p, b := range files {
		hashes[p] = HashFile(b)
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, Payload(id, version, hashes))
	return &Signature{Format: Format, ID: id, Version: version, Files: hashes, KeyID: KeyID(pub), Signature: base64.StdEncoding.EncodeToString(sig)}
}

// Parse decodes SIGNATURE.json strictly.
func Parse(raw []byte) (*Signature, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var s Signature
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if s.Format != Format {
		return nil, fmt.Errorf("%s: format %q is not supported", FileName, s.Format)
	}
	if s.ID == "" || s.Version == "" || len(s.Files) == 0 || s.Signature == "" {
		return nil, fmt.Errorf("%s: id, version, files and signature are required", FileName)
	}
	for p, h := range s.Files {
		if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size || p == "" || p == FileName {
			return nil, fmt.Errorf("%s: bad file entry %q", FileName, p)
		}
	}
	return &s, nil
}

// TrustedKey is a publisher key the user trusts.
type TrustedKey struct {
	KeyID string
	Name  string
	Key   ed25519.PublicKey
}

// ErrUntrusted: the package is signed by a key that is not trusted.
var ErrUntrusted = errors.New("signed by a key that is not trusted")

// Verify checks the signature against the trusted keys and returns the key
// that signed it. It does not check file contents; use CheckFile per file.
func (s *Signature) Verify(trusted []TrustedKey) (*TrustedKey, error) {
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%s: bad signature encoding", FileName)
	}
	payload := Payload(s.ID, s.Version, s.Files)
	for i := range trusted {
		k := &trusted[i]
		if k.KeyID == s.KeyID && ed25519.Verify(k.Key, payload, sig) {
			return k, nil
		}
	}
	for i := range trusted {
		if trusted[i].KeyID == s.KeyID {
			return nil, fmt.Errorf("%s: signature does not match (package changed after signing?)", FileName)
		}
	}
	return nil, fmt.Errorf("%w (key %s)", ErrUntrusted, s.KeyID)
}

// CheckFile verifies one file against the signed list.
func (s *Signature) CheckFile(path string, b []byte) error {
	want, ok := s.Files[path]
	if !ok {
		return fmt.Errorf("%s is not part of the signed package", path)
	}
	if !strings.EqualFold(HashFile(b), want) {
		return fmt.Errorf("%s does not match its signed hash (changed after signing)", path)
	}
	return nil
}
