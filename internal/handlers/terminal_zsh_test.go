package handlers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func requireZsh(t *testing.T) string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	return zsh
}

// fakeHome makes a throwaway HOME so tests never touch the real dotfiles.
// Unless files has a .zshrc, a minimal one is added.
func fakeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files[".zshrc"]; !ok {
		files[".zshrc"] = "PS1='$ '\n"
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dir)
	for _, k := range []string{"ZDOTDIR", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return dir
}

type zshSession struct {
	t   *testing.T
	pty *os.File
	pid int
	mu  sync.Mutex
	buf strings.Builder
}

func startZsh(t *testing.T, zsh, home string) *zshSession {
	t.Helper()
	cmd := terminalCommand(zsh, home)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	s := &zshSession{t: t, pty: ptmx, pid: cmd.Process.Pid}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptmx.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(b[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = ptmx.Close()
	})
	return s
}

func (s *zshSession) send(in string) {
	s.t.Helper()
	if _, err := s.pty.WriteString(in); err != nil {
		s.t.Fatal(err)
	}
}

func (s *zshSession) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *zshSession) waitFor(sub string) string {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out := s.output(); strings.Contains(out, sub) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timeout waiting for %q; output so far: %q", sub, s.output())
	return ""
}

func TestTerminalZshLoadsUserFilesInOrder(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{
		".zshenv":   "echo PM_ENV\n",
		".zprofile": "echo PM_PROFILE\n",
		".zshrc":    "echo PM_RC; PS1='$ '\n",
		".zlogin":   "echo PM_LOGIN\n",
	})
	out := startZsh(t, zsh, home).waitFor("PM_LOGIN")
	last := -1
	for _, m := range []string{"PM_ENV", "PM_PROFILE", "PM_RC", "PM_LOGIN"} {
		i := strings.Index(out, m)
		if i <= last {
			t.Fatalf("%s out of order or missing in %q", m, out)
		}
		last = i
	}
}

func TestTerminalZshLeavesNoTrace(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{})
	s := startZsh(t, zsh, home)
	s.send(`print -r -- "S=$? Z=${ZDOTDIR-unset} P=${PATCHMAN_USER_ZDOTDIR-unset}"` + "\n")
	s.waitFor("S=0 Z=unset P=unset")
}

func TestTerminalZshHistfileFixedOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS /etc/zshrc only")
	}
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{})
	s := startZsh(t, zsh, home)
	s.send(`print -r -- "H=$HISTFILE"` + "\n")
	s.waitFor("H=" + home + "/.zsh_history")
}

func TestTerminalZshHonoursZdotdirFromZshenv(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{
		".zshenv":            "ZDOTDIR=$HOME/.config/zsh\n",
		".config/zsh/.zshrc": "echo PM_CUSTOM_RC; PS1='$ '\n",
	})
	s := startZsh(t, zsh, home)
	s.waitFor("PM_CUSTOM_RC")
	s.send(`print -r -- "Z=$ZDOTDIR"` + "\n")
	s.waitFor("Z=" + home + "/.config/zsh")
}

func TestTerminalZshReportsCommandAndCwd(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{})
	if err := os.Mkdir(filepath.Join(home, "a b%c"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := startZsh(t, zsh, home)
	s.send("cd 'a b%c'\n")
	out := s.waitFor("a%20b%25c\x07")
	if !strings.Contains(out, "\x1b]7;file://") {
		t.Fatalf("no OSC 7 in %q", out)
	}
	s.send("true\n")
	s.waitFor("\x1b]6973;true\x07")
}

func TestTerminalZshCommandReportIsOneLine(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{})
	s := startZsh(t, zsh, home)
	s.send("echo pm-a \\\npm-b\n")
	out := s.waitFor("\x07pm-a pm-b\r\n") // BEL ending the OSC 6973 report, right before the command output
	re := regexp.MustCompile("\x1b\\]6973;[^\x07\r\n]*pm-b\x07")
	if !re.MatchString(out) {
		t.Fatalf("no one-line OSC 6973 report in %q", out)
	}
}

func TestTerminalZshKeepsHistoryWhenKilled(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc": "HISTFILE=$HOME/.zsh_history; HISTSIZE=100; SAVEHIST=100; PS1='$ '\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-persist\n")
	s.waitFor("\x07pm-persist\r\n") // BEL ending the OSC 6973 report, right before the command output
	_ = syscall.Kill(s.pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(home, ".zsh_history"))
		if strings.Contains(string(b), "echo pm-persist") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("history line not written before SIGKILL")
}

const ghostRC = "HISTFILE=$HOME/.zsh_history; HISTSIZE=100; SAVEHIST=100; PS1='$ '\n"

func requireGhostZsh(t *testing.T) string {
	t.Helper()
	zsh := requireZsh(t)
	if exec.Command(zsh, "-fc", "autoload is-at-least; is-at-least 5.9").Run() != nil {
		t.Skip("zsh < 5.9")
	}
	return zsh
}

