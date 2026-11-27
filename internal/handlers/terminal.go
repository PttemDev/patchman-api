package handlers

import (
	"embed"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
)

// TerminalHandler serves the desktop multi-session terminal: each tab is a
// plain login-shell PTY bridged over its own WebSocket. Desktop (embedded)
// only — running a shell on the user's own machine is never allowed on a
// hosted/shared BE.
type TerminalHandler struct {
	embedded bool
}

func NewTerminalHandler(embedded bool) *TerminalHandler {
	return &TerminalHandler{embedded: embedded}
}

// Status — GET /terminal/status. Cheap pre-flight so the FE can hide the
// terminal on web and show the shell name and home dir on desktop. Never errors.
func (h *TerminalHandler) Status(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"desktop": false}})
		return
	}
	home, _ := os.UserHomeDir()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"desktop": true,
		"shell":   filepath.Base(loginShell()),
		"home":    home,
	}})
}

// Shell — GET /terminal/shell. SECURITY GATE: embedded mode only (403
// otherwise). Framing is OUR protocol, identical to the K8s terminals.
func (h *TerminalHandler) Shell(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "the terminal is only available on the desktop app"})
		return
	}
	shell := loginShell()
	if shell == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "no shell available on this host"})
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve home: " + err.Error()})
		return
	}
	cmd := terminalCommand(shell, home)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "start shell: " + err.Error()})
		return
	}
	bridgePTY(c, cmd, ptmx)
}

// Wrapper startup files that make zsh report the running command and cwd
// (pm-session.zsh) and keep history when the shell is SIGKILLed. They chain
// to the user's own files, so nothing of the user's setup changes.
//
//go:embed terminal_zsh
var terminalZshFS embed.FS

// terminalZshDir is <configDir>/patchman/terminal/zsh (home as a fallback, like
// managedGitBase), 0700.
func terminalZshDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", herr
		}
		base = home
	}
	dir := filepath.Join(base, "patchman", "terminal", "zsh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// writeTerminalZsh (re)writes the embedded files into dir. zsh only reads
// zshenv/zprofile/zshrc/zlogin as dotfiles inside ZDOTDIR.
func writeTerminalZsh(dir string) error {
	entries, err := terminalZshFS.ReadDir("terminal_zsh")
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := terminalZshFS.ReadFile("terminal_zsh/" + e.Name())
		if err != nil {
			return err
		}
		name := e.Name()
		switch name {
		case "zshenv", "zprofile", "zshrc", "zlogin":
			name = "." + name
		}
		if err := writeFileAtomic(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic renames a finished temp file into place so a shell starting
// concurrently never reads a half-written startup file.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// terminalCommand builds the login shell started in home. For zsh it starts
// through the wrapper dir (ZDOTDIR), passing the user's real ZDOTDIR along.
func terminalCommand(shell, home string) *exec.Cmd {
	cmd := exec.Command(shell, "-l")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if filepath.Base(shell) != "zsh" {
		return cmd
	}
	dir, err := terminalZshDir()
	if err == nil {
		err = writeTerminalZsh(dir)
	}
	if err != nil {
		log.Printf("terminal: zsh wrapper unavailable, starting plain shell: %v", err)
		return cmd
	}
	userZdotdir := os.Getenv("ZDOTDIR")
	if userZdotdir == "" {
		userZdotdir = home
		cmd.Env = append(cmd.Env, "PATCHMAN_ZDOTDIR_UNSET=1")
	}
	cmd.Env = append(cmd.Env, "PATCHMAN_USER_ZDOTDIR="+userZdotdir, "ZDOTDIR="+dir)
	return cmd
}
