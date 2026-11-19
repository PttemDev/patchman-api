// Package manifest parses and lints a plugin's manifest.json
// (PLUGIN_MARKETPLACE_RFC §6.1, §14.4 item 6). Parse is strict: anything the
// runtime would have to guess about is an error, so a plugin either loads with
// exactly the permissions it declared or not at all.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"golang.org/x/net/idna"
)

// Limits the manifest may ask for; the runtime never exceeds them.
const (
	MaxWasmBytes        = 32 << 20 // §14.4 item 5: compile-DoS cap
	DefaultMemoryMB     = 32
	MaxMemoryMB         = 256
	DefaultTimeoutSec   = 30
	MaxTimeoutSec       = 120
	DefaultConcurrency  = 4
	MaxConcurrency      = 16
	MaxToolDescription  = 1024 // third-party text sent to agents (RFC §7)
	MaxOpDescription    = 1024
	MaxToolSchemaBytes  = 16 << 10 // an MCP tool's input schema is third-party text too
	maxToolNameLen      = 64       // MCP clients commonly cap tool names at 64
	connectionRefPrefix = "connection."
)

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*){2,}$`)
	semverRe  = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$`)
	opRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	fieldRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	prefixRe  = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	reserveOp = map[string]bool{"_start": true, "_initialize": true, "main": true}
)

// Manifest is the parsed, validated manifest.
type Manifest struct {
	ID          string        `json:"id"`
	Version     string        `json:"version"`
	Name        string        `json:"name"`
	Publisher   string        `json:"publisher"`
	Description string        `json:"description,omitempty"`
	Engine      string        `json:"engine,omitempty"`
	Connection  Connection    `json:"connection"`
	Permissions Permissions   `json:"permissions"`
	Limits      Limits        `json:"limits"`
	Ops         map[string]Op `json:"ops"`
	MCP         MCP           `json:"mcp"`
	UI          *UI           `json:"ui,omitempty"`
}

// UI is the declarative surface (RFC §16 P4b): which ops the plugin tab
// offers, in what order, and how to show each result. Custom iframe UI
// ("entry") is P4c and not accepted yet.
type UI struct {
	Views []View `json:"views,omitempty"`
	// Entry is the custom UI page, a file under ui/ (e.g. "ui/index.html").
	// It runs in a sandboxed iframe served from the backend (RFC §16.3).
	Entry string `json:"entry,omitempty"`
}

type View struct {
	Op     string `json:"op"`
	Title  string `json:"title,omitempty"`
	Result string `json:"result,omitempty"` // json (default) | table | text
}

type Connection struct {
	Schema       json.RawMessage `json:"schema,omitempty"`
	SecretFields []string        `json:"secretFields,omitempty"`
}

type Permissions struct {
	Net []NetRule `json:"net,omitempty"`
}

// NetRule is either a fixed host ("host") or a host taken from the
// connection's config ("from": "connection.<field>"). Ports are a fixed list
// or, with PortsFrom, a connection field.
type NetRule struct {
	Host      string `json:"host,omitempty"`
	From      string `json:"from,omitempty"`
	Ports     []int  `json:"ports,omitempty"`
	PortsFrom string `json:"portsFrom,omitempty"`
	Private   bool   `json:"private,omitempty"`
}

type Limits struct {
	MemoryMB       int `json:"memoryMB,omitempty"`
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	Concurrency    int `json:"concurrency,omitempty"`
}