func TestTerminalZshGhostSuggestsNewestSingleLine(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-ghost-target\necho pm-ghost-multi \\\nsecond\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-ghost")
	out := s.waitFor("\x1b[90m")
	if strings.Contains(out, "pm-ghost-multi") {
		t.Fatalf("multi-line history entry suggested: %q", out)
	}
	s.send("\x1b[C")
	s.send("\r")
	s.waitFor("\x07pm-ghost-target\r\n")
}

func TestTerminalZshGhostIgnoredWithoutAccept(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-long-command\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-l")
	s.send("\r")
	s.waitFor("\x07pm-l\r\n")
}

func TestTerminalZshGhostStaysOffWithAutosuggestions(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC + "_zsh_autosuggest_start() { :; }\n",
		".zsh_history": "echo pm-ghost-target\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-ghost")
	s.waitFor("pm-ghost")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(s.output(), "\x1b[90m") {
		t.Fatalf("ghost active alongside zsh-autosuggestions: %q", s.output())
	}
}

func TestTerminalZshGhostPrefersNewestOfSeveral(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-n-old\necho pm-n-new\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-n")
	s.waitFor("\x1b[90m")
	s.send("\x1b[C")
	s.send("\r")
	s.waitFor("\x07pm-n-new\r\n")
}

func TestTerminalZshGhostSuggestsCommandsContainingN(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-new-thing\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-ne")
	s.waitFor("\x1b[90m")
	s.send("\x1b[C")
	s.send("\r")
	s.waitFor("\x07pm-new-thing\r\n")
}

func TestTerminalZshGhostAcceptsAllWithEndOfLine(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-eol-target\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-eol")
	s.waitFor("\x1b[90m")
	s.send("\x05")
	s.send("\r")
	s.waitFor("\x07pm-eol-target\r\n")
}

func TestTerminalZshGhostAcceptsOneWordWithForwardWord(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo pm-word-one pm-word-two\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo pm-word-o")
	s.waitFor("\x1b[90m")
	s.send("\x1bf")
	s.send("\r")
	s.waitFor("\x07pm-word-one\r\n") // a full accept would print "pm-word-one pm-word-two"
}

func TestTerminalZshGhostWordAcceptAtWordBoundary(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "echo aa bb\nls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("echo")
	s.waitFor("\x1b[90m")
	s.send("\x1bf")
	s.send("\r")
	s.waitFor("\x07aa\r\n") // forward-word stops at the space; the word must be taken
}

func TestTerminalZshEndOfLineAndForwardWordStillMove(t *testing.T) {
	zsh := requireGhostZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":       ghostRC,
		".zsh_history": "ls\n",
	})
	s := startZsh(t, zsh, home)
	s.send("cho pm-mid" + "\x01" + "e" + "\x05" + " x\r")
	s.waitFor("\x07pm-mid x\r\n")
	s.send("echo aa bb" + "\x01" + "\x1bf" + "Z\r")
	s.waitFor("\x07Zaa bb\r\n")
}

func TestTerminalZshRestoresPresetZdotdir(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{
		"zd/.zshrc": "echo PM_ZD_RC; PS1='$ '\n",
	})
	t.Setenv("ZDOTDIR", home+"/zd")
	s := startZsh(t, zsh, home)
	s.waitFor("PM_ZD_RC")
	s.send(`print -r -- "Z=$ZDOTDIR U=${PATCHMAN_ZDOTDIR_UNSET-unset}"` + "\n")
	s.waitFor("Z=" + home + "/zd U=unset")
}

func TestTerminalZshWrapperSurvivesShWordSplit(t *testing.T) {
	zsh := requireZsh(t)
	home := fakeHome(t, map[string]string{
		".zshrc":  "setopt shwordsplit; PS1='$ '\n",
		".zlogin": "echo PM_LOGIN_OK\n",
	})
	// The wrapper dir must contain a space (macOS "Application Support").
	cfg := filepath.Join(home, "Conf Dir")
	if err := os.Mkdir(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir, err := terminalZshDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dir, " ") {
		t.Skipf("config dir %q has no space on this OS", dir)
	}
	out := startZsh(t, zsh, home).waitFor("PM_LOGIN_OK")
	if strings.Contains(out, "no such file or directory") {
		t.Fatalf("unquoted source path broke: %q", out)
	}
}

func TestWriteTerminalZshIsAtomicAndComplete(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := writeTerminalZsh(dir); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := terminalZshFS.ReadDir("terminal_zsh")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		switch name {
		case "zshenv", "zprofile", "zshrc", "zlogin":
			name = "." + name
		}
		want[name] = true
		data, _ := terminalZshFS.ReadFile("terminal_zsh/" + e.Name())
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != string(data) {
			t.Fatalf("%s: missing or wrong content (%v)", name, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, name)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", name, fi.Mode().Perm())
		}
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if !want[f.Name()] {
			t.Fatalf("unexpected leftover %q", f.Name())
		}
	}
}
