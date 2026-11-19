package main

import (
	"os"
	"path/filepath"
	"testing"
)

// DESKTOP_SECURITY_RFC L5: the Keychain key must never shadow an existing
// key file, and a migrated install must survive a Keychain read failure.
func TestEnsureEmbeddedCipherKeyOrder(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "patchman.db")
	write := func(p, v string) {
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 1. file wins over a differing env key
	write(db+".cipher.key", "FILEKEY")
	if k, _ := ensureEmbeddedCipherKey(db, "ENVKEY"); k != "FILEKEY" {
		t.Fatalf("file should win, got %q", k)
	}
	// 2. migrated (file → .bak): env key is used, nothing written
	_ = os.Rename(db+".cipher.key", db+".cipher.key.bak")
	if k, _ := ensureEmbeddedCipherKey(db, "FILEKEY"); k != "FILEKEY" {
		t.Fatalf("env key, got %q", k)
	}
	if _, err := os.Stat(db + ".cipher.key"); !os.IsNotExist(err) {
		t.Fatal("env key must not be written to disk")
	}
	// 3. keychain unreadable → .bak
	if k, _ := ensureEmbeddedCipherKey(db, ""); k != "FILEKEY" {
		t.Fatalf("bak fallback, got %q", k)
	}
	// 4. fresh install without a shell → new key file
	fresh := filepath.Join(t.TempDir(), "x.db")
	k, err := ensureEmbeddedCipherKey(fresh, "")
	if err != nil || len(k) != 44 {
		t.Fatalf("fresh key %q %v", k, err)
	}
	if b, _ := os.ReadFile(fresh + ".cipher.key"); string(b) != k {
		t.Fatal("fresh key not persisted")
	}
}

// Deleting the key file is only safe if a wrong or missing Keychain key can
// never replace the real one: the kid fingerprint pins it.
func TestEnsureEmbeddedCipherKeyPinned(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "patchman.db")
	if err := os.WriteFile(db+".cipher.key", []byte("REALKEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Upgrade: the file's key gets pinned.
	if k, _ := ensureEmbeddedCipherKey(db, "REALKEY"); k != "REALKEY" {
		t.Fatalf("got %q", k)
	}
	if b, _ := os.ReadFile(db + ".cipher.kid"); string(b) != cipherKeyID("REALKEY") {
		t.Fatal("kid not written")
	}
	// File deleted: the matching Keychain key works.
	_ = os.Remove(db + ".cipher.key")
	if k, err := ensureEmbeddedCipherKey(db, "REALKEY"); err != nil || k != "REALKEY" {
		t.Fatalf("keychain key: %q %v", k, err)
	}
	// A different (re-minted) Keychain key is refused, nothing is minted.
	if k, err := ensureEmbeddedCipherKey(db, "OTHERKEY"); err == nil || k != "" {
		t.Fatalf("wrong key accepted: %q", k)
	}
	if _, err := os.Stat(db + ".cipher.key"); !os.IsNotExist(err) {
		t.Fatal("must not mint a key over pinned secrets")
	}
	// Keychain unreadable, no file: refused rather than minted.
	if _, err := ensureEmbeddedCipherKey(db, ""); err == nil {
		t.Fatal("expected an error with no key source")
	}
	// A matching .bak still rescues it.
	_ = os.WriteFile(db+".cipher.key.bak", []byte("REALKEY"), 0o600)
	if k, _ := ensureEmbeddedCipherKey(db, ""); k != "REALKEY" {
		t.Fatalf("bak: %q", k)
	}
}
