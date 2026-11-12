package grafanaquery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
)

// Live tests against a real Grafana, gated by PATCHMAN_TEST_GRAFANA (base
// URL) + PATCHMAN_TEST_GRAFANA_ADMIN ("user:pass" of an admin, used only to
// mint a Viewer service-account token). The instance must have the demo
// provisioning used during development: datasources prom / loki / pg and the
// "pm-demo" dashboard. Run against several versions to check compatibility.
func liveEnv(t *testing.T) (string, string, string) {
	base := os.Getenv("PATCHMAN_TEST_GRAFANA")
	admin := os.Getenv("PATCHMAN_TEST_GRAFANA_ADMIN")
	if base == "" || admin == "" {
		t.Skip("PATCHMAN_TEST_GRAFANA / PATCHMAN_TEST_GRAFANA_ADMIN not set")
	}
	u, p, _ := strings.Cut(admin, ":")
	return base, u, p
}

// viewerToken creates a Viewer service account (9+) or API key (8.x) and
// returns its token.
func viewerToken(t *testing.T, base, user, pass string) string {
	t.Helper()
	post := func(path string, body any) (map[string]any, int) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+path, bytes.NewReader(b))
		req.SetBasicAuth(user, pass)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out, res.StatusCode
	}
	name := fmt.Sprintf("pm-live-%d", time.Now().UnixNano())
	sa, code := post("/api/serviceaccounts", map[string]any{"name": name, "role": "Viewer"})
	if code < 300 {
		id := int64(sa["id"].(float64))
		tok, code := post(fmt.Sprintf("/api/serviceaccounts/%d/tokens", id), map[string]any{"name": name})
		if code < 300 {
			return tok["key"].(string)
		}
	}
	key, code := post("/api/auth/keys", map[string]any{"name": name, "role": "Viewer"})
	if code >= 300 {
		t.Fatalf("could not mint a token: %v", key)
	}
	return key["key"].(string)
}

