package grafanaquery

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInterpolateFormats(t *testing.T) {
	rng := TimeRange{From: time.UnixMilli(1000), To: time.UnixMilli(2000)}
	vars := map[string]VarValue{
		"single": {Values: []string{"api.v1"}},
		"multi":  {Values: []string{"a.b", "c"}, Multi: true},
		"all":    {Values: []string{"$__all"}, IncludeAll: true, AllOptions: []string{"x", "y"}},
		"allc":   {Values: []string{"$__all"}, IncludeAll: true, AllValue: ".*", AllOptions: []string{"x"}},
		"q":      {Values: []string{"it's"}, Multi: true},
	}
	cases := []struct{ format, in, want string }{
		{"regex", `up{job="$single"}`, `up{job="api.v1"}`},
		{"regex", `up{job=~"$multi"}`, `up{job=~"(a\\.b|c)"}`},
		{"regex", `up{job=~"${all}"}`, `up{job=~"(x|y)"}`},
		{"regex", `up{job=~"$allc"}`, `up{job=~".*"}`},
		{"sqlstring", `WHERE s IN ($multi)`, `WHERE s IN ('a.b','c')`},
		{"sqlstring", `WHERE s IN ($q)`, `WHERE s IN ('it''s')`},
		{"glob", `$multi`, `{a.b,c}`},
		{"regex", `${multi:csv}`, `a.b,c`},
		{"regex", `${multi:pipe}`, `a.b|c`},
		{"regex", `${multi:json}`, `["a.b","c"]`},
		{"regex", `[[multi:raw]]`, `a.b,c`},
		{"regex", `${single:queryparam}`, `var-api.v1`},
		{"regex", `$__from-${__to:date:seconds}`, `1000-2`},
		{"regex", `rate(x[$__rate_interval]) $__interval $__range`, `rate(x[$__rate_interval]) $__interval $__range`},
		{"sqlstring", `WHERE $__timeFilter(ts)`, `WHERE $__timeFilter(ts)`},
		{"regex", `$unknown stays`, `$unknown stays`},
	}
	for _, c := range cases {
		ip := &Interpolator{Vars: vars, Range: rng, DefaultFormat: c.format}
		if got := ip.Replace(c.in); got != c.want {
			t.Errorf("%s %q → %q, want %q", c.format, c.in, got, c.want)
		}
	}
}

func TestInterpolateTargetKeepsRefAndDatasource(t *testing.T) {
	ip := &Interpolator{Vars: map[string]VarValue{"ds": {Values: []string{"prom"}}, "j": {Values: []string{"x"}}}, DefaultFormat: "regex"}
	out := ip.InterpolateTarget(map[string]any{
		"refId": "$j", "datasource": map[string]any{"uid": "$ds"}, "expr": "up{job=\"$j\"}",
		"nested": map[string]any{"q": []any{"$j"}},
	})
	if out["refId"] != "$j" || out["datasource"].(map[string]any)["uid"] != "$ds" {
		t.Fatalf("refId/datasource must be untouched: %v", out)
	}
	if out["expr"] != `up{job="x"}` || out["nested"].(map[string]any)["q"].([]any)[0] != "x" {
		t.Fatalf("strings not interpolated: %v", out)
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 14, 37, 12, 0, time.UTC)
	cases := []struct {
		in   string
		up   bool
		want time.Time
	}{
		{"now", false, now},
		{"now-6h", false, now.Add(-6 * time.Hour)},
		{"now-1d/d", false, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)},
		{"now-1d/d", true, time.Date(2026, 10, 18, 23, 59, 59, 999e6, time.UTC)},
		{"now/w", false, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}, // a Monday
		{"1700000000000", false, time.UnixMilli(1700000000000)},
	}
	for _, c := range cases {
		got, err := ParseTime(c.in, now, c.up)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s (up=%v) → %v %v, want %v", c.in, c.up, got, err, c.want)
		}
	}
	if _, err := ParseTime("yesterday", now, false); err == nil {
		t.Error("expected error")
	}
	if _, err := ParseRange("now", "now-1h", now); err == nil {
		t.Error("from after to must fail")
	}
}

