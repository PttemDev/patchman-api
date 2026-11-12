package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"patchman-be/internal/models"
)

// AIShellHandler spawns the user's locally-installed `claude` CLI in a PTY and
// bridges it to an xterm terminal over a WebSocket — an in-app "Claude Code"
// terminal, the way VS Code embeds one. Desktop (embedded) only: like the K8s
// local shell, running a CLI on the user's own machine is never allowed on a
// hosted/shared BE.
type AIShellHandler struct {
	db       *gorm.DB
	embedded bool
}

func NewAIShellHandler(db *gorm.DB, embedded bool) *AIShellHandler {
	return &AIShellHandler{db: db, embedded: embedded}
}

// ClaudeStatus — GET /ai/claude-status. Cheap pre-flight so the FE can render
// the right button state (available / install-hint / hidden on web) before
// opening the terminal WebSocket. Never errors.
func (h *AIShellHandler) ClaudeStatus(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"available": false, "desktop": false}})
		return
	}
	_, _, ok := resolveClaude(loginShell())
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"available": ok, "desktop": true}})
}

// ClaudeShell — GET /ai/claude-shell
//
// It launches `claude` rooted at a "workspace" dir under the managed git-repos
// base, where every cloned repo is exposed as a readable-named symlink, so the
// one terminal sees all projects by name (not by their UUID clone dirs).
//
// SECURITY GATE: embedded mode only (403 otherwise). Detection + launch both go
// through the user's LOGIN shell so nvm/homebrew PATHs resolve — a GUI app's own
// PATH is too thin to find `claude` on most machines. Framing is OUR protocol,
// identical to the K8s terminals: FE sends {type:stdin|resize}, BE emits
// {type:stdout|exit}.
func (h *AIShellHandler) ClaudeShell(c *gin.Context) {
	// Gate: desktop only.
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "the Claude Code terminal is only available on the desktop app"})
		return
	}

	shell := loginShell()

	// Resolve HOW to launch claude (through the login shell so nvm/brew/asdf
	// installs on the user's thin GUI PATH are found). Detection and launch use
	// the SAME shell invocation, so a positive probe guarantees a working launch.
	shellArgs, bin, ok := resolveClaude(shell)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "claude CLI not found. Install Claude Code (npm i -g @anthropic-ai/claude-code), then reopen this panel."})
		return
	}

	base, err := managedGitBase()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve git-repos dir: " + err.Error()})
		return
	}
	// Build the clean, project-named workspace root (best-effort; a failure here
	// just means the terminal opens at the git-repos base instead).
	workdir := base
	if ws, werr := buildRepoWorkspace(h.db, c.GetString("userID"), base); werr == nil && ws != "" {
		workdir = ws
	}

	// Launch `claude`, replacing the shell so the terminal *is* claude and the
	// session ends when claude exits.
	var cmd *exec.Cmd
	if len(shellArgs) > 0 {
		cmd = exec.Command(shell, shellArgs...)
	} else {
		cmd = exec.Command(bin)
	}
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "start claude: " + err.Error()})
		return
	}

	wsConn, err := k8sExecUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}

	// teardown runs exactly once: close the PTY master (unblocks the read pump),
	// kill the child, close the socket.
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			_ = ptmx.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = wsConn.Close()
		})
	}
	defer teardown()

	writer := &wsWriter{conn: wsConn}
	done := make(chan struct{})

	_ = wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	})

	// Keepalive ping (single-writer via wsWriter mutex).
	go func() {
		t := time.NewTicker(execPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := writer.ping(); err != nil {
					return
				}
			}
		}
	}()

	// Reader goroutine: WS frames → PTY (stdin + resize).
	go func() {
		defer close(done)
		defer teardown()
		for {
			_, msg, rerr := wsConn.ReadMessage()
			if rerr != nil {
				return
			}
			var in execInbound
			if json.Unmarshal(msg, &in) != nil {
				continue
			}
			switch in.Type {
			case "stdin":
				if _, werr := ptmx.Write([]byte(in.Data)); werr != nil {
					return
				}
			case "resize":
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: in.Rows, Cols: in.Cols})
			}
		}
	}()

	// PTY → WS {type:stdout} pump. Returns on claude exit (EOF) or teardown.
	buf := make([]byte, localShellChunk)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			if werr := writer.writeJSON(execOutbound{Type: "stdout", Data: string(buf[:n])}); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	teardown()
	waitErr := cmd.Wait()
	out := execOutbound{Type: "exit", Code: 0}
	if waitErr != nil {
		out.Error = waitErr.Error()
		if ee, okee := waitErr.(*exec.ExitError); okee {
			out.Code = ee.ExitCode()
		} else {
			out.Code = 1
		}
	}
	_ = writer.writeJSON(out)
}

// loginShell returns the user's $SHELL, falling back to common shells.
func loginShell() string {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = firstExisting("/bin/zsh", "/bin/bash", "/bin/sh")
	}
	return shell
}

