// Package mcpdisco is the single source of truth for the local-MCP discovery
// contract shared by the embedded backend (the writer) and the patchman-mcp
// stdio bridge (the reader). Keeping the schema, the file path, and the
// stable-bridge-path derivation in one package prevents drift between the two
// binaries — the bridge is launched by Claude Desktop with no arguments, so
// it must recompute the identical path the BE wrote.
package mcpdisco

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// SchemaVersion is the current discovery-file schema. Bump when the shape
// changes incompatibly so a reader can reject a file it doesn't understand.
const SchemaVersion = 1

// Info is the discovery-file payload. It tells the bridge where the embedded
// backend is listening and which token to present. The plaintext token lives
// nowhere else recoverable — see handlers.EnsureLocalMCPToken.
type Info struct {
	SchemaVersion int       `json:"schemaVersion"`
	URL           string    `json:"url"`
	Token         string    `json:"token"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"startedAt"`
	Version       string    `json:"version,omitempty"`
}

// configDir is os.UserConfigDir, swappable in tests.
var configDir = os.UserConfigDir

// Path returns <UserConfigDir>/PatchMan/mcp.json. Chosen over the Tauri
// app-data dir because both the BE and the separately-spawned bridge derive
// it the same way with no hard-coded bundle identifier.
func Path() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "PatchMan", "mcp.json"), nil
}

// Write persists the discovery file atomically (temp file + rename) with the
// directory at 0700 and the file at 0600. Overwrites any existing file — the
// self-healing property that makes a stale file from a crashed run harmless.
func Write(i Info) error {
	if i.SchemaVersion == 0 {
		i.SchemaVersion = SchemaVersion
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Read loads the discovery file from the default path.
func Read() (Info, error) {
	p, err := Path()
	if err != nil {
		return Info{}, err
	}
	return ReadPath(p)
}

// ReadPath loads a discovery file from an explicit path — used by the bridge
// when a --discovery flag or PATCHMAN_MCP_DISCOVERY override is set.
func ReadPath(path string) (Info, error) {
	var i Info
	b, err := os.ReadFile(path)
	if err != nil {
		return i, err
	}
	if err := json.Unmarshal(b, &i); err != nil {
		return i, err
	}
	return i, nil
}

// Remove deletes the discovery file. A missing file is not an error.
func Remove() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveIfOwner deletes the discovery file only while it still names pid. A
// backend shutting down must not delete the entry of another PatchMan — a
// second instance, or the app that started after it — or every bridge loses
// the running app until it restarts. A file that can't be parsed is removed.
func RemoveIfOwner(pid int) error {
	p, err := Path()
	if err != nil {
		return err
	}
	cur, err := ReadPath(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && cur.PID != pid {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Keep re-asserts info as the discovery file every interval until ctx ends.
// The file is rewritten when it is missing or unreadable, or when the
// backend it names no longer answers (crashed, killed, a removed test
// instance). A file naming another live PatchMan is left alone — the
// last-started app stays reachable — and once that one goes away this one
// takes over again. Startup itself always overwrites (Write), so a stale
// file from a crash never needs deleting.
func Keep(ctx context.Context, info Info, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, _ = keepOnce(info)
	}
}

// keepOnce is one Keep check; rewrote says whether the file was rewritten.
func keepOnce(info Info) (rewrote bool, err error) {
	p, err := Path()
	if err != nil {
		return false, err
	}
	cur, rerr := ReadPath(p)
	if rerr == nil && cur.PID == info.PID && cur.URL == info.URL {
		return false, nil
	}
	if rerr == nil && Alive(cur.URL) {
		return false, nil
	}
	return true, Write(info)
}

// Alive reports whether the PatchMan backend behind a discovery URL answers
// its health probe. A PID check would be fooled by PID reuse after a crash;
// a bare port check by an unrelated program on a reused port.
func Alive(mcpURL string) bool {
	u, err := url.Parse(mcpURL)
	if err != nil || u.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Scheme+"://"+u.Host+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// exeSuffix is ".exe" on Windows, empty elsewhere.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// StableBridgePath returns ~/.patchman/bin/patchman-mcp(.exe): an
// install-location-independent absolute path the Claude Desktop config can
// reference verbatim. Claude does not expand ~, so callers surface the
// resolved absolute path to the user.
func StableBridgePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".patchman", "bin", "patchman-mcp"+exeSuffix()), nil
}

// EnsureStableBridge copies the bundled patchman-mcp binary to the stable
// path, skipping the copy when contents are unchanged. The copy is atomic
// (temp file + rename) and never overwrites in place: Claude Desktop keeps
// the running bridge's file open, and an in-place overwrite fails with
// ETXTBSY on Linux / can corrupt a running Mach-O on macOS. Rename leaves the
// old inode to the running process and swaps the new one in for next launch.
func EnsureStableBridge() (string, error) {
	src, err := locateBundledBridge()
	if err != nil {
		return "", err
	}
	dst, err := StableBridgePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if sameContents(src, dst) {
		return dst, nil
	}
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// locateBundledBridge finds the patchman-mcp binary shipped beside the
// running server. Tauri strips the target-triple suffix at bundle time
// (Contents/MacOS/patchman-mcp), so the plain name is tried first; the glob
// fallback covers `tauri dev`, where the suffix is NOT stripped
// (patchman-mcp-<triple>).
func locateBundledBridge() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(self)
	cands := []string{filepath.Join(dir, "patchman-mcp"+exeSuffix())}
	if matches, gerr := filepath.Glob(filepath.Join(dir, "patchman-mcp-*")); gerr == nil {
		cands = append(cands, matches...)
	}
	for _, cand := range cands {
		if fi, e := os.Stat(cand); e == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("bundled patchman-mcp not found next to %s", self)
}

// sameContents reports whether two files have identical SHA-256 digests. A
// missing/unreadable file (notably a not-yet-created dst) reports false so the
// copy proceeds.
func sameContents(a, b string) bool {
	ha, err := fileSHA(a)
	if err != nil {
		return false
	}
	hb, err := fileSHA(b)
	if err != nil {
		return false
	}
	return ha == hb
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// OpenFile honors umask, so force the mode explicitly.
	return os.Chmod(dst, perm)
}
