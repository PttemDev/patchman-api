package pkgsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerify(t *testing.T) {
	pub, priv := key(t)
	other, _ := key(t)
	files := map[string][]byte{"manifest.json": []byte("{}"), "backend.wasm": []byte("wasm"), "ui/index.html": []byte("<p>")}
	sig := Sign(priv, "dev.a.b", "1.0.0", files)
	raw, _ := json.Marshal(sig)
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	trusted := []TrustedKey{{KeyID: KeyID(pub), Name: "me", Key: pub}}
	if k, err := parsed.Verify(trusted); err != nil || k.Name != "me" {
		t.Fatalf("verify: %v", err)
	}
	for p, b := range files {
		if err := parsed.CheckFile(p, b); err != nil {
			t.Error(err)
		}
	}
	if err := parsed.CheckFile("backend.wasm", []byte("evil")); err == nil {
		t.Error("changed file accepted")
	}
	if err := parsed.CheckFile("ui/extra.js", []byte("x")); err == nil {
		t.Error("unlisted file accepted")
	}
	if _, err := parsed.Verify([]TrustedKey{{KeyID: KeyID(other), Key: other}}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("untrusted key: %v", err)
	}
	// Any change to the signed content breaks the signature.
	for name, mutate := range map[string]func(s *Signature){
		"version": func(s *Signature) { s.Version = "9.9.9" },
		"id":      func(s *Signature) { s.ID = "dev.evil.b" },
		"hash":    func(s *Signature) { s.Files["backend.wasm"] = HashFile([]byte("evil")) },
		"add":     func(s *Signature) { s.Files["ui/evil.js"] = HashFile([]byte("x")) },
	} {
		c, _ := Parse(raw)
		mutate(c)
		if _, err := c.Verify(trusted); err == nil || errors.Is(err, ErrUntrusted) {
			t.Errorf("%s: tampered signature accepted or misreported: %v", name, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	_, priv := key(t)
	good, _ := json.Marshal(Sign(priv, "dev.a.b", "1.0.0", map[string][]byte{"manifest.json": []byte("{}")}))
	cases := map[string]string{
		"format":  strings.Replace(string(good), `"pm-sig-v1"`, `"pm-sig-v9"`, 1),
		"unknown": strings.Replace(string(good), `"format"`, `"extra":1,"format"`, 1),
		"badhash": strings.Replace(string(good), `"manifest.json":"`, `"manifest.json":"zz`, 1),
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestKeyParsing(t *testing.T) {
	pub, priv := key(t)
	if k, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub) + "\n"); err != nil || !k.Equal(pub) {
		t.Errorf("public key: %v", err)
	}
	if _, err := ParsePublicKey("short"); err == nil {
		t.Error("bad public key accepted")
	}
	if k, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv)); err != nil || !k.Equal(priv) {
		t.Errorf("private key: %v", err)
	}
	if k, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv.Seed())); err != nil || !k.Equal(priv) {
		t.Errorf("seed: %v", err)
	}
}