func TestLiveDashboardFlow(t *testing.T) {
	base, user, pass := liveEnv(t)
	cipher := testCipher(t)
	reg := NewRegistry(cipher, nil)
	ctx := context.Background()
	conns := map[string]models.GrafanaConnection{
		"basic": {ID: "live-basic", URL: base, AuthKind: "basic", Username: user, PasswordEnc: enc(t, cipher, pass)},
	}
	// PATCHMAN_TEST_GRAFANA_ONLY=basic: a Grafana with auth.basic disabled
	// (the token can't be minted over Basic; the basic connection must fall
	// back to the login form).
	if os.Getenv("PATCHMAN_TEST_GRAFANA_ONLY") != "basic" {
		conns["token"] = models.GrafanaConnection{ID: "live-token", URL: base, AuthKind: "token", TokenEnc: enc(t, cipher, viewerToken(t, base, user, pass))}
	}
	for name, conn := range conns {
		t.Run(name, func(t *testing.T) {
			info, _, err := reg.Test(ctx, conn)
			if err != nil {
				t.Fatalf("test: %v", err)
			}
			t.Logf("Grafana %s (%d.%d), %d datasources, user=%+v", info.Version, info.Major, info.Minor, len(info.Datasources), info.User)
			if info.FindDatasource("prom") == nil || info.FindDatasource("Prometheus") == nil {
				t.Fatalf("datasources: %+v", info.Datasources)
			}
			sr, err := reg.Search(ctx, conn, "")
			if err != nil || len(sr.Dashboards) == 0 || len(sr.Folders) == 0 {
				t.Fatalf("search: %v %+v", err, sr)
			}
			d, err := reg.GetDashboard(ctx, conn, "pm-demo")
			if err != nil {
				t.Fatalf("dashboard: %v", err)
			}
			rng, _ := ParseRange("now-30m", "now", time.Now())
			tl := d.Dashboard["templating"].(map[string]any)["list"].([]any)
			vars, err := reg.ResolveVariables(ctx, conn, tl, map[string]Selection{"env": {Values: []string{"prod", "test"}}}, rng)
			if err != nil {
				t.Fatalf("variables: %v", err)
			}
			byName := map[string]ResolvedVar{}
			for _, v := range vars {
				byName[v.Name] = v
				t.Logf("var %-8s type=%-10s options=%d current=%v err=%q", v.Name, v.Type, len(v.Options), v.Current.Values, v.Error)
			}
			if j := byName["job"]; len(j.Options) < 2 || j.Error != "" {
				t.Errorf("job options from label_values: %+v", j)
			}
			if i := byName["instance"]; len(i.Options) == 0 || i.Error != "" {
				t.Errorf("chained instance options: %+v", i)
			}
			if ds := byName["ds"]; len(ds.Options) == 0 || ds.Current.Values[0] != "prom" {
				t.Errorf("datasource var: %+v", ds)
			}
			if e := byName["env"]; len(e.Current.Values) != 2 {
				t.Errorf("env selection: %+v", e.Current)
			}
			vm := VarsFrom(vars)
			var panels []map[string]any
			var walk func([]any)
			walk = func(ps []any) {
				for _, p := range ps {
					pm := p.(map[string]any)
					if n, ok := pm["panels"].([]any); ok {
						walk(n)
					}
					if pm["type"] != "row" {
						panels = append(panels, pm)
					}
				}
			}
			walk(d.Dashboard["panels"].([]any))
			for _, p := range panels {
				var targets []map[string]any
				if ts, ok := p["targets"].([]any); ok {
					for _, tg := range ts {
						targets = append(targets, tg.(map[string]any))
					}
				}
				if len(targets) == 0 {
					continue
				}
				res, err := reg.Query(ctx, conn, QueryRequest{From: "now-30m", To: "now", Targets: targets, PanelDatasource: p["datasource"], Vars: vm, MaxDataPoints: 400}, nil)
				if err != nil {
					t.Errorf("panel %v: %v", p["title"], err)
					continue
				}
				for ref, rr := range res.Results {
					t.Logf("panel %-28v %s frames=%d err=%q %s", p["title"], ref, len(rr.Frames), rr.Error, rr.Notice)
					if rr.Error != "" || len(rr.Frames) == 0 {
						t.Errorf("panel %v %s: no data (%s)", p["title"], ref, rr.Error)
					}
				}
			}
			// Read-only guard on raw SQL.
			guard := func(sql string) error {
				if strings.Contains(strings.ToUpper(sql), "DELETE") {
					return fmt.Errorf("read-only")
				}
				return nil
			}
			res, err := reg.Query(ctx, conn, QueryRequest{From: "now-1h", To: "now", Targets: []map[string]any{{"refId": "A", "datasource": map[string]any{"uid": "pg"}, "rawSql": "DELETE FROM samples", "format": "table"}}}, guard)
			if err != nil || res.Results["A"].Error == "" {
				t.Errorf("guard must block: %v %+v", err, res)
			}
			// Autocomplete resource.
			body, err := reg.DSResource(ctx, conn, info.FindDatasource("prom"), "api/v1/label/__name__/values", nil)
			if err != nil || !bytes.Contains(body, []byte("up")) {
				t.Errorf("resource: %v %.200s", err, body)
			}
			if _, err := reg.DSResource(ctx, conn, info.FindDatasource("prom"), "api/v1/admin/tsdb/delete_series", nil); err == nil {
				t.Error("non-allowlisted path must be refused")
			}
		})
	}
}

