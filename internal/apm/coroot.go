package apm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/servicemap"
)

// corootProvider reads Coroot (verified against Coroot CE 1.28):
//
//   - The application map (GET /api/project/{p}/overview/map): apps as
//     "<project>:<ns>:<Kind>:<name>", databases / queues marked by labels,
//     upstream links with their request rate (weight) and Coroot's status.
//   - Per-call errors and latency from the coroot-node-agent metrics, through
//     /api/project/{p}/panel/data (works with any read session) or, when a
//     project API key is stored, /api/v1/query_range (X-API-Key). Coroot's
//     session Prometheus proxy does not serve queries.
//
// Sign-in: bearer (a Viewer service account's crt_ key — recommended), login
// (email + password → coroot_session), session (a pasted coroot_session), or
// apikey (a project key: metrics only, no map, no health).
type corootProvider struct{ base }

func init() { register(corootProvider{}) }

func (corootProvider) Name() string          { return "coroot" }
func (corootProvider) Title() string         { return "Coroot" }
func (corootProvider) AuthKinds() []string   { return []string{"bearer", "login", "session", "apikey"} }
func (corootProvider) APIKeyHeader() string  { return "X-API-Key" }
func (corootProvider) SessionCookie() string { return "coroot_session" }

func (p corootProvider) Login(ctx context.Context, c *Client) (string, error) {
	body, _ := json.Marshal(map[string]string{"email": c.Conn.Username, "password": c.Secrets.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// Coroot answers a wrong password with 404 "Invalid email or password."
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
			return "", &APIError{http.StatusUnauthorized, "auth", "Coroot rejected the email or password."}
		}
		return "", c.httpErr(resp.StatusCode, b)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == p.SessionCookie() && ck.Value != "" {
			return ck.Value, nil
		}
	}
	return "", &APIError{http.StatusUnauthorized, "auth", "Coroot accepted the sign-in but set no session — is this Coroot's address?"}
}

type corootUser struct {
	Email    string  `json:"email"`
	Name     string  `json:"name"`
	Projects []Scope `json:"projects"`
}

