package apm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"patchman-be/internal/servicemap"
)

// Provider is one observability backend.
type Provider interface {
	Name() string  // stable id: coroot, prometheus, …
	Title() string // display name
	// AuthKinds lists the accepted auth kinds; the first is the default.
	AuthKinds() []string
	APIKeyHeader() string
	SessionCookie() string
	// Login signs a "login" connection in and returns the session cookie.
	Login(ctx context.Context, c *Client) (string, error)
	Test(ctx context.Context, c *Client) (*TestResult, error)
	// Scopes lists what a map can be drawn for (Coroot projects, Datadog
	// envs, Kiali namespaces). Empty = the backend has no scopes.
	Scopes(ctx context.Context, c *Client) ([]Scope, error)
	// Map adds the backend's view of the service map to b.
	Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error
}

type TestResult struct {
	OK      bool     `json:"ok"`
	Version string   `json:"version,omitempty"`
	User    string   `json:"user,omitempty"`
	Scopes  []Scope  `json:"scopes,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

type Scope struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MapRequest struct {
	Scope      string // provider scope (project / env / namespaces)
	Namespaces []string
	Range      time.Duration
	End        time.Time
}

// Window is the PromQL range for a map request, at least 1m.
func (r MapRequest) Window() string {
	d := r.Range
	if d < time.Minute {
		d = 5 * time.Minute
	}
	return strconv.Itoa(int(d.Seconds())) + "s"
}

var providers = map[string]Provider{}

func register(p Provider) { providers[p.Name()] = p }

// Get returns a provider by name (nil when unknown).
func Get(name string) Provider { return providers[name] }

// Names lists the registered providers.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AuthAllowed reports whether provider p accepts auth kind k.
func AuthAllowed(p Provider, k string) bool {
	for _, a := range p.AuthKinds() {
		if a == k {
			return true
		}
	}
	return false
}

// base gives providers the defaults they rarely override.
type base struct{}

func (base) APIKeyHeader() string  { return "Authorization" }
func (base) SessionCookie() string { return "session" }
func (base) Login(context.Context, *Client) (string, error) {
	return "", &APIError{http.StatusBadRequest, "unsupported", "this provider has no sign-in flow"}
}
func (base) Scopes(context.Context, *Client) ([]Scope, error) { return nil, nil }

// promQuerier runs instant PromQL through a Prometheus-compatible API at
// prefix (".../api/v1"). rangeOnly backends (Coroot's API-key endpoints)
// only offer query_range, so the instant value is a one-point range.
type promQuerier struct {
	c         *Client
	prefix    string
	rangeOnly bool
	headers   map[string]string
}

type promResp struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (q promQuerier) Query(ctx context.Context, expr string, at time.Time) ([]servicemap.Sample, error) {
	v := url.Values{"query": {expr}}
	path := q.prefix + "/query"
	if q.rangeOnly {
		path = q.prefix + "/query_range"
		ts := strconv.FormatInt(at.Unix(), 10)
		v.Set("start", ts)
		v.Set("end", ts)
		v.Set("step", "60")
	} else {
		v.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	var r promResp
	raw, err := q.c.DoRawWith(ctx, http.MethodGet, path, v, nil, q.headers)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, &APIError{http.StatusBadGateway, "upstream", "the Prometheus API answered with something other than JSON — check the URL"}
	}
	if r.Status != "" && r.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", r.Error)
	}
	out := make([]servicemap.Sample, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		pt := s.Value
		if len(pt) == 0 && len(s.Values) > 0 {
			pt = s.Values[len(s.Values)-1]
		}
		if len(pt) != 2 {
			continue
		}
		str, _ := pt[1].(string)
		f, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(f) {
			continue
		}
		out = append(out, servicemap.Sample{Metric: s.Metric, Value: f})
	}
	return out, nil
}
