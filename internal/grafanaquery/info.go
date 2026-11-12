package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
)

// Datasource is the slim view of one Grafana datasource the FE and the query
// path need. Backend reports whether /api/ds/query can run it (frontend-only
// plugins cannot — those fall back to the datasource proxy where we know how).
type Datasource struct {
	ID        int64  `json:"id,omitempty"`
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	IsDefault bool   `json:"isDefault,omitempty"`
	Backend   bool   `json:"backend"`
	// TimeInterval is the datasource "scrape interval" (min interval floor).
	TimeInterval string `json:"timeInterval,omitempty"`
	// TimeField is an Elasticsearch/OpenSearch datasource's time field.
	TimeField string `json:"timeField,omitempty"`
}

// ServerInfo is what PatchMan learns about a Grafana once per connection.
type ServerInfo struct {
	Version     string       `json:"version"`
	Major       int          `json:"major"`
	Minor       int          `json:"minor"`
	Namespace   string       `json:"namespace,omitempty"`
	Datasources []Datasource `json:"datasources"`
	DefaultDS   string       `json:"defaultDatasource,omitempty"`
	AppSubURL   string       `json:"appSubUrl,omitempty"`
	User        *UserInfo    `json:"user,omitempty"`
	// ResolvedURL is set when Grafana was found under a sub path of the
	// saved URL (e.g. Percona PMM's /graph) — the URL to save.
	ResolvedURL string `json:"resolvedUrl,omitempty"`
}