func (p corootProvider) user(ctx context.Context, c *Client) (*corootUser, error) {
	var u corootUser
	if err := c.Do(ctx, http.MethodGet, "/api/user", nil, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Scopes lists the projects the signed-in user can see (/api/user; the
// /api/project/ form only has names).
func (p corootProvider) Scopes(ctx context.Context, c *Client) ([]Scope, error) {
	if c.Conn.AuthKind == "apikey" {
		return nil, nil
	}
	u, err := p.user(ctx, c)
	if err != nil {
		return nil, err
	}
	return u.Projects, nil
}

func (p corootProvider) keyQuerier(c *Client) promQuerier {
	return promQuerier{c: c, prefix: "/api/v1", rangeOnly: true, headers: map[string]string{"X-API-Key": c.Secrets.APIKey}}
}

func (p corootProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	res := &TestResult{OK: true}
	if c.Conn.AuthKind == "apikey" {
		if _, err := p.keyQuerier(c).Query(ctx, "vector(1)", nowUTC()); err != nil {
			return nil, err
		}
		res.Notes = append(res.Notes, "metrics_only")
		return res, nil
	}
	u, err := p.user(ctx, c)
	if err != nil {
		return nil, err
	}
	res.User = firstNonEmpty(u.Email, u.Name)
	res.Scopes = u.Projects
	if len(u.Projects) == 0 {
		res.Notes = append(res.Notes, "no_projects")
	}
	if c.Secrets.APIKey != "" {
		if _, err := p.keyQuerier(c).Query(ctx, "vector(1)", nowUTC()); err != nil {
			res.Notes = append(res.Notes, "project_key_rejected")
		} else {
			res.Notes = append(res.Notes, "project_key_ok")
		}
	}
	return res, nil
}

func (p corootProvider) project(ctx context.Context, c *Client, scope string) (string, error) {
	if pr := firstNonEmpty(scope, c.Conn.Scope); pr != "" {
		return pr, nil
	}
	scopes, err := p.Scopes(ctx, c)
	if err != nil {
		return "", err
	}
	if len(scopes) == 0 {
		return "", &APIError{http.StatusNotFound, "not_found", "This Coroot user sees no projects."}
	}
	return scopes[0].ID, nil
}

func (p corootProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	at := req.End
	if at.IsZero() {
		at = nowUTC()
	}
	if c.Conn.AuthKind == "apikey" {
		st.Notes = append(st.Notes, "metrics_only")
		return mapCorootAgent(ctx, p.keyQuerier(c), at, req.Window(), b, "coroot")
	}
	project, err := p.project(ctx, c, req.Scope)
	if err != nil {
		return err
	}
	rng := req.Range
	if rng < time.Minute {
		rng = 15 * time.Minute
	}
	q := url.Values{"from": {"now-" + strconv.Itoa(int(rng.Minutes())) + "m"}, "to": {"now"}}
	var m corootMap
	if err := c.Do(ctx, http.MethodGet, "/api/project/"+url.PathEscape(project)+"/overview/map", q, nil, &m); err != nil {
		return err
	}
	if m.Data == nil {
		return &APIError{http.StatusNotFound, "not_found", fmt.Sprintf("Coroot has no project %q", project)}
	}
	links := applyCorootMap(b, project, m.Data.Map)

	// Per-call errors and latency from the node-agent metrics.
	var querier servicemap.PromQuerier = panelQuerier{c: c, project: project}
	if c.Secrets.APIKey != "" {
		querier = p.keyQuerier(c)
	}
	if err := mapCorootAgent(ctx, querier, at, req.Window(), b, "coroot"); err != nil {
		st.Notes = append(st.Notes, "coroot_red_unavailable:"+trimErr(err))
	}
	// The map's links fill in what the metrics did not observe.
	for _, l := range links {
		if e := b.EdgeBetween(l.from, l.to); e != nil {
			if e.Issue == "" {
				e.Issue = l.issue
			}
			if !e.Stats.HasRED && l.rps > 0 {
				e.Stats.HasRED, e.Stats.RateOnly, e.Stats.RPS = true, true, l.rps
			}
			if e.Stats.P95Ms == 0 {
				e.Stats.P95Ms = l.p95
			}
			continue
		}
		stats := servicemap.Stats{}
		if l.rps > 0 {
			stats = servicemap.Stats{HasRED: true, RateOnly: true, RPS: l.rps, P95Ms: l.p95}
		}
		proto := ""
		if n := b.Node(l.to); n != nil {
			proto = servicemap.ProtocolOf(n.Subtype)
		}
		if e := b.Link(l.from, l.to, "request", proto, "coroot", stats, nil); e != nil {
			e.Issue = l.issue
		}
	}
	servicemap.Rollup(b, "coroot")

	// Instances ("2/3") for every app, also the ones with no links.
	var apps corootApps
	if err := c.Do(ctx, http.MethodGet, "/api/project/"+url.PathEscape(project)+"/overview/applications", nil, nil, &apps); err == nil && apps.Data != nil {
		applyCorootApps(b, project, apps.Data.Applications)
	}
	return nil
}

type corootMap struct {
	Data *struct {
		Map []corootApp `json:"map"`
	} `json:"data"`
}

type corootApp struct {
	ID        string            `json:"id"`
	Category  string            `json:"category"`
	Labels    map[string]string `json:"labels"`
	Status    string            `json:"status"`
	Upstreams []struct {
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Stats  []string `json:"stats"`
		Weight float64  `json:"weight"`
	} `json:"upstreams"`
}

type corootApps struct {
	Data *struct {
		Applications []struct {
			ID        string `json:"id"`
			Category  string `json:"category"`
			Status    string `json:"status"`
			Instances struct {
				Value string `json:"value"`
			} `json:"instances"`
		} `json:"applications"`
	} `json:"data"`
}

type corootLink struct {
	from, to, issue string
	rps, p95        float64
}

// corootNode turns a Coroot app id into the map's node id:
// "<project>:<ns>:<Kind>:<name>" → "ns/Kind/name" (a K8s workload id), and
// "external:external:ExternalService:<name>" → "ext:<name>".
func corootNode(id string) (nodeID, ns, kind, name string, external bool) {
	parts := strings.SplitN(id, ":", 4)
	if len(parts) < 4 {
		return "ext:" + strings.ToLower(id), "", "", id, true
	}
	if parts[2] == "ExternalService" {
		return "ext:" + strings.ToLower(parts[3]), "", "ExternalService", parts[3], true
	}
	ns, kind, name = parts[1], parts[2], parts[3]
	if ns == "_" {
		ns = ""
	}
	return servicemap.WorkloadID(ns, kind, name), ns, kind, name, false
}

func corootHealth(s string) string {
	switch strings.ToLower(s) {
	case "ok", "info":
		return "ok"
	case "warning":
		return "warning"
	case "critical":
		return "critical"
	}
	return "unknown"
}

var corootLatencyRe = regexp.MustCompile(`⏱️?\s*([\d.]+)\s*(µs|us|ms|s)\b`)

func corootLatencyMs(stats []string) float64 {
	for _, s := range stats {
		if m := corootLatencyRe.FindStringSubmatch(s); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			switch m[2] {
			case "µs", "us":
				return v / 1000
			case "s":
				return v * 1000
			}
			return v
		}
	}
	return 0
}

func applyCorootMap(b *servicemap.Builder, project string, apps []corootApp) []corootLink {
	ids := map[string]string{}
	for _, a := range apps {
		nodeID, ns, kind, name, external := corootNode(a.ID)
		n := &servicemap.Node{ID: nodeID, Namespace: ns, Name: name, Health: corootHealth(a.Status), Category: a.Category,
			Coroot: &servicemap.CorootRef{Project: project, AppID: a.ID}}
		switch {
		case a.Labels["db"] != "":
			n.Kind, n.Subtype = servicemap.KindDatastore, corootDBSubtype(a.Labels["db"])
		case a.Labels["queue"] != "":
			n.Kind, n.Subtype = servicemap.KindMessaging, corootDBSubtype(a.Labels["queue"])
		case external:
			n.Kind, n.Subtype = servicemap.KindExternal, "http"
			if _, port, ok := strings.Cut(name, ":"); ok {
				if pn, err := strconv.Atoi(port[strings.LastIndex(port, ":")+1:]); err == nil {
					if sub := servicemap.Subtype("", pn); sub != "" {
						n.Subtype = sub
						if k := servicemap.KindOf(sub); k != servicemap.KindService {
							n.Kind = k
						}
					}
				}
			}
		default:
			n.Kind, n.Subtype = servicemap.KindService, "http"
		}
		if !external && kind != "" && kind != "Unknown" {
			n.K8s = &servicemap.K8sRef{Kind: kind, Namespace: ns, Name: name}
		}
		got := b.Upsert(n, "coroot")
		ids[a.ID] = got.ID
		// The node-agent metrics address external services by IP:port.
		if external {
			port := ""
			if i := strings.LastIndex(name, ":"); i >= 0 {
				port = name[i+1:]
			}
			for _, k := range []string{"external endpoint", "external endpoints"} {
				for _, ip := range strings.Split(a.Labels[k], ",") {
					if ip = strings.TrimSpace(ip); ip != "" && ip != "..." && port != "" {
						b.Alias("ext:"+ip+":"+port, got.ID)
					}
				}
			}
		}
	}
	var links []corootLink
	for _, a := range apps {
		for _, u := range a.Upstreams {
			to, ok := ids[u.ID]
			if !ok {
				nodeID, _, _, _, _ := corootNode(u.ID)
				to = nodeID
			}
			issue := ""
			for _, s := range u.Stats {
				if strings.Contains(s, "⚠") {
					issue = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "⚠️"), "⚠"))
				}
			}
			if issue == "" && strings.EqualFold(u.Status, "critical") {
				issue = "critical"
			}
			links = append(links, corootLink{from: ids[a.ID], to: to, issue: issue, rps: u.Weight, p95: corootLatencyMs(u.Stats)})
		}
	}
	sort.Slice(links, func(i, j int) bool { return links[i].from+links[i].to < links[j].from+links[j].to })
	return links
}

