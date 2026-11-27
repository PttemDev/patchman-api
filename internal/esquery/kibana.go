package esquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
)

// Kibana data views (8.x) / index patterns (7.x, OpenSearch Dashboards) live
// in Kibana as saved objects, not in Elasticsearch. This file reads them over
// Kibana's public HTTP API:
//
//   - kibanaProxy connections already talk to Kibana; they reuse that URL,
//     space and Kibana auth (basic or session cookie).
//   - Direct ES connections may set an optional options.kibanaUrl (+
//     kibanaSpace). They reuse the ES Authorization header (basic / ApiKey —
//     Kibana accepts both; ES service tokens usually don't work on Kibana) and
//     get their OWN pooled TLS client built from kibanaCaCert /
//     kibanaInsecureSkipVerify, because the ES CA / fingerprint pin / mTLS pair
//     rarely match a Kibana on another host.

// ErrNoKibana is returned when the connection has no Kibana endpoint.
var ErrNoKibana = errors.New("no Kibana URL configured on this connection")

// ErrKibanaNotFound marks a Kibana 404 (unknown route on old versions, or an
// unknown data view id).
var ErrKibanaNotFound = errors.New("not found in Kibana")

// DataView is the version-neutral view of a Kibana data view / index pattern.
type DataView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Pattern   string `json:"pattern"`             // the index pattern (Kibana "title"), e.g. logs-*
	TimeField string `json:"timeField,omitempty"` // empty = no time field
	// RuntimeFields is Kibana's runtimeFieldMap: name → {type, script{source}}.
	// Only filled by the detail call (the 8.x list omits it).
	RuntimeFields map[string]any `json:"runtimeFields,omitempty"`
	Space         string         `json:"space,omitempty"`
	Tenant        string         `json:"tenant,omitempty"` // OSD security tenant ("" = OSD's default)
	// Detailed is true when TimeField/RuntimeFields are authoritative (detail
	// call or a 7.x _find that returned them).
	Detailed bool `json:"detailed"`
}

// KibanaSpace is one Kibana space.
type KibanaSpace struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	Initials    string `json:"initials,omitempty"`
}

// SpacesResult carries the spaces plus whether the server supports spaces at
// all (OpenSearch Dashboards / very old Kibana answer 404).
type SpacesResult struct {
	Spaces    []KibanaSpace `json:"spaces"`
	Supported bool          `json:"supported"`
	Default   string        `json:"default"` // the connection's configured space ("" = default)
}

var spaceIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,100}$`)

// SanitizeSpace validates a Kibana space id. "" and "default" both mean the
// default space (no /s/ prefix).
func SanitizeSpace(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" || s == "default" {
		return "", nil
	}
	if !spaceIDRe.MatchString(s) {
		return "", fmt.Errorf("invalid Kibana space id %q", s)
	}
	return s, nil
}

// HasKibana reports whether the connection can reach Kibana at all.
func HasKibana(conn models.ESConnection) bool {
	if strings.EqualFold(strings.TrimSpace(conn.AuthKind), "kibanaProxy") {
		return true
	}
	return kibanaURLOption(conn) != ""
}

func kibanaURLOption(conn models.ESConnection) string {
	ku, _ := parseOptions(conn.OptionsJSON)["kibanaUrl"].(string)
	return strings.TrimRight(strings.TrimSpace(ku), "/")
}

// ConfiguredSpace returns the connection's default space (options.kibanaSpace).
func ConfiguredSpace(conn models.ESConnection) string {
	s, _ := parseOptions(conn.OptionsJSON)["kibanaSpace"].(string)
	out, err := SanitizeSpace(s)
	if err != nil {
		return ""
	}
	return out
}

// kibanaTarget is the resolved Kibana endpoint for a connection.
type kibanaTarget struct {
	base   string
	client *http.Client
	auth   string // Authorization header value ("" = none)
	cookie string // Cookie header value ("" = none)
}

func (r *Registry) kibana(conn models.ESConnection) (*kibanaTarget, error) {
	if strings.EqualFold(strings.TrimSpace(conn.AuthKind), "kibanaProxy") {
		res, err := r.resolve(conn)
		if err != nil {
			return nil, err
		}
		t := &kibanaTarget{base: res.baseURL, client: res.client}
		if res.kibanaAuthMode == "cookie" {
			t.cookie = res.kibanaCookie
		} else {
			t.auth = res.kibanaAuthValue
		}
		return t, nil
	}
	ku := kibanaURLOption(conn)
	if ku == "" {
		return nil, ErrNoKibana
	}
	// No conn.TLS inheritance: the ES TLS switch describes the ES nodes. A bare
	// host defaults to https.
	base := withScheme(ku, true)
	cl, err := r.kibanaClient(conn)
	if err != nil {
		return nil, err
	}
	auth, err := r.authHeader(conn)
	if err != nil {
		return nil, err
	}
	return &kibanaTarget{base: base, client: cl, auth: auth}, nil
}

// kibanaClient pools a separate *http.Client for a direct connection's Kibana,
// built from the kibana* TLS options only.
func (r *Registry) kibanaClient(conn models.ESConnection) (*http.Client, error) {
	key := conn.ID + "#kibana"
	r.mu.Lock()
	if pc, ok := r.clients[key]; ok && conn.ID != "" {
		pc.lastUsed = time.Now()
		r.mu.Unlock()
		return pc.client, nil
	}
	r.mu.Unlock()

	opts := parseOptions(conn.OptionsJSON)
	kopts := map[string]any{}
	if v, ok := opts["kibanaCaCert"].(string); ok && strings.TrimSpace(v) != "" {
		kopts["caCert"] = v
	}
	if v, ok := opts["kibanaInsecureSkipVerify"].(bool); ok && v {
		kopts["insecureSkipVerify"] = true
	}
	b, _ := json.Marshal(kopts)
	cl, err := buildClient(models.ESConnection{OptionsJSON: string(b)})
	if err != nil {
		return nil, fmt.Errorf("kibana TLS: %w", err)
	}
	if conn.ID != "" {
		r.mu.Lock()
		r.clients[key] = &pooledClient{client: cl, lastUsed: time.Now()}
		r.mu.Unlock()
	}
	return cl, nil
}

// KibanaScope selects where Kibana saved objects are read: a Kibana space
// (/s/<id> prefix) and/or an OpenSearch Dashboards security tenant
// (securitytenant header). Kibana has spaces, OSD has tenants; an empty
// field means "the server's default".
type KibanaScope struct {
	Space  string
	Tenant string
}

func (s KibanaScope) cacheKey() string { return s.Space + "|t=" + s.Tenant }

// kibanaGet performs a GET against Kibana's public API in the given scope.
// A 404 is returned as ErrKibanaNotFound so callers can fall back to the 7.x
// saved-objects API.
func (r *Registry) kibanaGet(ctx context.Context, conn models.ESConnection, sc KibanaScope, path string) ([]byte, error) {
	space := sc.Space
	t, err := r.kibana(conn)
	if err != nil {
		return nil, err
	}
	u := t.base
	if space != "" {
		u += "/s/" + url.PathEscape(space)
	}
	u += path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setKibanaHeaders(req)
	if sc.Tenant != "" {
		// OSD's tenant resolver reads this header before the session cookie,
		// so it also overrides the tenant a kibanaProxy cookie was minted in.
		req.Header.Set("securitytenant", sc.Tenant)
	}
	if t.auth != "" {
		req.Header.Set("Authorization", t.auth)
	}
	if t.cookie != "" {
		req.Header.Set("Cookie", t.cookie)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kibana unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrKibanaNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("Kibana auth failed (401): check the credentials — an ES service token is usually not accepted by Kibana")
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("your Kibana user cannot read data views in this space (403): %s", kibanaMsg(raw))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("Kibana error %d: %s", resp.StatusCode, kibanaMsg(raw))
	}
	return raw, nil
}

// ---- cache --------------------------------------------------------------------

const kibanaCacheTTL = 60 * time.Second

type kibanaCacheEntry struct {
	at  time.Time
	val any
}

var (
	kibanaCacheMu sync.Mutex
	kibanaCache   = map[string]kibanaCacheEntry{}
)

func cacheGet(key string) (any, bool) {
	kibanaCacheMu.Lock()
	defer kibanaCacheMu.Unlock()
	e, ok := kibanaCache[key]
	if !ok || time.Since(e.at) > kibanaCacheTTL {
		delete(kibanaCache, key)
		return nil, false
	}
	return e.val, true
}

func cachePut(key string, v any) {
	kibanaCacheMu.Lock()
	defer kibanaCacheMu.Unlock()
	kibanaCache[key] = kibanaCacheEntry{at: time.Now(), val: v}
}

// dropKibanaCache removes every cached Kibana value of a connection (edit /
// delete / explicit refresh).
func dropKibanaCache(connID string) {
	kibanaCacheMu.Lock()
	defer kibanaCacheMu.Unlock()
	for k := range kibanaCache {
		if strings.HasPrefix(k, connID+"|") {
			delete(kibanaCache, k)
		}
	}
}

// RefreshKibana drops the cached data views / spaces of a connection.
func (r *Registry) RefreshKibana(connID string) { dropKibanaCache(connID) }

// ---- spaces -------------------------------------------------------------------

// Spaces lists Kibana spaces. A 404 (OpenSearch Dashboards, spaces disabled)
// yields a single default space with Supported=false.
func (r *Registry) Spaces(ctx context.Context, conn models.ESConnection) (*SpacesResult, error) {
	key := conn.ID + "|spaces"
	if v, ok := cacheGet(key); ok && conn.ID != "" {
		return v.(*SpacesResult), nil
	}
	out := &SpacesResult{Default: ConfiguredSpace(conn)}
	raw, err := r.kibanaGet(ctx, conn, KibanaScope{}, "/api/spaces/space")
	switch {
	case errors.Is(err, ErrKibanaNotFound):
		out.Spaces = []KibanaSpace{{ID: "default", Name: "Default"}}
	case err != nil:
		return nil, err
	default:
		var sp []KibanaSpace
		if err := json.Unmarshal(raw, &sp); err != nil {
			return nil, fmt.Errorf("unexpected Kibana spaces response: %w", err)
		}
		out.Spaces = sp
		out.Supported = true
	}
	if conn.ID != "" {
		cachePut(key, out)
	}
	return out, nil
}

// ---- data views ---------------------------------------------------------------

// DataViews lists data views in a scope: 8.x /api/data_views, falling back to
// the 7.x / OpenSearch Dashboards saved-objects _find on 404.
func (r *Registry) DataViews(ctx context.Context, conn models.ESConnection, sc KibanaScope) ([]DataView, error) {
	space := sc.Space
	key := conn.ID + "|dvs|" + sc.cacheKey()
	if v, ok := cacheGet(key); ok && conn.ID != "" {
		return v.([]DataView), nil
	}
	if err := r.checkTenant(ctx, conn, sc.Tenant); err != nil {
		return nil, err
	}
	raw, err := r.kibanaGet(ctx, conn, sc, "/api/data_views")
	var out []DataView
	switch {
	case errors.Is(err, ErrKibanaNotFound):
		out, err = r.findIndexPatterns(ctx, conn, sc)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		out, err = parseDataViewList(raw, space)
		if err != nil {
			return nil, err
		}
	}
	for i := range out {
		out[i].Tenant = sc.Tenant
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if conn.ID != "" {
		cachePut(key, out)
	}
	return out, nil
}

// findIndexPatterns pages through saved_objects/_find?type=index-pattern.
func (r *Registry) findIndexPatterns(ctx context.Context, conn models.ESConnection, sc KibanaScope) ([]DataView, error) {
	space := sc.Space
	const perPage = 1000
	var out []DataView
	for page := 1; page <= 20; page++ {
		q := url.Values{}
		q.Set("type", "index-pattern")
		q.Add("fields", "title")
		q.Add("fields", "timeFieldName")
		q.Add("fields", "name")
		q.Set("per_page", fmt.Sprint(perPage))
		q.Set("page", fmt.Sprint(page))
		raw, err := r.kibanaGet(ctx, conn, sc, "/api/saved_objects/_find?"+q.Encode())
		if err != nil {
			if errors.Is(err, ErrKibanaNotFound) {
				return nil, fmt.Errorf("this Kibana exposes neither /api/data_views nor /api/saved_objects")
			}
			return nil, err
		}
		rows, total, err := parseSavedObjectsFind(raw, space)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < perPage || len(out) >= total {
			break
		}
	}
	return out, nil
}

// DataView returns one data view with its time field + runtime fields.
func (r *Registry) DataView(ctx context.Context, conn models.ESConnection, sc KibanaScope, id string) (*DataView, error) {
	space := sc.Space
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("data view id is required")
	}
	key := conn.ID + "|dv|" + sc.cacheKey() + "|" + id
	if v, ok := cacheGet(key); ok && conn.ID != "" {
		dv := v.(DataView)
		return &dv, nil
	}
	if err := r.checkTenant(ctx, conn, sc.Tenant); err != nil {
		return nil, err
	}
	esc := url.PathEscape(id)
	raw, err := r.kibanaGet(ctx, conn, sc, "/api/data_views/data_view/"+esc)
	var dv *DataView
	switch {
	case errors.Is(err, ErrKibanaNotFound):
		raw, err = r.kibanaGet(ctx, conn, sc, "/api/saved_objects/index-pattern/"+esc)
		if err != nil {
			if errors.Is(err, ErrKibanaNotFound) {
				return nil, fmt.Errorf("data view %q not found in this space / tenant", id)
			}
			return nil, err
		}
		dv, err = parseSavedObject(raw, space)
	case err != nil:
		return nil, err
	default:
		dv, err = parseDataViewDetail(raw, space)
	}
	if err != nil {
		return nil, err
	}
	dv.Tenant = sc.Tenant
	if conn.ID != "" {
		cachePut(key, *dv)
	}
	return dv, nil
}

// ---- parsers (pure; unit-tested) ---------------------------------------------

// parseDataViewList parses 8.x GET /api/data_views:
// {"data_view":[{"id","name","title","namespaces","typeMeta"}]}.
func parseDataViewList(raw []byte, space string) ([]DataView, error) {
	var body struct {
		DataView []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Title         string `json:"title"`
			TimeFieldName string `json:"timeFieldName"`
		} `json:"data_view"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unexpected Kibana data views response: %w", err)
	}
	out := make([]DataView, 0, len(body.DataView))
	for _, d := range body.DataView {
		out = append(out, DataView{
			ID: d.ID, Name: nameOr(d.Name, d.Title), Pattern: d.Title,
			TimeField: d.TimeFieldName, Space: space,
		})
	}
	return out, nil
}

