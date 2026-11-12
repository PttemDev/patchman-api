package grafanaquery

import (
	"reflect"
	"testing"
)

// Shapes seen on Percona PMM and imported community dashboards.

func pmmInfo() *ServerInfo {
	return &ServerInfo{Datasources: []Datasource{
		{ID: 1, UID: "PA58DA793C7250F1B", Name: "Metrics", Type: "prometheus", IsDefault: true},
		{ID: 2, UID: "ClickHouse", Name: "ClickHouse", Type: "grafana-clickhouse-datasource"},
		{ID: 3, UID: "loki-1", Name: "Loki", Type: "loki"},
		{ID: 4, UID: "loki-2", Name: "Loki B", Type: "loki"},
	}}
}

func TestResolveTargetDS(t *testing.T) {
	info := pmmInfo()
	cases := []struct {
		name                          string
		ref, refType, panel, panelTyp string
		want                          string
	}{
		{"own ref wins", "ClickHouse", "", "Metrics", "", "ClickHouse"},
		{"no ref uses panel", "", "", "ClickHouse", "", "ClickHouse"},
		{"unknown uid falls back to panel", "prometheus", "prometheus", "Metrics", "", "PA58DA793C7250F1B"},
		{"unknown uid, no panel, by type", "prometheus", "prometheus", "", "", "PA58DA793C7250F1B"},
		{"type only ref picks first of type", "", "loki", "", "", "loki-1"},
		{"type only panel", "", "", "", "loki", "loki-1"},
		{"nothing → default", "", "", "", "", "PA58DA793C7250F1B"},
		{"mixed keeps own ref", "loki-2", "", "-- Mixed --", "datasource", "loki-2"},
		{"mixed without ref → default", "", "", "-- Mixed --", "datasource", "PA58DA793C7250F1B"},
		{"mixed without ref, typed", "", "loki", "-- Mixed --", "datasource", "loki-1"},
	}
	for _, c := range cases {
		ds := resolveTargetDS(info, c.ref, c.refType, c.panel, c.panelTyp)
		got := ""
		if ds != nil {
			got = ds.UID
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if ds := resolveTargetDS(info, "${DS_GONE}", "", "", ""); ds != nil {
		t.Errorf("unresolved string ref must not guess, got %s", ds.UID)
	}
}

func TestEmptyTarget(t *testing.T) {
	cases := []struct {
		target map[string]any
		typ    string
		want   bool
	}{
		{map[string]any{"refId": "C"}, "prometheus", true},
		{map[string]any{"expr": " "}, "prometheus", true},
		{map[string]any{"expr": "up"}, "prometheus", false},
		{map[string]any{"expr": ""}, "loki", true},
		{map[string]any{"rawSql": ""}, "grafana-clickhouse-datasource", true},
		{map[string]any{"rawSql": "", "builderOptions": map[string]any{}}, "grafana-clickhouse-datasource", false},
		{map[string]any{"rawSql": "", "sql": map[string]any{}}, "postgres", false},
		{map[string]any{"format": "table"}, "postgres", false}, // no rawSql key at all: leave to Grafana
		{map[string]any{}, "elasticsearch", false},
	}
	for i, c := range cases {
		if got := emptyTarget(c.target, c.typ); got != c.want {
			t.Errorf("#%d %v (%s): got %v want %v", i, c.target, c.typ, got, c.want)
		}
	}
}

func TestResolveOrder(t *testing.T) {
	defs := []map[string]any{
		{"name": "service", "query": "label_values(up{node=~\"$node\"}, service)"},
		{"name": "slowq", "query": map[string]any{"rawSql": "SELECT q WHERE t > ${qthres}"}},
		{"name": "node", "query": "label_values(up, node)"},
		{"name": "qthres", "type": "custom", "query": "1,5,10"},
		nil,
		{"name": "self", "query": "$self"},
		{"name": "a", "query": "$b"},
		{"name": "b", "query": "[[a]]"}, // cycle: broken, both still resolved
	}
	got := resolveOrder(defs)
	pos := map[int]int{}
	for p, i := range got {
		pos[i] = p
	}
	if len(got) != len(defs) {
		t.Fatalf("order must visit every def: %v", got)
	}
	if pos[2] > pos[0] {
		t.Errorf("node must resolve before service: %v", got)
	}
	if pos[3] > pos[1] {
		t.Errorf("qthres must resolve before slowq: %v", got)
	}
	// Independent variables keep dashboard order.
	if !reflect.DeepEqual(got[:4], []int{2, 0, 3, 1}) {
		t.Errorf("order = %v", got)
	}
}

func TestParsePublicDashboardURL(t *testing.T) {
	cases := []struct{ in, root, token string }{
		{"https://g.example.com/public-dashboards/abc123", "https://g.example.com", "abc123"},
		{"https://g.example.com/public-dashboards/abc123?orgId=1#x", "https://g.example.com", "abc123"},
		{"https://h.example.com/graph/public-dashboards/tok/", "https://h.example.com/graph", "tok"},
		{"http://localhost:3000/public/dashboards/tok2", "http://localhost:3000", "tok2"},
	}
	for _, c := range cases {
		root, tok, err := ParsePublicDashboardURL(c.in)
		if err != nil || root != c.root || tok != c.token {
			t.Errorf("%s → (%q, %q, %v)", c.in, root, tok, err)
		}
	}
	for _, bad := range []string{"", "not a url", "https://g.example.com/d/uid/x", "https://g.example.com/public-dashboards/"} {
		if _, _, err := ParsePublicDashboardURL(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
