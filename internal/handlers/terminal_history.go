package handlers

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Shell history for the terminal's History panel: the user's own zsh / bash /
// fish history file, newest first, one row per distinct command. Desktop only,
// like the shell itself — it reads a file on the user's machine.

type historyEntry struct {
	Command string `json:"command"`
	// Time is unix seconds when the shell recorded it (zsh extended history,
	// bash HISTTIMEFORMAT, fish); null when the file has no timestamps.
	Time *int64 `json:"time"`
}

// historyTailBytes caps how much of a (possibly huge) history file is read:
// the newest part is what the panel shows.
const historyTailBytes = 4 << 20

// History — GET /terminal/history?limit=500.
func (h *TerminalHandler) History(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "the terminal is only available on the desktop app"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	path, kind := historyFile(loginShell())
	if path == "" {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"entries": []historyEntry{}, "file": ""}})
		return
	}
	data, err := readTail(path, historyTailBytes)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"entries": []historyEntry{}, "file": path}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read history: " + err.Error()})
		return
	}
	var all []historyEntry
	switch kind {
	case "fish":
		all = parseFishHistory(data)
	case "bash":
		all = parseBashHistory(data)
	default:
		all = parseZshHistory(data)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entries": newestDistinct(all, limit), "file": path}})
}

// historyFile picks the history file of the user's shell: $HISTFILE when set,
// else the shell's default location.
func historyFile(shell string) (string, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	base := filepath.Base(shell)
	kind := "zsh"
	switch base {
	case "bash", "sh":
		kind = "bash"
	case "fish":
		kind = "fish"
	}
	if f := strings.TrimSpace(os.Getenv("HISTFILE")); f != "" && kind != "fish" {
		return f, kind
	}
	switch kind {
	case "bash":
		return filepath.Join(home, ".bash_history"), kind
	case "fish":
		dir := os.Getenv("XDG_DATA_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(dir, "fish", "fish_history"), kind
	}
	dir := strings.TrimSpace(os.Getenv("ZDOTDIR"))
	if dir == "" {
		dir = home
	}
	return filepath.Join(dir, ".zsh_history"), kind
}

func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > max {
		if _, err := f.Seek(st.Size()-max, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		// Drop the partial first line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
		return data, nil
	}
	return io.ReadAll(f)
}

// unmetafy undoes zsh's history encoding: a 0x83 byte marks the next byte as
// XOR 0x20 (how zsh stores non-ASCII text such as ı, ş, ğ).
func unmetafy(b []byte) []byte {
	if bytes.IndexByte(b, 0x83) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == 0x83 && i+1 < len(b) {
			i++
			out = append(out, b[i]^0x20)
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// parseZshHistory reads plain and extended (": <ts>:<dur>;cmd") lines; a line
// ending in a backslash continues on the next one (multi-line commands).
func parseZshHistory(data []byte) []historyEntry {
	lines := strings.Split(string(unmetafy(data)), "\n")
	var out []historyEntry
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = line[:len(line)-1] + "\n" + lines[i]
		}
		e := historyEntry{Command: line}
		if strings.HasPrefix(line, ": ") {
			if semi := strings.IndexByte(line, ';'); semi > 0 {
				meta := line[2:semi]
				if colon := strings.IndexByte(meta, ':'); colon > 0 {
					if ts, err := strconv.ParseInt(meta[:colon], 10, 64); err == nil {
						e.Time = &ts
						e.Command = line[semi+1:]
					}
				}
			}
		}
		if strings.TrimSpace(e.Command) != "" {
			out = append(out, e)
		}
	}
	return out
}

// parseBashHistory: one command per line; "#<unix>" lines (HISTTIMEFORMAT)
// time the command after them.
func parseBashHistory(data []byte) []historyEntry {
	var out []historyEntry
	var ts *int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			if v, err := strconv.ParseInt(line[1:], 10, 64); err == nil {
				ts = &v
				continue
			}
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, historyEntry{Command: line, Time: ts})
		ts = nil
	}
	return out
}

// parseFishHistory reads fish's "- cmd: …" / "  when: …" records.
func parseFishHistory(data []byte) []historyEntry {
	var out []historyEntry
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "- cmd: "):
			cmd := strings.ReplaceAll(strings.TrimPrefix(line, "- cmd: "), `\n`, "\n")
			out = append(out, historyEntry{Command: strings.ReplaceAll(cmd, `\\`, `\`)})
		case strings.HasPrefix(line, "  when: ") && len(out) > 0:
			if v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "  when: ")), 10, 64); err == nil {
				out[len(out)-1].Time = &v
			}
		}
	}
	return out
}

// newestDistinct returns up to limit entries, newest first, each command once
// (its latest use).
func newestDistinct(all []historyEntry, limit int) []historyEntry {
	seen := make(map[string]bool, len(all))
	out := make([]historyEntry, 0, min(limit, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		cmd := strings.TrimRight(all[i].Command, " \t")
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		e := all[i]
		e.Command = cmd
		out = append(out, e)
	}
	return out
}