// parseDataViewDetail parses 8.x GET /api/data_views/data_view/{id}:
// {"data_view":{"id","name","title","timeFieldName","runtimeFieldMap":{...}}}.
func parseDataViewDetail(raw []byte, space string) (*DataView, error) {
	var body struct {
		DataView struct {
			ID              string         `json:"id"`
			Name            string         `json:"name"`
			Title           string         `json:"title"`
			TimeFieldName   string         `json:"timeFieldName"`
			RuntimeFieldMap map[string]any `json:"runtimeFieldMap"`
		} `json:"data_view"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unexpected Kibana data view response: %w", err)
	}
	d := body.DataView
	if d.ID == "" && d.Title == "" {
		return nil, fmt.Errorf("unexpected Kibana data view response")
	}
	return &DataView{
		ID: d.ID, Name: nameOr(d.Name, d.Title), Pattern: d.Title,
		TimeField: d.TimeFieldName, RuntimeFields: emptyNil(d.RuntimeFieldMap),
		Space: space, Detailed: true,
	}, nil
}

type savedObjectRow struct {
	ID         string `json:"id"`
	Attributes struct {
		Title         string `json:"title"`
		Name          string `json:"name"`
		TimeFieldName string `json:"timeFieldName"`
		// 7.11+ stores runtimeFieldMap as a JSON *string* attribute.
		RuntimeFieldMap json.RawMessage `json:"runtimeFieldMap"`
	} `json:"attributes"`
}

func (s savedObjectRow) toDataView(space string, detailed bool) DataView {
	return DataView{
		ID: s.ID, Name: nameOr(s.Attributes.Name, s.Attributes.Title),
		Pattern: s.Attributes.Title, TimeField: s.Attributes.TimeFieldName,
		RuntimeFields: decodeRuntimeMap(s.Attributes.RuntimeFieldMap),
		Space:         space, Detailed: detailed,
	}
}

// parseSavedObjectsFind parses GET /api/saved_objects/_find (7.x / OSD).
func parseSavedObjectsFind(raw []byte, space string) ([]DataView, int, error) {
	var body struct {
		Total        int              `json:"total"`
		SavedObjects []savedObjectRow `json:"saved_objects"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, 0, fmt.Errorf("unexpected Kibana saved objects response: %w", err)
	}
	out := make([]DataView, 0, len(body.SavedObjects))
	for _, so := range body.SavedObjects {
		// _find was asked for title+timeFieldName, so the time field is
		// authoritative; runtime fields still need the detail call.
		dv := so.toDataView(space, false)
		out = append(out, dv)
	}
	return out, body.Total, nil
}

