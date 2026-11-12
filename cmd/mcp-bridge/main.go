// Command patchman-mcp is a tiny stdio↔HTTP relay that lets an external MCP
// client (Claude Desktop) drive the PatchMan desktop app's embedded backend.
//
// The embedded BE exposes MCP at POST /api/mcp as Streamable HTTP with
// Stateless + JSONResponse, so each newline-delimited JSON-RPC message from
// stdin maps 1:1 to one HTTP POST returning one JSON object (or 202 for a
// notification). This relay is deliberately transport-agnostic: it never
// parses tool schemas or MCP semantics, which keeps it robust across MCP
// version changes. It reads the discovery file per message so a PatchMan
// restart on a fresh random port is picked up without restarting Claude.
//
// While the app isn't running the bridge answers initialize / ping /
// tools/list itself (an empty tool list), so the MCP client keeps the server
// connected instead of marking it failed for the rest of its session; once
// the app answers, it sends notifications/tools/list_changed and the client
// fetches the real tools.
//
// stdout discipline: ONLY newline-delimited JSON-RPC ever goes to stdout.
// Diagnostics go to stderr (Go's log default), which Claude Desktop surfaces
// in mcp-server-<name>.log. Writing anything else to stdout corrupts framing.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/mcpdisco"
	"patchman-be/internal/mcpinstr"
)

// buildVersion is stamped via -ldflags at build time (build-sidecar.mjs);
// kept for parity with the server binary. Diagnostics only.
var buildVersion = "dev"

// envelope pulls just enough out of a JSON-RPC message to classify it. An
// absent or null id means it's a notification (no response expected).
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

type bridge struct {
	httpc *http.Client
	out   *bufio.Writer
	outMu sync.Mutex // serializes stdout writes across per-message goroutines

	protoMu sync.RWMutex
	proto   string // MCP-Protocol-Version captured from the initialize response

	// The agent that started this bridge (its initialize clientInfo) and the
	// project it works in (the bridge's working directory's name — the agent
	// launches it there). Sent on every message: the desktop's MCP token is
	// shared by all local agents, so the token can't say which one is calling.
	clientMu    sync.RWMutex
	clientName  string
	clientTitle string
	project     string

	read func() (mcpdisco.Info, error) // discovery-file reader (per-message re-read)

	// Offline handling: alive probes a discovery URL; while the app is down a
	// single watcher polls every watchEvery and announces list_changed once it
	// answers.
	alive      func(mcpURL string) bool
	watchEvery time.Duration
	watchMu    sync.Mutex
	watching   bool
}

func main() {
	b := &bridge{
		// No client timeout: a legitimate 60s tool call (e.g. run_db_query)
		// must not be severed by the transport.
		httpc:      &http.Client{},
		out:        bufio.NewWriter(os.Stdout),
		read:       resolveReader(),
		project:    projectFromDir(),
		alive:      mcpdisco.Alive,
		watchEvery: 3 * time.Second,
	}

	// ReadBytes grows to any size, unlike bufio.Scanner's 64 KiB default —
	// large-but-bounded tool payloads (server default MaxRequestBodyBytes is
	// 4 MiB) would otherwise be truncated.
	in := bufio.NewReader(os.Stdin)
	var wg sync.WaitGroup
	for {
		line, err := in.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			// One goroutine per message so a slow tool call never blocks a
			// subsequent notifications/cancelled meant to abort it.
			wg.Add(1)
			go func(m []byte) { defer wg.Done(); b.handle(m) }(append([]byte(nil), msg...))
		}
		if err != nil { // EOF: Claude closed stdin
			break
		}
	}
	wg.Wait()
}

