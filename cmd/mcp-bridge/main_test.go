package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/mcpdisco"
)

// The bridge names its agent (from initialize) and its project (the working
// directory's name, never the path) on every message it forwards.
func TestBridgeSendsAgentAndProjectHeaders(t *testing.T) {
	var mu sync.Mutex
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "getman")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var out bytes.Buffer
	b := &bridge{
		httpc:   srv.Client(),
		out:     bufio.NewWriter(&out),
		read:    func() (mcpdisco.Info, error) { return mcpdisco.Info{URL: srv.URL, Token: "tok"}, nil },
		project: projectFromDir(),
	}
	b.handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"claude-code","version":"2"}}}`))
	b.handle([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall","arguments":{"q":"x"}}}`))

	if len(got) != 2 {
		t.Fatalf("forwarded %d messages", len(got))
	}
	for i, h := range got {
		if h.Get("X-PatchMan-Client-Name") != "claude-code" || h.Get("X-PatchMan-Project") != "getman" {
			t.Errorf("message %d headers: name=%q project=%q", i, h.Get("X-PatchMan-Client-Name"), h.Get("X-PatchMan-Project"))
		}
	}
}

func TestProjectFromDirSkipsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	t.Chdir(home)
	if p := projectFromDir(); p != "" {
		t.Errorf("home dir gave project %q", p)
	}
}

// While the desktop app is down the bridge answers initialize itself (so the
// client keeps the server connected), lists no tools, errors on tool calls,
// and announces tools/list_changed exactly once when the app comes up.
func TestBridgeOfflineThenAppStarts(t *testing.T) {
	var mu sync.Mutex
	up := false
	var forwarded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		mu.Lock()
		forwarded = append(forwarded, env.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"result":{"tools":[{"name":"recall"}]}}`))
	}))
	defer srv.Close()

	var out syncBuffer
	b := &bridge{
		httpc: srv.Client(),
		out:   bufio.NewWriter(&out),
		read: func() (mcpdisco.Info, error) {
			mu.Lock()
			defer mu.Unlock()
			if !up {
				return mcpdisco.Info{}, os.ErrNotExist
			}
			return mcpdisco.Info{URL: srv.URL, Token: "tok"}, nil
		},
		alive:      func(string) bool { mu.Lock(); defer mu.Unlock(); return up },
		watchEvery: 20 * time.Millisecond,
	}

	b.handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code"}}}`))
	b.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	b.handle([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	b.handle([]byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	b.handle([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall"}}`))
	lines := out.lines()
	if len(lines) != 4 {
		t.Fatalf("offline answers = %d: %v", len(lines), lines)
	}
	var init struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil || init.ID != 1 || init.Result.ProtocolVersion != "2025-06-18" ||
		!init.Result.Capabilities.Tools.ListChanged || !strings.Contains(init.Result.Instructions, "recall(q)") {
		t.Fatalf("offline initialize = %s", lines[0])
	}
	if lines[1] != `{"id":2,"jsonrpc":"2.0","result":{"tools":[]}}` || lines[2] != `{"id":3,"jsonrpc":"2.0","result":{}}` ||
		!strings.Contains(lines[3], `"id":4`) || !strings.Contains(lines[3], "not running") {
		t.Fatalf("offline answers = %v", lines)
	}
	if b.getProto() != "2025-06-18" {
		t.Fatalf("negotiated version not kept: %q", b.getProto())
	}

	// The app starts: one list_changed, then calls are forwarded.
	mu.Lock()
	up = true
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for len(out.lines()) < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // no second announcement
	lines = out.lines()
	if len(lines) != 5 || lines[4] != `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` {
		t.Fatalf("after start = %v", lines)
	}
	b.handle([]byte(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`))
	mu.Lock()
	got := strings.Join(forwarded, ",")
	mu.Unlock()
	if got != "tools/list" || !strings.Contains(out.lines()[5], `"recall"`) {
		t.Fatalf("forwarded=%s last=%s", got, out.lines()[5])
	}
}

// An unknown client protocol version gets the SDK's latest one offline.
func TestBridgeOfflineUnknownProtocolVersion(t *testing.T) {
	var out syncBuffer
	b := &bridge{out: bufio.NewWriter(&out), read: func() (mcpdisco.Info, error) { return mcpdisco.Info{}, os.ErrNotExist }}
	b.handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`))
	if l := out.lines(); len(l) != 1 || !strings.Contains(l[0], `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("got %v", l)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer that splits written lines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s.buf.String()), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