func TestCalcInterval(t *testing.T) {
	r := TimeRange{From: time.Unix(0, 0), To: time.Unix(6*3600, 0)}
	if got := CalcInterval(r, 1000, 0); got != 30*time.Second {
		t.Errorf("6h/1000 → %v, want 30s", got)
	}
	if got := CalcInterval(r, 1000, time.Minute); got != time.Minute {
		t.Errorf("min interval floor → %v", got)
	}
	if FormatDuration(90*time.Second) != "90s" || FormatDuration(2*time.Hour) != "2h" {
		t.Error("FormatDuration")
	}
	if d, _ := ParseDuration(">1m"); d != time.Minute {
		t.Error(">1m")
	}
	if d, _ := ParseDuration("1d"); d != 24*time.Hour {
		t.Error("1d")
	}
}

func TestCustomRegexSort(t *testing.T) {
	opts := parseCustom(`prod, test : t1, a\,b`)
	if len(opts) != 3 || opts[1].Text != "test" || opts[1].Value != "t1" || opts[2].Value != "a,b" {
		t.Fatalf("parseCustom: %+v", opts)
	}
	in := []Option{{"node-10", "node-10"}, {"node-2", "node-2"}, {"db-1", "db-1"}}
	f := ApplyRegex(in, `/node-(\d+)/`)
	if len(f) != 2 || f[0].Value != "10" {
		t.Fatalf("regex capture: %+v", f)
	}
	named := ApplyRegex(in, `/(?P<text>node)-(?P<value>\d+)/`)
	if named[0].Text != "node" || named[0].Value != "10" {
		t.Fatalf("named groups: %+v", named)
	}
	s := []Option{{"node-10", ""}, {"node-2", ""}, {"node-1", ""}}
	SortOptions(s, 7)
	if s[0].Text != "node-1" || s[2].Text != "node-10" {
		t.Fatalf("natural sort: %+v", s)
	}
	if len(ApplyRegex(in, `/(?=lookahead)/`)) != 3 {
		t.Fatal("uncompilable regex must not filter")
	}
}