// resolveReader selects the discovery source once at startup: an explicit
// --discovery flag or PATCHMAN_MCP_DISCOVERY env override, else the default
// well-known path. The chosen source is re-read on every message.
func resolveReader() func() (mcpdisco.Info, error) {
	fs := flag.NewFlagSet("patchman-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	custom := fs.String("discovery", "", "path to the mcp.json discovery file")
	_ = fs.Parse(os.Args[1:]) // tolerate unknown args passed by the launcher

	path := *custom
	if path == "" {
		path = os.Getenv("PATCHMAN_MCP_DISCOVERY")
	}
	if path != "" {
		return func() (mcpdisco.Info, error) { return mcpdisco.ReadPath(path) }
	}
	return mcpdisco.Read
}

func (b *bridge) handle(msg []byte) {
	var env envelope
	_ = json.Unmarshal(msg, &env)
	isReq := len(env.ID) > 0 && string(env.ID) != "null"
	if env.Method == "initialize" {
		b.captureClient(msg)
	}

	info, err := b.read()
	if err != nil {
		// Missing/stale discovery file => the app isn't running. Never exit:
		// answer what can be answered here, a clean JSON-RPC error otherwise.
		// Notifications are simply dropped.
		b.offline(env, isReq, msg, "PatchMan desktop app is not running. Launch PatchMan and try again.")
		return
	}
	// Defense-in-depth against a tampered discovery file: only ever dial
	// loopback. The BE always binds 127.0.0.1.
	if !isLoopbackURL(info.URL) {
		if isReq {
			b.writeErr(env.ID, -32002, "PatchMan discovery file points at a non-loopback URL; refusing to connect.")
		}
		return
	}

	req, err := http.NewRequest(http.MethodPost, info.URL, bytes.NewReader(msg))
	if err != nil {
		if isReq {
			b.writeErr(env.ID, -32000, "PatchMan discovery URL is invalid: "+err.Error())
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// REQUIRED: the go-sdk stateless handler rejects a POST whose Accept
	// lacks BOTH application/json and text/event-stream, even in JSONResponse
	// mode. Missing this 400s every call.
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+info.Token)
	name, title := b.getClient()
	if name != "" || title != "" {
		req.Header.Set("X-PatchMan-Client-Name", headerSafe(name))
		req.Header.Set("X-PatchMan-Client-Title", headerSafe(title))
	}
	if b.project != "" {
		req.Header.Set("X-PatchMan-Project", b.project)
	}
	if v := b.getProto(); v != "" {
		// In stateless mode every POST is a fresh temporary session, so echo
		// the version Claude negotiated to avoid silent per-request skew.
		req.Header.Set("MCP-Protocol-Version", v)
	}

	resp, err := b.httpc.Do(req)
	if err != nil {
		b.offline(env, isReq, msg, "PatchMan is not reachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Notification-only POST => 202/204 with no body: relay nothing.
	if resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent ||
		len(bytes.TrimSpace(body)) == 0 {
		return
	}
	if resp.StatusCode >= 400 {
		if isReq {
			b.writeErr(env.ID, -32001, fmt.Sprintf("PatchMan MCP endpoint returned HTTP %d", resp.StatusCode))
		}
		return
	}
	b.captureProto(body) // grab result.protocolVersion from the initialize response
	b.writeLine(body)
}

// offline answers a message while the desktop app can't be reached and
// starts watching for it. initialize is answered here (tools capability with
// listChanged, the agent-memory instructions), ping with an empty result and
// tools/list with no tools; anything else gets errMsg.
func (b *bridge) offline(env envelope, isReq bool, msg []byte, errMsg string) {
	defer b.watch()
	if !isReq {
		return
	}
	switch env.Method {
	case "initialize":
		b.writeResult(env.ID, b.offlineInit(msg))
	case "ping":
		b.writeResult(env.ID, map[string]any{})
	case "tools/list":
		b.writeResult(env.ID, map[string]any{"tools": []any{}})
	default:
		b.writeErr(env.ID, -32000, errMsg)
	}
}

// knownProtocolVersions are the MCP versions the embedded backend's SDK
// speaks; offline initialize echoes the client's when it is one of them.
var knownProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// offlineInit is the initialize result given while the app is down.
func (b *bridge) offlineInit(msg []byte) map[string]any {
	var m struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(msg, &m)
	version := knownProtocolVersions[0]
	for _, v := range knownProtocolVersions {
		if v == m.Params.ProtocolVersion {
			version = v
		}
	}
	b.protoMu.Lock()
	b.proto = version
	b.protoMu.Unlock()
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "patchman", "title": "Patchman", "version": buildVersion},
		"instructions": "The PatchMan desktop app is not running yet: its tools appear by themselves once it starts.\n\n" +
			mcpinstr.MemoryInstructions,
	}
}

// watch starts (once) a poller that waits for the app to answer, then tells
// the client its tool list changed so it fetches the real tools.
func (b *bridge) watch() {
	b.watchMu.Lock()
	if b.watching || b.alive == nil {
		b.watchMu.Unlock()
		return
	}
	b.watching = true
	b.watchMu.Unlock()
	go func() {
		for {
			time.Sleep(b.watchEvery)
			info, err := b.read()
			if err != nil || !isLoopbackURL(info.URL) || !b.alive(info.URL) {
				continue
			}
			b.watchMu.Lock()
			b.watching = false
			b.watchMu.Unlock()
			b.writeLine([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
			return
		}
	}()
}

func (b *bridge) writeResult(id json.RawMessage, result any) {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	b.writeLine(out)
}

// captureClient records the agent's clientInfo from its initialize request.
func (b *bridge) captureClient(msg []byte) {
	var m struct {
		Params struct {
			ClientInfo struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	b.clientMu.Lock()
	b.clientName, b.clientTitle = m.Params.ClientInfo.Name, m.Params.ClientInfo.Title
	b.clientMu.Unlock()
}

func (b *bridge) getClient() (string, string) {
	b.clientMu.RLock()
	defer b.clientMu.RUnlock()
	return b.clientName, b.clientTitle
}

// projectFromDir is the working directory's name — the project the agent
// was started in — or "" for the home or root directory, where there is
// none. Only the name is sent, never the path.
func projectFromDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd = filepath.Clean(wd)
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == wd {
		return ""
	}
	name := filepath.Base(wd)
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return headerSafe(name)
}

// headerSafe keeps a header value free of control characters and at most
// 120 characters long.
func headerSafe(s string) string {
	out := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(out); len(r) > 120 {
		out = string(r[:120])
	}
	return out
}

// captureProto records result.protocolVersion. Only the initialize response
// carries it, so this is effectively init-only.
func (b *bridge) captureProto(body []byte) {
	var r struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &r) == nil && r.Result.ProtocolVersion != "" {
		b.protoMu.Lock()
		b.proto = r.Result.ProtocolVersion
		b.protoMu.Unlock()
	}
}

func (b *bridge) getProto() string {
	b.protoMu.RLock()
	defer b.protoMu.RUnlock()
	return b.proto
}

// writeLine emits exactly one newline-delimited JSON-RPC message. The body is
// compacted so an indented server response never splits into multiple stdio
// "lines" and corrupts framing for Claude's reader.
func (b *bridge) writeLine(body []byte) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		// Not compactable JSON: strip interior newlines so framing stays one
		// line per message.
		compact.Reset()
		compact.Write(bytes.ReplaceAll(bytes.TrimSpace(body), []byte("\n"), []byte(" ")))
	}
	b.outMu.Lock()
	defer b.outMu.Unlock()
	b.out.Write(compact.Bytes())
	b.out.WriteByte('\n')
	b.out.Flush()
}

func (b *bridge) writeErr(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	b.writeLine(out)
}

// isLoopbackURL reports whether raw targets localhost.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