// parseSavedObject parses GET /api/saved_objects/index-pattern/{id}.
func parseSavedObject(raw []byte, space string) (*DataView, error) {
	var so savedObjectRow
	if err := json.Unmarshal(raw, &so); err != nil {
		return nil, fmt.Errorf("unexpected Kibana saved object response: %w", err)
	}
	if so.ID == "" {
		return nil, fmt.Errorf("unexpected Kibana saved object response")
	}
	dv := so.toDataView(space, true)
	return &dv, nil
}

func decodeRuntimeMap(raw json.RawMessage) map[string]any {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var m map[string]any
	if strings.HasPrefix(s, "\"") {
		var inner string
		if json.Unmarshal(raw, &inner) != nil {
			return nil
		}
		if json.Unmarshal([]byte(inner), &m) != nil {
			return nil
		}
		return emptyNil(m)
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return emptyNil(m)
}

func emptyNil(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func nameOr(name, title string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return title
}

// PingKibana checks GET /api/status (used by the connection Test as a
// non-fatal warning).
func (r *Registry) PingKibana(ctx context.Context, conn models.ESConnection) error {
	_, err := r.kibanaGet(ctx, conn, KibanaScope{}, "/api/status")
	if errors.Is(err, ErrKibanaNotFound) {
		return nil // reachable, route absent (OSD variants) — fine
	}
	return err
}