type Op struct {
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	Write       bool            `json:"write,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type MCP struct {
	Prefix string `json:"prefix,omitempty"`
	Tools  []Tool `json:"tools,omitempty"`
}

type Tool struct {
	Name        string `json:"name"`
	Op          string `json:"op"`
	Description string `json:"description"`
}

// ToolName is the name agents see: plugin_<prefix>_<name>. The fixed
// "plugin_" head means a plugin can never shadow a built-in tool.
func (m *Manifest) ToolName(t Tool) string { return "plugin_" + m.MCP.Prefix + "_" + t.Name }

// SecretField reports whether name is a declared secret field.
func (m *Manifest) SecretField(name string) bool {
	for _, f := range m.Connection.SecretFields {
		if f == name {
			return true
		}
	}
	return false
}

// Parse decodes and lints raw manifest bytes.
func Parse(raw []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if !idRe.MatchString(m.ID) || len(m.ID) > 128 {
		add("id %q must be reverse-DNS (e.g. dev.acme.clickhouse), lowercase", m.ID)
	}
	if !semverRe.MatchString(m.Version) {
		add("version %q is not semver", m.Version)
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 80 {
		add("name is required (≤80 chars)")
	}
	if strings.TrimSpace(m.Publisher) == "" || len(m.Publisher) > 80 {
		add("publisher is required (≤80 chars)")
	}

	// Connection schema + secret fields.
	props := map[string]bool{}
	if len(m.Connection.Schema) > 0 {
		var s jsonschema.Schema
		if err := json.Unmarshal(m.Connection.Schema, &s); err != nil {
			add("connection.schema: %v", err)
		} else if _, err := s.Resolve(nil); err != nil {
			add("connection.schema: %v", err)
		} else {
			if s.Type != "object" {
				add("connection.schema.type must be \"object\"")
			}
			for k := range s.Properties {
				props[k] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, f := range m.Connection.SecretFields {
		if !fieldRe.MatchString(f) {
			add("secret field %q: invalid name", f)
		}
		if seen[f] {
			add("secret field %q listed twice", f)
		}
		if len(props) > 0 && !props[f] {
			add("secret field %q is not in connection.schema", f)
		}
		seen[f] = true
	}

	// Network rules: normalize so a declared host can never silently miss.
	for i := range m.Permissions.Net {
		r := &m.Permissions.Net[i]
		switch {
		case r.Host != "" && r.From != "":
			add("permissions.net[%d]: set host or from, not both", i)
		case r.Host != "":
			h, err := NormalizeHost(r.Host, true)
			if err != nil {
				add("permissions.net[%d].host: %v", i, err)
			}
			r.Host = h
		case r.From != "":
			if err := checkRef(r.From, props, m.Connection.SecretFields); err != nil {
				add("permissions.net[%d].from: %v", i, err)
			}
		default:
			add("permissions.net[%d]: host or from is required", i)
		}
		switch {
		case len(r.Ports) > 0 && r.PortsFrom != "":
			add("permissions.net[%d]: set ports or portsFrom, not both", i)
		case r.PortsFrom != "":
			if err := checkRef(r.PortsFrom, props, m.Connection.SecretFields); err != nil {
				add("permissions.net[%d].portsFrom: %v", i, err)
			}
		case len(r.Ports) == 0:
			add("permissions.net[%d]: ports or portsFrom is required", i)
		}
		for _, p := range r.Ports {
			if p < 1 || p > 65535 {
				add("permissions.net[%d]: port %d out of range", i, p)
			}
		}
	}

	// Limits.
	l := &m.Limits
	if l.MemoryMB == 0 {
		l.MemoryMB = DefaultMemoryMB
	}
	if l.TimeoutSeconds == 0 {
		l.TimeoutSeconds = DefaultTimeoutSec
	}
	if l.Concurrency == 0 {
		l.Concurrency = DefaultConcurrency
	}
	if l.MemoryMB < 1 || l.MemoryMB > MaxMemoryMB {
		add("limits.memoryMB must be 1..%d", MaxMemoryMB)
	}
	if l.TimeoutSeconds < 1 || l.TimeoutSeconds > MaxTimeoutSec {
		add("limits.timeoutSeconds must be 1..%d", MaxTimeoutSec)
	}
	if l.Concurrency < 1 || l.Concurrency > MaxConcurrency {
		add("limits.concurrency must be 1..%d", MaxConcurrency)
	}

	// Ops.
	if len(m.Ops) == 0 {
		add("ops: at least one op is required")
	}
	for name, op := range m.Ops {
		if !opRe.MatchString(name) || reserveOp[name] {
			add("op %q: invalid name", name)
		}
		if op.Stream {
			add("op %q: stream ops are not supported yet (RFC §15)", name)
		}
		if len(op.Description) > MaxOpDescription {
			add("op %q: description over %d chars", name, MaxOpDescription)
		}
		for _, sch := range []struct {
			k string
			v json.RawMessage
		}{{"input", op.Input}, {"output", op.Output}} {
			if len(sch.v) == 0 {
				continue
			}
			var s jsonschema.Schema
			if err := json.Unmarshal(sch.v, &s); err != nil {
				add("op %q %s schema: %v", name, sch.k, err)
				continue
			}
			if _, err := s.Resolve(nil); err != nil {
				add("op %q %s schema: %v", name, sch.k, err)
			}
			if sch.k == "input" && s.Type != "object" {
				add("op %q input schema type must be \"object\"", name)
			}
		}
	}

	// MCP tools.
	if len(m.MCP.Tools) > 0 {
		if m.MCP.Prefix == "" {
			m.MCP.Prefix = defaultPrefix(m.ID)
		}
		if !prefixRe.MatchString(m.MCP.Prefix) {
			add("mcp.prefix %q must be [a-z][a-z0-9]{0,15}", m.MCP.Prefix)
		}
		names := map[string]bool{}
		for _, t := range m.MCP.Tools {
			if !opRe.MatchString(t.Name) || t.Name == "connections" {
				add("mcp tool %q: invalid or reserved name", t.Name)
			}
			if names[t.Name] {
				add("mcp tool %q listed twice", t.Name)
			}
			names[t.Name] = true
			if len(m.ToolName(t)) > maxToolNameLen {
				add("mcp tool %q: full name %q over %d chars", t.Name, m.ToolName(t), maxToolNameLen)
			}
			op, ok := m.Ops[t.Op]
			switch {
			case !ok:
				add("mcp tool %q: op %q not declared", t.Name, t.Op)
			case len(op.Input) > MaxToolSchemaBytes:
				add("mcp tool %q: op %q input schema over %d bytes (sent to agents)", t.Name, t.Op, MaxToolSchemaBytes)
			case op.Sensitive:
				// LocalGate skips step-up for in-process calls (RFC D8).
				add("mcp tool %q: op %q is sensitive and cannot be an MCP tool", t.Name, t.Op)
			}
			if strings.TrimSpace(t.Description) == "" || len(t.Description) > MaxToolDescription {
				add("mcp tool %q: description required (≤%d chars)", t.Name, MaxToolDescription)
			}
		}
	}

	// Declarative views and the custom UI entry.
	if m.UI != nil && m.UI.Entry != "" {
		if !strings.HasPrefix(m.UI.Entry, "ui/") || !strings.HasSuffix(m.UI.Entry, ".html") || !uiPathOK(strings.TrimPrefix(m.UI.Entry, "ui/")) {
			add("ui.entry %q must be an .html file under ui/", m.UI.Entry)
		}
	}
	if m.UI != nil {
		seenView := map[string]bool{}
		for i := range m.UI.Views {
			v := &m.UI.Views[i]
			if _, ok := m.Ops[v.Op]; !ok {
				add("ui.views[%d]: op %q not declared", i, v.Op)
			}
			if seenView[v.Op] {
				add("ui.views[%d]: op %q listed twice", i, v.Op)
			}
			seenView[v.Op] = true
			if len(v.Title) > 80 {
				add("ui.views[%d]: title over 80 chars", i)
			}
			switch v.Result {
			case "":
				v.Result = "json"
			case "json", "table", "text":
			default:
				add("ui.views[%d]: result must be json, table or text", i)
			}
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, errors.New("manifest: " + strings.Join(errs, "; "))
	}
	return &m, nil
}

func checkRef(ref string, props map[string]bool, secrets []string) error {
	if !strings.HasPrefix(ref, connectionRefPrefix) {
		return fmt.Errorf("%q must start with %q", ref, connectionRefPrefix)
	}
	f := strings.TrimPrefix(ref, connectionRefPrefix)
	if !fieldRe.MatchString(f) {
		return fmt.Errorf("%q: invalid field", ref)
	}
	for _, s := range secrets {
		if s == f {
			return fmt.Errorf("%q is a secret field", ref)
		}
	}
	if len(props) > 0 && !props[f] {
		return fmt.Errorf("%q is not in connection.schema", ref)
	}
	return nil
}

// UIContentTypes are the file types a UI bundle may hold, and how they are
// served (fixed by extension; the browser is told not to sniff).
var UIContentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".woff": "font/woff",
	".woff2": "font/woff2", ".ttf": "font/ttf", ".txt": "text/plain; charset=utf-8", ".map": "application/json",
}

// UI bundle limits.
const (
	MaxUIFiles = 200
	MaxUIBytes = 10 << 20
)

var uiSegRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)

// UIPathOK reports whether p (relative to ui/) is a clean bundle path with an
// allowed extension: no "..", no hidden files, no backslashes.
func UIPathOK(p string) bool { return uiPathOK(p) }

func uiPathOK(p string) bool {
	if p == "" || len(p) > 400 || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if !uiSegRe.MatchString(seg) || strings.Contains(seg, "..") {
			return false
		}
	}
	dot := strings.LastIndex(p, ".")
	if dot < 0 {
		return false
	}
	_, ok := UIContentTypes[strings.ToLower(p[dot:])]
	return ok
}

// RefField returns the connection field a "connection.<field>" ref names.
func RefField(ref string) string { return strings.TrimPrefix(ref, connectionRefPrefix) }

func defaultPrefix(id string) string {
	seg := id[strings.LastIndex(id, ".")+1:]
	seg = strings.ReplaceAll(seg, "-", "")
	if len(seg) > 16 {
		seg = seg[:16]
	}
	return seg
}

// NormalizeHost lowercases, strips a trailing dot and converts IDNA to
// ASCII. Bracketed or bare IP literals are kept canonical. allowWildcard
// accepts a leading "*." (fixed manifest hosts only).
func NormalizeHost(h string, allowWildcard bool) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errors.New("empty host")
	}
	if strings.ContainsAny(h, "/:@?#% ") && !strings.HasPrefix(h, "[") {
		if _, err := netip.ParseAddr(h); err != nil {
			return "", fmt.Errorf("%q: host only, no scheme, port, path or user", h)
		}
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().String(), nil
	}
	if strings.HasPrefix(h, "[") || strings.Contains(h, ":") {
		return "", fmt.Errorf("%q: bad IP literal", h)
	}
	wild := false
	if strings.HasPrefix(h, "*.") {
		if !allowWildcard {
			return "", fmt.Errorf("%q: wildcard not allowed here", h)
		}
		wild, h = true, h[2:]
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", fmt.Errorf("%q: %v", h, err)
	}
	// Real TLDs start with a letter; "2130706433" or "0x7f.1" are IP
	// spellings some resolvers accept, never names.
	tld := ascii[strings.LastIndex(ascii, ".")+1:]
	if tld == "" || tld[0] < 'a' || tld[0] > 'z' || strings.HasPrefix(tld, "0x") {
		return "", fmt.Errorf("%q: numeric host is not a valid name", h)
	}
	if wild {
		if strings.Count(ascii, ".") < 1 {
			return "", fmt.Errorf("%q: wildcard needs a registrable domain", h)
		}
		return "*." + ascii, nil
	}
	return ascii, nil
}

// ParsePort parses a port value taken from a connection field.
func ParsePort(v string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %q out of range", v)
	}
	return p, nil
}