func TestConvertV2Rows(t *testing.T) {
	var obj struct {
		Spec map[string]any `json:"spec"`
	}
	b, err := os.ReadFile("testdata/v2_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	d := ConvertV2(obj.Spec)
	panels := d["panels"].([]any)
	var rows, collapsed int
	var ts map[string]any
	for _, p := range panels {
		pm := p.(map[string]any)
		if pm["type"] == "row" {
			rows++
			if pm["collapsed"] == true {
				collapsed++
				if len(pm["panels"].([]any)) != 2 {
					t.Errorf("collapsed row should nest its 2 panels: %v", pm["panels"])
				}
			}
		}
		if pm["title"] == "HTTP requests / s ($job)" {
			ts = pm
		}
	}
	if rows != 2 || collapsed != 1 {
		t.Fatalf("rows=%d collapsed=%d", rows, collapsed)
	}
	if ts == nil || ts["type"] != "timeseries" {
		t.Fatalf("timeseries panel missing: %v", ts)
	}
	tg := ts["targets"].([]any)[0].(map[string]any)
	if !strings.Contains(tg["expr"].(string), "prometheus_http_requests_total") || tg["refId"] != "A" {
		t.Fatalf("target: %v", tg)
	}
	if ds := tg["datasource"].(map[string]any); ds["uid"] != "${ds}" || ds["type"] != "prometheus" {
		t.Fatalf("target datasource: %v", ds)
	}
	vars := d["templating"].(map[string]any)["list"].([]any)
	var job, inst map[string]any
	for _, v := range vars {
		vm := v.(map[string]any)
		switch vm["name"] {
		case "job":
			job = vm
		case "instance":
			inst = vm
		}
	}
	if job["type"] != "query" || job["multi"] != true || job["refresh"] != 1 {
		t.Fatalf("job var: %v", job)
	}
	if q, _ := job["query"].(map[string]any); q["query"] != "label_values(up, job)" {
		t.Fatalf("job query: %v", job["query"])
	}
	if inst["query"] != `label_values(up{job=~"$job"}, instance)` {
		t.Fatalf("legacy string query lost: %v", inst["query"])
	}
	if d["time"].(map[string]any)["from"] != "now-30m" || d["refresh"] != "30s" {
		t.Fatalf("time settings: %v %v", d["time"], d["refresh"])
	}
}

func TestConvertV2TabsAndAutoGrid(t *testing.T) {
	var obj struct {
		Spec map[string]any `json:"spec"`
	}
	b, _ := os.ReadFile("testdata/v2_tabs.json")
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	panels := ConvertV2(obj.Spec)["panels"].([]any)
	if panels[0].(map[string]any)["type"] != "row" || panels[0].(map[string]any)["title"] != "Auto grid" {
		t.Fatalf("first tab → row: %v", panels[0])
	}
	// 4 auto-grid items in 3 columns → widths 8, second line at y+9.
	g := panels[1].(map[string]any)["gridPos"].(map[string]any)
	g4 := panels[4].(map[string]any)["gridPos"].(map[string]any)
	if g["w"] != 8 || g4["x"] != 0 || g4["y"] != g["y"].(int)+9 {
		t.Fatalf("auto grid positions: %v %v", g, g4)
	}
	last := panels[len(panels)-1].(map[string]any)
	if last["gridPos"].(map[string]any)["w"] != 12 {
		t.Fatalf("grid tab item: %v", last)
	}
}

func TestPromToFrames(t *testing.T) {
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api","code":"200"},"values":[[1700000000,"1.5"],[1700000015,"NaN"]]}]}}`)
	frames, err := promToFrames(body, "A", "{{job}} {{code}}", 15*time.Second)
	if err != nil || len(frames) != 1 {
		t.Fatal(err, len(frames))
	}
	var f Frame
	_ = json.Unmarshal(frames[0], &f)
	if f.Schema.Fields[1].Config["displayNameFromDS"] != "api 200" || f.Data.Values[0][0].(float64) != 1700000000000 {
		t.Fatalf("frame: %s", frames[0])
	}
	if f.Data.Values[1][1] != nil {
		t.Fatalf("NaN should become null: %v", f.Data.Values[1])
	}
}

func TestNormalizeCookieValue(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                                 "abc",
		"grafana_session=abc":                 "abc",
		"foo=1; grafana_session=abc; bar=2":   "abc",
		" grafana_session = abc ; x=y":        "abc",
		"grafana_session_expiry=1; other=zzz": "grafana_session_expiry=1; other=zzz",
	} {
		if got := normalizeCookieValue(in, "grafana_session"); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// A regex that matches nothing must yield [] (JSON), never null — the FE
// variable picker crashed on null options.
func TestNoMatchIsEmptyNotNil(t *testing.T) {
	out := ApplyRegex([]Option{{"a", "a"}}, `/zzz/`)
	if out == nil {
		t.Fatal("ApplyRegex returned nil")
	}
	b, _ := json.Marshal(out)
	if string(b) != "[]" {
		t.Fatalf("got %s", b)
	}
}

// All with no known options (the variable's values couldn't be listed) must
// match everything, not nothing.
func TestAllWithoutOptionsMatchesEverything(t *testing.T) {
	all := map[string]VarValue{"svc": {Values: []string{"$__all"}, IncludeAll: true, Multi: true}}
	for format, want := range map[string]string{"regex": `up{svc=~".*"}`, "lucene": `up{svc=~"*"}`, "glob": `up{svc=~"*"}`} {
		ip := &Interpolator{Vars: all, DefaultFormat: format}
		if got := ip.Replace(`up{svc=~"$svc"}`); got != want {
			t.Errorf("%s: %q, want %q", format, got, want)
		}
	}
}

func TestIsPromLike(t *testing.T) {
	for _, ty := range []string{"prometheus", "victoriametrics-metrics-datasource", "grafana-amazonprometheus-datasource"} {
		if !IsPromLike(ty) {
			t.Errorf("%s should be prom-like", ty)
		}
	}
	if IsPromLike("elasticsearch") {
		t.Error("elasticsearch is not prom-like")
	}
}