// claudeShellModes are the login-shell invocations tried, in order, to locate +
// run claude. A plain login shell (-l -c) sources .zprofile (Homebrew, most
// setups) and is hang-safe; a login+interactive shell (-l -i -c) additionally
// sources .zshrc/.bashrc where nvm/asdf usually live. First that resolves wins.
var claudeShellModes = [][]string{
	{"-l", "-c"},
	{"-l", "-i", "-c"},
}

// resolveClaude decides how to launch claude. It returns either shell args
// (to run as `<shell> <args...>`, when a login-shell probe finds claude) OR a
// direct binary path (LookPath fallback when there's no shell). `ok` is false
// when claude can't be found any way. Detection uses the SAME invocation it
// returns for launch, so a positive result is a launchable one.
func resolveClaude(shell string) (shellArgs []string, bin string, ok bool) {
	if shell != "" {
		for _, mode := range claudeShellModes {
			probe := append(append([]string{}, mode...), "command -v claude")
			// .Output() gives the child /dev/null stdin (nil Stdin) so an
			// interactive probe can't block on input; the timeout guards against
			// an rc file that hangs (direnv/tmux/read) — otherwise ClaudeStatus,
			// called on every panel mount, would wedge the AI tab.
			pctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			out, err := exec.CommandContext(pctx, shell, probe...).Output()
			cancel()
			if err == nil && strings.TrimSpace(string(out)) != "" {
				return append(append([]string{}, mode...), "exec claude"), "", true
			}
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return nil, p, true
	}
	return nil, "", false
}

var repoNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// buildRepoWorkspace (re)builds a "workspace" dir under the git-repos base that
// contains one readable-named symlink per managed clone (name → ../<uuid>), so a
// terminal opened there lists projects by name instead of UUID clone dirs. Stale
// symlinks (target gone / repo removed) are pruned. Returns the workspace path.
func buildRepoWorkspace(db *gorm.DB, userID, base string) (string, error) {
	ws := filepath.Join(base, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return "", err
	}

	var repos []models.GitRepo
	if err := db.Where("user_id = ?", userID).Find(&repos).Error; err != nil {
		return ws, err // dir exists; caller can still open it
	}

	// Desired links: readable name → clone dir. Track wanted link names so we can
	// prune anything else that is a symlink in the workspace.
	wanted := map[string]bool{}
	used := map[string]string{} // name → target, for collision detection
	for i := range repos {
		r := &repos[i]
		if strings.TrimSpace(r.LocalPath) == "" {
			continue
		}
		if fi, err := os.Stat(r.LocalPath); err != nil || !fi.IsDir() {
			continue // clone missing / not yet cloned
		}
		name := sanitizeRepoName(r.ProjectName, r.RemoteURL, r.ID)
		// Resolve name collisions across repos deterministically.
		if prev, clash := used[name]; clash && prev != r.LocalPath {
			name = name + "-" + shortID(r.ID)
		}
		used[name] = r.LocalPath
		wanted[name] = true

		link := filepath.Join(ws, name)
		// Point at a RELATIVE target (../<uuid>) so the workspace survives a move
		// of the whole app-data dir.
		rel, rerr := filepath.Rel(ws, r.LocalPath)
		if rerr != nil {
			rel = r.LocalPath
		}
		// Replace an existing link if it points elsewhere; leave a correct one.
		if cur, lerr := os.Readlink(link); lerr == nil {
			if cur == rel {
				continue
			}
			_ = os.Remove(link)
		} else if _, serr := os.Lstat(link); serr == nil {
			// A non-symlink squatting the name — don't clobber real files.
			continue
		}
		_ = os.Symlink(rel, link)
	}

	// Prune stale symlinks (only symlinks — never touch real dirs/files).
	if entries, derr := os.ReadDir(ws); derr == nil {
		for _, e := range entries {
			if wanted[e.Name()] {
				continue
			}
			full := filepath.Join(ws, e.Name())
			fi, lerr := os.Lstat(full)
			if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
				continue // not a symlink we manage — never touch real dirs/files
			}
			// A managed symlink no longer in `wanted` (repo removed/renamed) → prune.
			_ = os.Remove(full)
		}
	}
	return ws, nil
}

// sanitizeRepoName derives a filesystem-safe, human-readable link name for a
// clone: the last segment of the GitLab project path (or the remote URL), with
// unsafe characters folded to '-'. Falls back to a short id when empty.
func sanitizeRepoName(projectName, remoteURL, id string) string {
	cand := strings.TrimSpace(projectName)
	if cand == "" {
		u := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(remoteURL), "/"), ".git")
		cand = u
	}
	// Keep only the last path segment ("group/sub/repo" → "repo").
	if i := strings.LastIndexAny(cand, "/:"); i >= 0 {
		cand = cand[i+1:]
	}
	cand = repoNameUnsafe.ReplaceAllString(cand, "-")
	cand = strings.Trim(cand, "-._")
	if cand == "" {
		cand = "repo-" + shortID(id)
	}
	return cand
}

// shortID returns the first 6 chars of a UUID-ish id (or the whole thing).
func shortID(id string) string {
	id = strings.ReplaceAll(strings.TrimSpace(id), "-", "")
	if len(id) > 6 {
		return id[:6]
	}
	if id == "" {
		return "x"
	}
	return id
}
