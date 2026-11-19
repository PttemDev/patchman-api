package manifest

import (
	"strings"
	"testing"
)

const good = `{
  "id": "dev.acme.httpdemo", "version": "1.2.0", "name": "HTTP demo", "publisher": "acme",
  "connection": {
    "schema": {"type": "object", "properties": {
      "host": {"type": "string"}, "port": {"type": "string"}, "token": {"type": "string"}},
      "required": ["host"]},
    "secretFields": ["token"]
  },
  "permissions": {"net": [
    {"host": "API.Example.com.", "ports": [443]},
    {"from": "connection.host", "portsFrom": "connection.port", "private": true}
  ]},
  "ops": {
    "search": {"description": "search", "input": {"type": "object", "properties": {"path": {"type": "string"}}}},
    "drop": {"write": true, "sensitive": true}
  },
  "mcp": {"tools": [{"name": "search", "op": "search", "description": "Search things."}]}
}`

func TestParseGood(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if m.Permissions.Net[0].Host != "api.example.com" {
		t.Errorf("host not normalized: %q", m.Permissions.Net[0].Host)
	}
	if m.MCP.Prefix != "httpdemo" || m.ToolName(m.MCP.Tools[0]) != "plugin_httpdemo_search" {
		t.Errorf("tool name = %q", m.ToolName(m.MCP.Tools[0]))
	}
	if m.Limits.MemoryMB != DefaultMemoryMB || m.Limits.TimeoutSeconds != DefaultTimeoutSec || m.Limits.Concurrency != DefaultConcurrency {
		t.Errorf("defaults not applied: %+v", m.Limits)
	}
	if !m.SecretField("token") || m.SecretField("host") {
		t.Error("SecretField")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"bad id":             {`"dev.acme.httpdemo"`, `"Acme"`, "reverse-DNS"},
		"bad version":        {`"1.2.0"`, `"1.2"`, "semver"},
		"unknown field":      {`"publisher": "acme",`, `"publisher": "acme", "evil": 1,`, "unknown field"},
		"host with scheme":   {`"API.Example.com."`, `"https://api.example.com"`, "host only"},
		"host with port":     {`"API.Example.com."`, `"api.example.com:443"`, "host only"},
		"numeric host":       {`"API.Example.com."`, `"2130706433"`, "numeric host"},
		"bad port":           {`[443]`, `[0]`, "out of range"},
		"from secret":        {`"connection.host"`, `"connection.token"`, "secret field"},
		"from unknown":       {`"connection.host"`, `"connection.nope"`, "not in connection.schema"},
		"secret not in sch":  {`["token"]`, `["password"]`, "not in connection.schema"},
		"sensitive mcp tool": {`"op": "search"`, `"op": "drop"`, "sensitive"},
		"undeclared op tool": {`"op": "search"`, `"op": "nope"`, "not declared"},
		"stream op":          {`"drop": {"write": true`, `"drop": {"stream": true, "write": true`, "stream"},
		"reserved op":        {`"search": {"description"`, `"_start": {"description"`, "invalid name"},
		"memory cap":         {`"permissions"`, `"limits": {"memoryMB": 4096}, "permissions"`, "memoryMB"},
		"view unknown op":    {`"mcp": {`, `"ui": {"views": [{"op": "nope"}]}, "mcp": {`, "not declared"},
		"view bad result":    {`"mcp": {`, `"ui": {"views": [{"op": "search", "result": "chart"}]}, "mcp": {`, "result must be"},
		"ui entry outside":   {`"mcp": {`, `"ui": {"entry": "index.html"}, "mcp": {`, "must be an .html file under ui/"},
		"ui entry traversal": {`"mcp": {`, `"ui": {"entry": "ui/../x.html"}, "mcp": {`, "must be an .html file under ui/"},
		"long description":   {`"Search things."`, `"` + strings.Repeat("x", MaxToolDescription+1) + `"`, "description"},
	}
	for name, c := range cases {
		raw := strings.Replace(good, c.from, c.to, 1)
		if raw == good {
			t.Fatalf("%s: replacement did not apply", name)
		}
		_, err := Parse([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want %q", name, err, c.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"Example.COM.":    "example.com",
		"bücher.example":  "xn--bcher-kva.example",
		"[::1]":           "::1",
		"::ffff:10.0.0.1": "10.0.0.1",
		"*.example.com":   "*.example.com",
		"10.0.0.5":        "10.0.0.5",
	}
	for in, want := range ok {
		got, err := NormalizeHost(in, true)
		if err != nil || got != want {
			t.Errorf("%q: got %q err %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a/b", "user@host", "*.com", "[::1", "2130706433", "0x7f.1"} {
		if got, err := NormalizeHost(in, true); err == nil {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
	if _, err := NormalizeHost("*.example.com", false); err == nil {
		t.Error("wildcard accepted where not allowed")
	}
}

func TestUIPathOK(t *testing.T) {
	for _, p := range []string{"index.html", "js/app.js", "img/logo.svg", "a-b_c.min.css"} {
		if !UIPathOK(p) {
			t.Errorf("%q rejected", p)
		}
	}
	for _, p := range []string{"", "../x.js", "a/../b.js", ".hidden.js", "x.exe", "noext", "a\\b.js", "a//b.js", "x.html/"} {
		if UIPathOK(p) {
			t.Errorf("%q accepted", p)
		}
	}
}