func applyCorootApps(b *servicemap.Builder, project string, apps []struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Status    string `json:"status"`
	Instances struct {
		Value string `json:"value"`
	} `json:"instances"`
}) {
	for _, a := range apps {
		nodeID, ns, kind, name, external := corootNode(a.ID)
		if external {
			continue
		}
		n := b.Node(nodeID)
		if n == nil {
			n = b.Upsert(&servicemap.Node{ID: nodeID, Kind: servicemap.KindService, Subtype: "http", Namespace: ns, Name: name,
				Health: corootHealth(a.Status), Category: a.Category, Coroot: &servicemap.CorootRef{Project: project, AppID: a.ID},
				K8s: &servicemap.K8sRef{Kind: kind, Namespace: ns, Name: name}}, "coroot")
		}
		if n.Replicas == "" && strings.Contains(a.Instances.Value, "/") {
			n.Replicas = a.Instances.Value
		}
	}
}

func corootDBSubtype(s string) string {
	switch s = strings.ToLower(s); s {
	case "postgres", "postgresql":
		return "postgres"
	case "mongodb", "mongo":
		return "mongo"
	case "mysql", "mariadb":
		return "mysql"
	case "elasticsearch", "opensearch":
		return "elasticsearch"
	}
	return s
}

// panelQuerier runs PromQL through Coroot's dashboard panel endpoint — the
// one PromQL path a read-only session has. Labels only come back through the
// legend template, so the legend spells out every label the query keeps.
type panelQuerier struct {
	c       *Client
	project string
}

