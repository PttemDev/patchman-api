package mcp

import (
	"strings"
	"testing"
)

func TestCompactServiceMap(t *testing.T) {
	body := `{"data":{"nodes":[
		{"id":"shop/Deployment/api","kind":"service","subtype":"http","namespace":"shop","name":"api","health":"warning","stats":{"rps":12.5,"errorRate":0.02,"p95Ms":80,"hasRed":true}},
		{"id":"shop/StatefulSet/postgres","kind":"datastore","subtype":"postgres","namespace":"shop","name":"postgres","health":"ok","stats":{"rps":0,"errorRate":0},"connection":{"tool":"db","name":"Orders DB"}}],
		"edges":[
		{"from":"shop/Deployment/api","to":"shop/StatefulSet/postgres","protocol":"postgres","flow":"request","sources":["config","coroot"],"stats":{"rps":30,"errorRate":0.001,"p95Ms":4,"hasRed":true}},
		{"from":"shop/Deployment/api","to":"shop/StatefulSet/postgres","protocol":"postgres","flow":"request","sources":["config"],"stats":{"rps":0,"errorRate":0}}],
		"sources":{"config":{"ok":true,"notes":["secrets_skipped:1"]},"apm:x":{"ok":false,"error":"Coroot rejected the API key."}}}}`
	out := compactServiceMap(body)
	for _, want := range []string{
		"2 nodes, 2 edges",
		"- apm:x: error: Coroot rejected the API key.",
		"- config: ok (secrets_skipped:1)",
		`shop/postgres [datastore/postgres] ok · PatchMan db connection "Orders DB"`,
		"shop/api -> shop/postgres [postgres, request] 30.00 rps · 0.1% err · p95 4ms; config,coroot",
		"(declared, no traffic data); config",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
