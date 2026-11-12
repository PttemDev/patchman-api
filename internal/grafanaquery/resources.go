package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"patchman-be/internal/models"
)

// Prometheus / Loki HTTP API access through Grafana. Three routes exist
// across versions; the first that works is remembered per datasource:
//
//	resources:  /api/datasources/uid/:uid/resources/<path>   (backend resource handler)
//	proxy-uid:  /api/datasources/proxy/uid/:uid/<path>        (8.x+)
//	proxy-id:   /api/datasources/proxy/:id/<path>             (oldest)
//
// Only the read paths below are reachable — the allowlist is what keeps a
// read grant from turning this into a generic datasource proxy.
var promAllow = []string{"api/v1/labels", "api/v1/label/", "api/v1/series", "api/v1/metadata", "api/v1/query", "api/v1/query_range"}
var lokiAllow = []string{"loki/api/v1/labels", "loki/api/v1/label/", "loki/api/v1/series", "loki/api/v1/query", "loki/api/v1/query_range"}

var routeCache sync.Map // connID|dsUID → "resources" | "proxy-uid" | "proxy-id"

func allowed(path string, list []string) bool {
	for _, p := range list {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) && strings.HasSuffix(path, "/values")) {
			return true
		}
	}
	return false
}

// DSResource calls an allowlisted Prometheus/Loki API path (without leading
// slash, e.g. "api/v1/labels" or "loki/api/v1/label/job/values") on a
// datasource and returns the raw JSON body.
func (r *Registry) DSResource(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, path string, q url.Values) ([]byte, error) {
	path = strings.TrimPrefix(path, "/")
	isLoki := strings.Contains(strings.ToLower(ds.Type), "loki")
	if isLoki {
		if !strings.HasPrefix(path, "loki/") {
			path = "loki/" + path
		}
		if !allowed(path, lokiAllow) {
			return nil, fmt.Errorf("path %q is not allowed", path)
		}
	} else if !allowed(path, promAllow) {
		return nil, fmt.Errorf("path %q is not allowed", path)
	}
	key := conn.ID + "|" + ds.UID
	routes := []string{"resources", "proxy-uid", "proxy-id"}
	if v, ok := routeCache.Load(key); ok {
		routes = append([]string{v.(string)}, routes...)
	}
	var lastErr error
	tried := map[string]bool{}
	for _, rt := range routes {
		if tried[rt] {
			continue
		}
		tried[rt] = true
		var p string
		switch rt {
		case "resources":
			rp := path
			if isLoki {
				// Loki's resource handler takes the bare API path (labels,
				// label/x/values, series, …).
				rp = strings.TrimPrefix(path, "loki/api/v1/")
			}
			p = "/api/datasources/uid/" + url.PathEscape(ds.UID) + "/resources/" + rp
		case "proxy-uid":
			p = "/api/datasources/proxy/uid/" + url.PathEscape(ds.UID) + "/" + path
		case "proxy-id":
			if ds.ID <= 0 {
				continue
			}
			p = fmt.Sprintf("/api/datasources/proxy/%d/%s", ds.ID, path)
		}
		resp, err := r.Do(ctx, conn, http.MethodGet, p, q, nil)
		if err != nil {
			lastErr = err
			if ae, ok := AsAPIError(err); ok && (ae.Code == "auth" || ae.Code == "session_expired") {
				return nil, err
			}
			continue
		}
		if !json.Valid(resp.Body) {
			lastErr = fmt.Errorf("datasource returned non-JSON")
			continue
		}
		routeCache.Store(key, rt)
		return resp.Body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("datasource API unavailable")
	}
	return nil, lastErr
}

// dsStrings decodes {"status":"success","data":[...strings]}.
func dsStrings(body []byte) ([]string, error) {
	var res struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if res.Status == "error" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return res.Data, nil
}

// dsSeries decodes {"data":[{label:value,...}]}.
func dsSeries(body []byte) ([]map[string]string, error) {
	var res struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
		Error  string              `json:"error"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if res.Status == "error" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return res.Data, nil
}