// TestLiveSessionRotation waits past Grafana's token rotation interval
// (start Grafana with GF_AUTH_TOKEN_ROTATION_INTERVAL_MINUTES=2) and checks
// the session keeps working via rotation. Gated separately — it is slow.
func TestLiveSessionRotation(t *testing.T) {
	base, user, pass := liveEnv(t)
	if os.Getenv("PATCHMAN_TEST_GRAFANA_ROTATION") == "" {
		t.Skip("PATCHMAN_TEST_GRAFANA_ROTATION not set")
	}
	cipher := testCipher(t)
	store := &memStore{}
	reg := NewRegistry(cipher, store)
	ctx := context.Background()
	cookie, err := reg.SignIn(ctx, models.GrafanaConnection{URL: base}, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	conn := models.GrafanaConnection{ID: "live-sess", URL: base, AuthKind: "session", SessionEnc: enc(t, cipher, cookie)}
	if _, err := reg.Info(ctx, conn); err != nil {
		t.Fatal(err)
	}
	wait := 150 * time.Second
	t.Logf("waiting %v for the token rotation interval", wait)
	time.Sleep(wait)
	var u map[string]any
	if err := reg.DoJSON(ctx, conn, "GET", "/api/user", nil, nil, &u); err != nil {
		t.Fatalf("after rotation interval: %v", err)
	}
	if store.last["live-sess"] == "" || store.last["live-sess"] == cookie {
		t.Fatalf("expected a rotated cookie to be persisted, got %q", store.last["live-sess"])
	}
	t.Logf("rotated and persisted; user=%v", u["login"])
}

// TestLiveVariableKinds resolves the "oms-overview" dev dashboard: Elasticsearch
// terms (incl. a chained one), InfluxDB SHOW TAG VALUES, the VictoriaMetrics
// plugin and an unsupported datasource. Gated by PATCHMAN_TEST_GRAFANA_OMS.
func TestLiveVariableKinds(t *testing.T) {
	base, user, pass := liveEnv(t)
	if os.Getenv("PATCHMAN_TEST_GRAFANA_OMS") == "" {
		t.Skip("PATCHMAN_TEST_GRAFANA_OMS not set")
	}
	cipher := testCipher(t)
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "live-oms", URL: base, AuthKind: "basic", Username: user, PasswordEnc: enc(t, cipher, pass)}
	ctx := context.Background()
	d, err := reg.GetDashboard(ctx, conn, "oms-overview")
	if err != nil {
		t.Fatal(err)
	}
	rng, _ := ParseRange("now-1h", "now", time.Now())
	vars, err := reg.ResolveVariables(ctx, conn, d.Dashboard["templating"].(map[string]any)["list"].([]any), nil, rng)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ResolvedVar{}
	for _, v := range vars {
		by[v.Name] = v
		t.Logf("%-7s free=%v options=%v err=%q", v.Name, v.Free, v.Options, v.Error)
	}
	for _, n := range []string{"service", "route", "host", "vmjob"} {
		if v := by[n]; v.Free || len(v.Options) < 1 || v.Error != "" {
			t.Errorf("%s: expected listed options, got %+v", n, v)
		}
	}
	// route is chained on service: with All selected it sees every route.
	if v := by["route"]; len(v.Options) < 4 {
		t.Errorf("route: expected all routes for service=All, got %v", v.Options)
	}
	// Narrowing service narrows route (the dev data has one "solo-svc" doc
	// whose only route is /solo).
	vars2, err := reg.ResolveVariables(ctx, conn, d.Dashboard["templating"].(map[string]any)["list"].([]any),
		map[string]Selection{"service": {Values: []string{"solo-svc"}}}, rng)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars2 {
		if v.Name == "route" {
			t.Logf("route for solo-svc: %v", v.Options)
			if len(v.Options) != 2 || v.Options[1].Value != "/solo" { // All + /solo
				t.Errorf("chained route did not narrow: %v", v.Options)
			}
		}
	}
	// Panels with All selected on listed variables return data.
	vm := VarsFrom(vars)
	for _, p := range d.Dashboard["panels"].([]any) {
		pm := p.(map[string]any)
		var targets []map[string]any
		for _, tg := range pm["targets"].([]any) {
			targets = append(targets, tg.(map[string]any))
		}
		res, err := reg.Query(ctx, conn, QueryRequest{From: "now-1h", To: "now", Targets: targets, PanelDatasource: pm["datasource"], Vars: vm, MaxDataPoints: 300}, nil)
		if err != nil {
			t.Fatalf("%v: %v", pm["title"], err)
		}
		for ref, rr := range res.Results {
			t.Logf("panel %-22v %s frames=%d err=%q", pm["title"], ref, len(rr.Frames), rr.Error)
			if rr.Error != "" || len(rr.Frames) == 0 {
				t.Errorf("panel %v: no data", pm["title"])
			}
		}
	}
}