type UserInfo struct {
	Login   string `json:"login"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	OrgRole string `json:"orgRole,omitempty"`
}

// AtLeast reports whether the server version is ≥ major.minor.
func (s *ServerInfo) AtLeast(major, minor int) bool {
	if s == nil || s.Major == 0 {
		return true // unknown → assume modern
	}
	if s.Major != major {
		return s.Major > major
	}
	return s.Minor >= minor
}

// FindDatasource resolves a datasource reference by uid, name or numeric id.
// "" / "default" resolve to the default datasource.
func (s *ServerInfo) FindDatasource(ref string) *Datasource {
	ref = strings.TrimSpace(ref)
	if s == nil {
		return nil
	}
	if ref == "" || strings.EqualFold(ref, "default") {
		ref = s.DefaultDS
		for i := range s.Datasources {
			if s.Datasources[i].IsDefault {
				return &s.Datasources[i]
			}
		}
	}
	for i := range s.Datasources {
		if s.Datasources[i].UID == ref {
			return &s.Datasources[i]
		}
	}
	for i := range s.Datasources {
		if s.Datasources[i].Name == ref {
			return &s.Datasources[i]
		}
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		for i := range s.Datasources {
			if s.Datasources[i].ID == id {
				return &s.Datasources[i]
			}
		}
	}
	return nil
}

// FindByType returns the default datasource when it has type typ, else the
// first datasource of that type (nil for "" or no match).
func (s *ServerInfo) FindByType(typ string) *Datasource {
	if s == nil || typ == "" || typ == "datasource" {
		return nil
	}
	var first *Datasource
	for i := range s.Datasources {
		d := &s.Datasources[i]
		if !strings.EqualFold(d.Type, typ) {
			continue
		}
		if d.IsDefault {
			return d
		}
		if first == nil {
			first = d
		}
	}
	return first
}

// Info returns the cached server info, fetching it when stale. It reads
// /api/frontend/settings (works for any signed-in role on every supported
// version) and /api/user; /api/health is the version fallback.
func (r *Registry) Info(ctx context.Context, conn models.GrafanaConnection) (*ServerInfo, error) {
	st, err := r.state(conn)
	if err != nil {
		return nil, err
	}
	st.authMu.Lock()
	if st.info != nil && time.Since(st.infoAt) < infoTTL {
		info := st.info
		st.authMu.Unlock()
		return info, nil
	}
	st.authMu.Unlock()

	info, err := r.fetchInfo(ctx, conn)
	if err != nil {
		return nil, err
	}
	st.authMu.Lock()
	st.info, st.infoAt = info, time.Now()
	st.authMu.Unlock()
	return info, nil
}

// grafanaSubPaths are tried when the saved URL isn't Grafana's root:
// Percona PMM serves Grafana under /graph, many reverse proxies under /grafana.
var grafanaSubPaths = []string{"/graph", "/grafana"}

// loadSettings reads /api/frontend/settings from Grafana's root, finding the
// root first when the saved URL isn't it. Returns the resolved root ("" when
// the saved URL was right).
func (r *Registry) loadSettings(ctx context.Context, conn models.GrafanaConnection, out any) (string, error) {
	resolved := r.findRoot(ctx, conn)
	return resolved, r.DoJSON(ctx, conn, http.MethodGet, "/api/frontend/settings", nil, nil, out)
}

// findRoot checks once per connection state whether the saved URL is
// Grafana's root, using the unauthenticated /api/health (so a probe never
// counts as a failed sign-in); if not, it tries the known sub paths.
func (r *Registry) findRoot(ctx context.Context, conn models.GrafanaConnection) string {
	st, err := r.state(conn)
	if err != nil {
		return ""
	}
	st.baseMu.Lock()
	checked := st.rootChecked
	st.baseMu.Unlock()
	if checked {
		return ""
	}
	base, err := BaseURL(conn)
	if err != nil {
		return ""
	}
	resolved := ""
	if !r.isGrafanaRoot(ctx, st, base) {
		for _, sub := range grafanaSubPaths {
			if strings.HasSuffix(base, sub) {
				continue
			}
			if r.isGrafanaRoot(ctx, st, base+sub) {
				resolved = base + sub
				break
			}
		}
	}
	st.baseMu.Lock()
	st.rootChecked = true
	if resolved != "" {
		st.base = resolved
	}
	st.baseMu.Unlock()
	return resolved
}

// isGrafanaRoot: GET <root>/api/health answers Grafana's health JSON.
func (r *Registry) isGrafanaRoot(ctx context.Context, st *connState, root string) bool {
	hctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, root+"/api/health", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	res, err := st.client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var h map[string]any
	if json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&h) != nil {
		return false
	}
	_, hasDB := h["database"]
	_, hasVersion := h["version"]
	return hasDB || hasVersion
}

func (r *Registry) fetchInfo(ctx context.Context, conn models.GrafanaConnection) (*ServerInfo, error) {
	var settings struct {
		AppSubURL         string `json:"appSubUrl"`
		Namespace         string `json:"namespace"`
		DefaultDatasource string `json:"defaultDatasource"`
		BuildInfo         struct {
			Version string `json:"version"`
		} `json:"buildInfo"`
		Datasources map[string]struct {
			ID        json.Number    `json:"id"`
			UID       string         `json:"uid"`
			Name      string         `json:"name"`
			Type      string         `json:"type"`
			IsDefault bool           `json:"isDefault"`
			Meta      map[string]any `json:"meta"`
			JSONData  map[string]any `json:"jsonData"`
		} `json:"datasources"`
	}
	resolved, err := r.loadSettings(ctx, conn, &settings)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{
		ResolvedURL: resolved,
		Datasources: []Datasource{},
		Version:     settings.BuildInfo.Version,
		Namespace:   settings.Namespace,
		DefaultDS:   settings.DefaultDatasource,
		AppSubURL:   settings.AppSubURL,
	}
	if info.Version == "" {
		var h struct {
			Version string `json:"version"`
		}
		if r.DoJSON(ctx, conn, http.MethodGet, "/api/health", nil, nil, &h) == nil {
			info.Version = h.Version
		}
	}
	info.Major, info.Minor = parseVersion(info.Version)
	if info.Namespace == "" {
		info.Namespace = "default"
	}
	for name, d := range settings.Datasources {
		// Pseudo datasources (-- Mixed --, -- Dashboard --) are handled by the
		// query planner, not listed.
		if d.Type == "datasource" && d.UID != "grafana" {
			continue
		}
		id, _ := d.ID.Int64()
		ds := Datasource{ID: id, UID: d.UID, Name: name, Type: d.Type, IsDefault: d.IsDefault}
		if d.Name != "" {
			ds.Name = d.Name
		}
		if b, ok := d.Meta["backend"].(bool); ok {
			ds.Backend = b
		}
		if ti, ok := d.JSONData["timeInterval"].(string); ok {
			ds.TimeInterval = ti
		}
		if tf, ok := d.JSONData["timeField"].(string); ok {
			ds.TimeField = tf
		}
		if ds.Name == info.DefaultDS {
			ds.IsDefault = true
		}
		info.Datasources = append(info.Datasources, ds)
	}
	sort.Slice(info.Datasources, func(i, j int) bool { return info.Datasources[i].Name < info.Datasources[j].Name })

	var u struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if r.DoJSON(ctx, conn, http.MethodGet, "/api/user", nil, nil, &u) == nil && u.Login != "" {
		info.User = &UserInfo{Login: u.Login, Name: u.Name, Email: u.Email}
		var orgs []struct {
			OrgID int64  `json:"orgId"`
			Role  string `json:"role"`
		}
		if r.DoJSON(ctx, conn, http.MethodGet, "/api/user/orgs", nil, nil, &orgs) == nil && len(orgs) > 0 {
			info.User.OrgRole = orgs[0].Role
		}
	}
	return info, nil
}

func parseVersion(v string) (int, int) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	major, minor := 0, 0
	if len(parts) > 0 {
		major, _ = strconv.Atoi(digits(parts[0]))
	}
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(digits(parts[1]))
	}
	return major, minor
}

func digits(s string) string {
	for i, c := range s {
		if c < '0' || c > '9' {
			return s[:i]
		}
	}
	return s
}

// Test probes the connection (version, user, datasources). An unsaved
// connection gets a temporary state id so every call of the probe shares one
// auth state; when that probe had to rotate a pasted session, the new cookie
// is returned so the caller saves the live value instead of the (now
// superseded) pasted one.
func (r *Registry) Test(ctx context.Context, conn models.GrafanaConnection) (*ServerInfo, string, error) {
	temp := conn.ID == ""
	if temp {
		conn.ID = fmt.Sprintf("%s%d", testIDPrefix, time.Now().UnixNano())
		defer r.Close(conn.ID)
	}
	r.Close(conn.ID)
	info, err := r.fetchInfo(ctx, conn)
	rotated := ""
	if conn.AuthKind == "session" {
		r.mu.Lock()
		if st, ok := r.conns[conn.ID]; ok {
			st.authMu.Lock()
			rotated = st.liveCookie
			st.authMu.Unlock()
		}
		r.mu.Unlock()
	}
	return info, rotated, err
}