const legendSep = "|~|"

var (
	byRe    = regexp.MustCompile(`by\s*\(([^)]*)\)`)
	replRe  = regexp.MustCompile(`label_replace\([^"]*"(\w+)"`)
	labelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// queryLabels lists the labels a query's result keeps: every `by (…)`
// label plus label_replace targets, minus le.
func queryLabels(expr string) []string {
	set := map[string]bool{}
	for _, m := range byRe.FindAllStringSubmatch(expr, -1) {
		for _, l := range strings.Split(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" && l != "le" && labelRe.MatchString(l) {
				set[l] = true
			}
		}
	}
	for _, m := range replRe.FindAllStringSubmatch(expr, -1) {
		set[m[1]] = true
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// maxPanelQuery keeps panel/data GET URLs well under the 8 KB request line
// that ingress controllers (nginx) accept by default.
const maxPanelQuery = 1200

// splitTopLevelOr splits "a or b or c" at top-level `or`s (outside
// parentheses, braces and quotes).
func splitTopLevelOr(expr string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case depth == 0 && strings.HasPrefix(expr[i:], " or "):
			parts = append(parts, strings.TrimSpace(expr[start:i]))
			start = i + 4
			i += 3
		}
	}
	return append(parts, strings.TrimSpace(expr[start:]))
}

// Query runs expr through panel/data, in several requests when the folded
// query would make the URL too long.
func (q panelQuerier) Query(ctx context.Context, expr string, at time.Time) ([]servicemap.Sample, error) {
	if len(expr) <= maxPanelQuery {
		return q.query(ctx, expr, at)
	}
	var out []servicemap.Sample
	chunk := ""
	flush := func() error {
		if chunk == "" {
			return nil
		}
		rows, err := q.query(ctx, chunk, at)
		if err != nil {
			return err
		}
		out = append(out, rows...)
		chunk = ""
		return nil
	}
	for _, part := range splitTopLevelOr(expr) {
		if chunk != "" && len(chunk)+4+len(part) > maxPanelQuery {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		if chunk == "" {
			chunk = part
		} else {
			chunk += " or " + part
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func (q panelQuerier) query(ctx context.Context, expr string, at time.Time) ([]servicemap.Sample, error) {
	labels := queryLabels(expr)
	legend := make([]string, len(labels))
	for i, l := range labels {
		legend[i] = "{{" + l + "}}"
	}
	panel, _ := json.Marshal(map[string]any{
		"name":   "patchman",
		"source": map[string]any{"metrics": map[string]any{"queries": []any{map[string]any{"query": expr, "legend": strings.Join(legend, legendSep)}}}},
		"widget": map[string]any{"chart": map[string]any{"display": "line"}},
	})
	v := url.Values{
		"from":  {strconv.FormatInt(at.Add(-5*time.Minute).UnixMilli(), 10)},
		"to":    {strconv.FormatInt(at.UnixMilli(), 10)},
		"query": {string(panel)},
	}
	var r struct {
		Chart *struct {
			Series []struct {
				Name string     `json:"name"`
				Data []*float64 `json:"data"`
			} `json:"series"`
		} `json:"chart"`
	}
	if err := q.c.Do(ctx, http.MethodGet, "/api/project/"+url.PathEscape(q.project)+"/panel/data", v, nil, &r); err != nil {
		return nil, err
	}
	if r.Chart == nil {
		return nil, nil
	}
	out := make([]servicemap.Sample, 0, len(r.Chart.Series))
	for _, s := range r.Chart.Series {
		var last *float64
		for i := len(s.Data) - 1; i >= 0; i-- {
			if s.Data[i] != nil {
				last = s.Data[i]
				break
			}
		}
		if last == nil {
			continue
		}
		values := strings.Split(s.Name, legendSep)
		m := map[string]string{}
		if len(values) == len(labels) {
			for i, l := range labels {
				m[l] = values[i]
			}
		}
		out = append(out, servicemap.Sample{Metric: m, Value: *last})
	}
	return out, nil
}