// TestLivePMM runs against a Percona PMM server given by its ROOT URL
// (PATCHMAN_TEST_PMM, e.g. http://localhost:13480; Grafana lives under
// /graph) with admin credentials in PATCHMAN_TEST_PMM_ADMIN. It checks root
// detection and walks the first dashboards (PATCHMAN_TEST_PMM_DASHBOARDS:
// comma separated titles, or * for all): variables + every panel query.
func TestLivePMM(t *testing.T) {
	root := os.Getenv("PATCHMAN_TEST_PMM")
	admin := os.Getenv("PATCHMAN_TEST_PMM_ADMIN")
	if root == "" || admin == "" {
		t.Skip("PATCHMAN_TEST_PMM / PATCHMAN_TEST_PMM_ADMIN not set")
	}
	user, pass, _ := strings.Cut(admin, ":")
	cipher := testCipher(t)
	reg := NewRegistry(cipher, nil)
	ctx := context.Background()
	conn := models.GrafanaConnection{URL: root, AuthKind: "basic", Username: user, PasswordEnc: enc(t, cipher, pass)}
	info, _, err := reg.Test(ctx, conn)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	t.Logf("Grafana %s, resolved=%q, datasources=%v", info.Version, info.ResolvedURL, func() []string {
		var n []string
		for _, d := range info.Datasources {
			n = append(n, d.Name+"("+d.Type+")")
		}
		return n
	}())
	if !strings.HasSuffix(info.ResolvedURL, "/graph") {
		t.Fatalf("expected the /graph root to be found, got %q", info.ResolvedURL)
	}
	conn.URL = info.ResolvedURL
	conn.ID = "pmm-live"
	sr, err := reg.Search(ctx, conn, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d folders, %d dashboards", len(sr.Folders), len(sr.Dashboards))
	want := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("PATCHMAN_TEST_PMM_DASHBOARDS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[strings.ToLower(n)] = true
		}
	}
	rng, _ := ParseRange("now-15m", "now", time.Now())
	unsupported := map[string]int{}
	checked := 0
	all := want["*"]
	for _, hit := range sr.Dashboards {
		if len(want) > 0 && !all && !want[strings.ToLower(hit.Title)] {
			continue
		}
		if len(want) == 0 && checked >= 6 {
			break
		}
		checked++
		d, err := reg.GetDashboard(ctx, conn, hit.UID)
		if err != nil {
			t.Errorf("%s: %v", hit.Title, err)
			continue
		}
		tl, _ := d.Dashboard["templating"].(map[string]any)["list"].([]any)
		vars, err := reg.ResolveVariables(ctx, conn, tl, nil, rng)
		if err != nil {
			t.Errorf("%s vars: %v", hit.Title, err)
			continue
		}
		var varErrs []string
		for _, v := range vars {
			if v.Error != "" {
				varErrs = append(varErrs, v.Name+": "+v.Error)
			}
		}
		vm := VarsFrom(vars)
		var panels []map[string]any
		var walk func([]any)
		walk = func(ps []any) {
			for _, p := range ps {
				pm := p.(map[string]any)
				if n, ok := pm["panels"].([]any); ok {
					walk(n)
				}
				if pm["type"] != "row" {
					panels = append(panels, pm)
				}
			}
		}
		walk(d.Dashboard["panels"].([]any))
		ok, empty, failed := 0, 0, 0
		var fails []string
		for _, p := range panels {
			pt, _ := p["type"].(string)
			switch pt {
			case "timeseries", "graph", "stat", "singlestat", "gauge", "bargauge", "table", "table-old", "text", "piechart", "grafana-piechart-panel", "barchart", "logs", "heatmap", "grafana-polystat-panel", "state-timeline", "natel-discrete-panel":
			default:
				unsupported[pt]++
			}
			var targets []map[string]any
			if ts, ok := p["targets"].([]any); ok {
				for _, tg := range ts {
					targets = append(targets, tg.(map[string]any))
				}
			}
			if len(targets) == 0 {
				continue
			}
			res, err := reg.Query(ctx, conn, QueryRequest{From: "now-15m", To: "now", Targets: targets, PanelDatasource: p["datasource"], Vars: vm, MaxDataPoints: 300, MinInterval: str(p["interval"])}, nil)
			if err != nil {
				failed++
				fails = append(fails, fmt.Sprintf("%v: %v", p["title"], err))
				continue
			}
			hasData, hasErr := false, ""
			for _, rr := range res.Results {
				if len(rr.Frames) > 0 {
					hasData = true
				}
				if rr.Error != "" {
					hasErr = rr.Error
				}
			}
			switch {
			case hasErr != "":
				failed++
				if len(fails) < 4 {
					fails = append(fails, fmt.Sprintf("%v: %s", p["title"], hasErr))
				}
			case hasData:
				ok++
			default:
				empty++
			}
		}
		t.Logf("%-40s vars=%d varErrs=%d panels=%d ok=%d empty=%d failed=%d", hit.Title, len(vars), len(varErrs), len(panels), ok, empty, failed)
		for _, e := range varErrs {
			t.Logf("    var error: %.200s", e)
		}
		for _, f := range fails {
			t.Logf("    panel error: %.240s", f)
		}
	}
	t.Logf("unsupported panel types seen: %v", unsupported)
}
