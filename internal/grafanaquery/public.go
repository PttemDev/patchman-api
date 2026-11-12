package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
)

// Public (externally shared) dashboards: Grafana 10+ serves a dashboard and
// each panel's data to anyone holding the link's access token, no sign-in.
// Variables are not supported there; panels are queried by id.

// ParsePublicDashboardURL splits a public dashboard link into the Grafana
// root and the access token: https://g.example.com/public-dashboards/<token>.
func ParsePublicDashboardURL(raw string) (root, token string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("not a URL")
	}
	for _, marker := range []string{"/public-dashboards/", "/public/dashboards/"} {
		if i := strings.Index(u.Path, marker); i >= 0 {
			token = strings.Trim(strings.SplitN(u.Path[i+len(marker):], "/", 2)[0], " ")
			root = u.Scheme + "://" + u.Host + strings.TrimRight(u.Path[:i], "/")
			if token == "" {
				break
			}
			return root, token, nil
		}
	}
	return "", "", fmt.Errorf("not a Grafana public dashboard link (expected …/public-dashboards/<token>)")
}

// PublicDashboardRef is a public dashboard link saved on a connection
// (options.publicDashboards) — how a Grafana that needs a login for
// everything else still shows its shared dashboards.
type PublicDashboardRef struct {
	Token string `json:"token"`
	Title string `json:"title,omitempty"`
}

var publicTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ValidPublicToken reports whether s looks like a Grafana access token.
func ValidPublicToken(s string) bool { return publicTokenRE.MatchString(s) }

// PublicDashboards returns the connection's saved public dashboard links.
func PublicDashboards(conn models.GrafanaConnection) []PublicDashboardRef {
	list, _ := parseOptions(conn.OptionsJSON)["publicDashboards"].([]any)
	var out []PublicDashboardRef
	for _, it := range list {
		m, _ := it.(map[string]any)
		tok, _ := m["token"].(string)
		if !ValidPublicToken(tok) {
			continue
		}
		title, _ := m["title"].(string)
		out = append(out, PublicDashboardRef{Token: tok, Title: title})
	}
	return out
}

// TestPublic loads every saved public dashboard and returns them titled.
func (r *Registry) TestPublic(ctx context.Context, conn models.GrafanaConnection, refs []PublicDashboardRef) ([]PublicDashboardRef, error) {
	if conn.ID == "" {
		conn.ID = fmt.Sprintf("%s%d", testIDPrefix, time.Now().UnixNano())
		defer r.Close(conn.ID)
	}
	out := make([]PublicDashboardRef, 0, len(refs))
	for _, ref := range refs {
		d, err := r.GetPublicDashboard(ctx, conn, ref.Token)
		if err != nil {
			short := ref.Token
			if len(short) > 8 {
				short = short[:8] + "…"
			}
			return nil, fmt.Errorf("public dashboard %s: %w", short, err)
		}
		title, _ := d.Dashboard["title"].(string)
		out = append(out, PublicDashboardRef{Token: ref.Token, Title: title})
	}
	return out, nil
}

// GetPublicDashboard loads a public dashboard by its access token.
func (r *Registry) GetPublicDashboard(ctx context.Context, conn models.GrafanaConnection, token string) (*Dashboard, error) {
	if !ValidPublicToken(token) {
		return nil, fmt.Errorf("invalid public dashboard token")
	}
	r.findRoot(ctx, conn)
	var res struct {
		Dashboard map[string]any `json:"dashboard"`
		Meta      struct {
			Slug        string `json:"slug"`
			FolderTitle string `json:"folderTitle"`
		} `json:"meta"`
	}
	if err := r.DoJSON(ctx, conn, http.MethodGet, "/api/public/dashboards/"+url.PathEscape(token), nil, nil, &res); err != nil {
		return nil, err
	}
	if res.Dashboard == nil {
		return nil, fmt.Errorf("public dashboard not found")
	}
	return &Dashboard{Dashboard: res.Dashboard, Meta: DashboardMeta{
		URL: "/public-dashboards/" + token, Slug: res.Meta.Slug, FolderTitle: res.Meta.FolderTitle, Schema: "v1",
	}}, nil
}

// QueryPublicPanel runs one panel of a public dashboard.
func (r *Registry) QueryPublicPanel(ctx context.Context, conn models.GrafanaConnection, token string, panelID int, from, to string, maxDataPoints int) (*QueryResult, error) {
	if !ValidPublicToken(token) {
		return nil, fmt.Errorf("invalid public dashboard token")
	}
	r.findRoot(ctx, conn)
	rng, err := ParseRange(from, to, time.Now())
	if err != nil {
		return nil, err
	}
	if maxDataPoints <= 0 {
		maxDataPoints = 1000
	}
	iv := CalcInterval(rng, maxDataPoints, 0)
	body := map[string]any{
		"intervalMs":    iv.Milliseconds(),
		"maxDataPoints": maxDataPoints,
		"timeRange": map[string]any{
			"from": strconv.FormatInt(rng.FromMs(), 10), "to": strconv.FormatInt(rng.ToMs(), 10), "timezone": "browser",
		},
	}
	path := fmt.Sprintf("/api/public/dashboards/%s/panels/%d/query", url.PathEscape(token), panelID)
	var raw []byte
	resp, err := r.Do(ctx, conn, http.MethodPost, path, nil, mustJSON(body))
	if err != nil {
		ae, ok := AsAPIError(err)
		if !ok || len(ae.body) == 0 || !strings.Contains(string(ae.body), `"results"`) {
			return nil, err
		}
		raw = ae.body
	} else {
		raw = resp.Body
	}
	var res struct {
		Results map[string]struct {
			Frames []json.RawMessage `json:"frames"`
			Error  string            `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode public query response: %w", err)
	}
	out := &QueryResult{Results: map[string]*RefResult{}, IntervalMs: iv.Milliseconds(), From: rng.FromMs(), To: rng.ToMs()}
	for ref, rr := range res.Results {
		frames := rr.Frames
		if frames == nil {
			frames = []json.RawMessage{}
		}
		out.Results[ref] = &RefResult{Frames: frames, Error: rr.Error}
	}
	return out, nil
}
