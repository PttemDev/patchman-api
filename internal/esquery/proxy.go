package esquery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
)

// Class distinguishes a read-only forwarded request from a mutating one.
type Class int

const (
	ClassRead Class = iota
	ClassWrite
)

// errWriteRequired is returned by Do when the classifier flags a write but the
// caller did not pass allowWrite. Handlers translate it to a 403.
type errWriteRequired struct{}

func (errWriteRequired) Error() string { return "write access required" }

// IsWriteRequired reports whether err is the write-gate sentinel.
func IsWriteRequired(err error) bool {
	_, ok := err.(errWriteRequired)
	return ok
}

// readPOSTMarkers are path substrings that make a POST/GET a READ. A POST to
// any of these is a search-family call, not a mutation.
var readPOSTMarkers = []string{
	"_search", "_count", "_msearch", "_field_caps", "_mapping", "_cat",
	"_analyze", "_validate", "_sql", "_explain", "_tasks", "_nodes",
	"_cluster/health", "_cluster/state", "_alias", "_settings", "_stats",
}

// writePOSTMarkers are path substrings that make a POST/PATCH a WRITE.
var writePOSTMarkers = []string{
	"_bulk", "_update", "_delete_by_query", "_reindex", "_update_by_query",
	"_create", "_doc",
}

// Classify returns whether a (method, path) pair is a read or a write.
//   - GET / HEAD → read.
//   - PUT / DELETE → write.
//   - POST / PATCH → read if the path names a search-family endpoint, write if
//     it names a mutating endpoint; ambiguous mutating verbs default to write.
func Classify(method, path string) Class {
	m := strings.ToUpper(strings.TrimSpace(method))
	p := strings.ToLower(path)
	switch m {
	case http.MethodGet, http.MethodHead:
		return ClassRead
	case http.MethodPut, http.MethodDelete:
		return ClassWrite
	case http.MethodPost, http.MethodPatch:
		// Read-family endpoints win first (e.g. POST _search, POST _sql).
		for _, marker := range readPOSTMarkers {
			if strings.Contains(p, marker) {
				return ClassRead
			}
		}
		for _, marker := range writePOSTMarkers {
			if strings.Contains(p, marker) {
				return ClassWrite
			}
		}
		// Ambiguous mutating verb → write.
		return ClassWrite
	default:
		return ClassWrite
	}
}

// Do forwards a single request. It classifies (method, path); a write with
// allowWrite=false is rejected with the write-gate sentinel. Otherwise the
// call is forwarded to baseURL+path with the auth header + JSON body, and the
// upstream status/body are returned verbatim.
func (r *Registry) Do(ctx context.Context, conn models.ESConnection, method, path string, body []byte, allowWrite bool) (*ExecResult, error) {
	if Classify(method, path) == ClassWrite && !allowWrite {
		return nil, errWriteRequired{}
	}
	return r.rawDo(ctx, conn, method, path, body)
}

// rawDo performs the HTTP forward with no classification (Test / Cluster /
// Mapping use it directly for known-read calls).
func (r *Registry) rawDo(ctx context.Context, conn models.ESConnection, method, path string, body []byte) (*ExecResult, error) {
	res, err := r.resolve(conn)
	if err != nil {
		return nil, err
	}

	var reqURL, outMethod string
	if res.isKibanaProxy {
		// Route the whole ES verb+path+query through Kibana's Console proxy. The
		// raw ES path (INCLUDING its own ?query) is the `path` query value —
		// url.Values.Encode() percent-encodes the embedded ?/&. The outgoing
		// method is always POST; the ES verb travels in the `method` param.
		q := url.Values{}
		q.Set("method", strings.ToUpper(method))
		q.Set("path", path)
		kbase := res.baseURL
		if res.kibanaSpace != "" {
			kbase += "/s/" + url.PathEscape(res.kibanaSpace)
		}
		reqURL = kbase + "/api/console/proxy?" + q.Encode()
		outMethod = http.MethodPost
	} else {
		reqURL = res.baseURL + normalizePath(path)
		outMethod = strings.ToUpper(method)
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, outMethod, reqURL, reader)
	if err != nil {
		return nil, err
	}

	if res.isKibanaProxy {
		req.Header.Set("Content-Type", kibanaContentType(path))
		setKibanaHeaders(req)
		if res.kibanaAuthMode == "cookie" {
			if res.kibanaCookie != "" {
				req.Header.Set("Cookie", res.kibanaCookie)
			}
		} else if res.kibanaAuthValue != "" {
			req.Header.Set("Authorization", res.kibanaAuthValue)
		}
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if res.authHeader != "" {
			req.Header.Set("Authorization", res.authHeader)
		}
	}

	start := time.Now()
	resp, err := res.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	took := int(time.Since(start).Milliseconds())

	// For kibanaProxy the true ES status hides in x-console-proxy-status-code
	// (8.x/9.x return HTTP 200 even on ES errors). When that header is absent on
	// a non-2xx response, the failure never reached ES — it's a Kibana-level
	// error (auth/privilege/xsrf/payload/reachability); map it to a clear error.
	status := resp.StatusCode
	if res.isKibanaProxy {
		if h := resp.Header.Get("x-console-proxy-status-code"); h != "" {
			if n, e := strconv.Atoi(h); e == nil {
				status = n
			}
		} else if resp.StatusCode >= 400 {
			return nil, kibanaProxyError(resp.StatusCode, raw)
		}
	}

	if len(raw) == 0 {
		raw = []byte("null")
	} else if !json.Valid(raw) {
		// Wrap non-JSON upstream bodies so the FE always gets valid JSON.
		wrapped, _ := json.Marshal(string(raw))
		raw = wrapped
	}
	return &ExecResult{Status: status, TookMs: took, Body: json.RawMessage(raw)}, nil
}

// setKibanaHeaders adds the headers every Kibana Console proxy call needs.
func setKibanaHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	// Kibana requires kbn-xsrf; OpenSearch Dashboards (the OSS fork) requires
	// osd-xsrf instead and 400s without it. Each ignores the other's header,
	// so send both to support Kibana AND OpenSearch Dashboards.
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("osd-xsrf", "true")
	// The Console proxy is an *internal* Kibana route. With
	// server.restrictInternalApis on (Elastic Cloud / serverless, newer
	// 8.x-9.x deployments) Kibana answers 400 "uri [/api/console/proxy]
	// with method [post] exists but is not available with the current
	// configuration" unless the request says it comes from Kibana's own
	// UI — which is what its Dev Tools sends. Older Kibana and OpenSearch
	// Dashboards ignore the header.
	req.Header.Set("x-elastic-internal-origin", "Kibana")
}

// kibanaContentType keeps application/json unless the ES path targets a
// bulk/msearch endpoint (whose body is NDJSON), in which case Kibana expects
// application/x-ndjson.
func kibanaContentType(path string) string {
	p := path
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(strings.ToLower(p), "/")
	if strings.HasSuffix(p, "_bulk") || strings.HasSuffix(p, "_msearch") {
		return "application/x-ndjson"
	}
	return "application/json"
}

// kibanaProxyError maps a Kibana-level (pre-ES) failure status to a clear,
// user-facing message so Test + Request explain it's a Kibana auth/privilege/
// config problem, not an ES one.
func kibanaProxyError(status int, body []byte) error {
	switch status {
	case http.StatusBadRequest: // 400
		msg := kibanaMsg(body)
		if strings.Contains(msg, "not available with the current configuration") {
			return fmt.Errorf("Kibana refused the Console proxy (400): its internal APIs are restricted (server.restrictInternalApis) — %s", msg)
		}
		return fmt.Errorf("Kibana rejected the request (400): %s", msg)
	case http.StatusUnauthorized: // 401
		return fmt.Errorf("Kibana auth failed (check credentials / the session cookie may have expired)")
	case http.StatusForbidden: // 403
		return fmt.Errorf("Your Kibana user lacks Dev Tools / Console access")
	case http.StatusRequestEntityTooLarge: // 413
		return fmt.Errorf("Kibana rejected the request (413): payload too large (exceeds server.maxPayload)")
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 502/503/504
		return fmt.Errorf("Kibana could not reach Elasticsearch (%d)", status)
	default:
		return fmt.Errorf("Kibana proxy error %d: %s", status, kibanaMsg(body))
	}
}

// kibanaMsg extracts the human message from a Kibana error body
// ({statusCode,error,message}), falling back to a trimmed raw snippet.
func kibanaMsg(body []byte) string {
	var k struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &k) == nil {
		if strings.TrimSpace(k.Message) != "" {
			return k.Message
		}
		if strings.TrimSpace(k.Error) != "" {
			return k.Error
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// normalizePath ensures the forwarded path starts with a single leading slash.
func normalizePath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// Cluster aggregates GET / + _cluster/health + _cat/nodes into a ClusterResult
// and derives the flavor from the info tagline / version.distribution.
func (r *Registry) Cluster(ctx context.Context, conn models.ESConnection) (*ClusterResult, error) {
	var (
		wg                 sync.WaitGroup
		infoRes, healthRes *ExecResult
		nodesRes           *ExecResult
		infoErr, healthErr error
		nodesErr           error
	)
	wg.Add(3)
	go func() { defer wg.Done(); infoRes, infoErr = r.rawDo(ctx, conn, http.MethodGet, "/", nil) }()
	go func() {
		defer wg.Done()
		healthRes, healthErr = r.rawDo(ctx, conn, http.MethodGet, "/_cluster/health", nil)
	}()
	go func() {
		defer wg.Done()
		nodesRes, nodesErr = r.rawDo(ctx, conn, http.MethodGet,
			"/_cat/nodes?format=json&h=name,version,node.role,master,heap.percent,cpu,load_1m,disk.used_percent", nil)
	}()
	wg.Wait()

	if infoErr != nil {
		return nil, infoErr
	}

	out := &ClusterResult{}

	// GET / → name, cluster_name, version, distribution/tagline.
	var info struct {
		Name        string `json:"name"`
		ClusterName string `json:"cluster_name"`
		Tagline     string `json:"tagline"`
		Version     struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if infoRes != nil {
		_ = json.Unmarshal(infoRes.Body, &info)
	}
	out.Info.Name = info.Name
	out.Info.ClusterName = info.ClusterName
	out.Info.Version = info.Version.Number
	out.Info.Flavor = deriveFlavor(conn.Flavor, info.Version.Distribution, info.Tagline)

	// _cluster/health → status + shard/node counts.
	if healthErr == nil && healthRes != nil {
		var health struct {
			Status            string `json:"status"`
			NumberOfNodes     int    `json:"number_of_nodes"`
			NumberOfDataNodes int    `json:"number_of_data_nodes"`
			ActiveShards      int    `json:"active_shards"`
			UnassignedShards  int    `json:"unassigned_shards"`
			RelocatingShards  int    `json:"relocating_shards"`
			ClusterName       string `json:"cluster_name"`
		}
		if err := json.Unmarshal(healthRes.Body, &health); err == nil {
			out.Info.HealthStatus = health.Status
			out.Info.NumberOfNodes = health.NumberOfNodes
			out.Info.NumberOfDataNodes = health.NumberOfDataNodes
			out.Info.ActiveShards = health.ActiveShards
			out.Info.UnassignedShards = health.UnassignedShards
			out.Info.RelocatingShards = health.RelocatingShards
			if out.Info.ClusterName == "" {
				out.Info.ClusterName = health.ClusterName
			}
		}
	}

	// _cat/nodes → per-node rows.
	out.Nodes = []NodeInfo{}
	if nodesErr == nil && nodesRes != nil {
		var rows []map[string]any
		if err := json.Unmarshal(nodesRes.Body, &rows); err == nil {
			for _, row := range rows {
				out.Nodes = append(out.Nodes, NodeInfo{
					Name:        catStr(row, "name"),
					Version:     catStr(row, "version"),
					Roles:       catStr(row, "node.role"),
					HeapPercent: catStr(row, "heap.percent"),
					CPU:         catStr(row, "cpu"),
					Load:        catStr(row, "load_1m"),
					DiskPercent: catStr(row, "disk.used_percent"),
				})
			}
		}
	}
	return out, nil
}

// Indices lists indices via _cat/indices, flattening each row to IndexRow.
func (r *Registry) Indices(ctx context.Context, conn models.ESConnection) ([]IndexRow, error) {
	res, err := r.rawDo(ctx, conn, http.MethodGet,
		// NB: keep the h= column set to names supported across Elasticsearch AND
		// OpenSearch — an unknown _cat column returns 400 and breaks the whole
		// listing. pri.store.size is safe on both; creation date is read from
		// the index _settings in the detail drawer instead (index.creation_date).
		"/_cat/indices?format=json&h=health,status,index,uuid,docs.count,docs.deleted,pri,rep,store.size,pri.store.size&bytes=b", nil)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, upstreamError(res)
	}
	var rows []map[string]any
	if err := json.Unmarshal(res.Body, &rows); err != nil {
		return nil, fmt.Errorf("parse _cat/indices: %w", err)
	}
	out := make([]IndexRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, IndexRow{
			Health:       catStr(row, "health"),
			Status:       catStr(row, "status"),
			Index:        catStr(row, "index"),
			UUID:         catStr(row, "uuid"),
			DocsCount:    catInt(row, "docs.count"),
			DocsDeleted:  catInt(row, "docs.deleted"),
			PriShards:    int(catInt(row, "pri")),
			RepShards:    int(catInt(row, "rep")),
			StoreSize:    catStr(row, "store.size"),
			PriStoreSize: catStr(row, "pri.store.size"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// Aliases lists aliases via _cat/aliases, flattening each row to AliasRow.
// One alias fanned across N indices yields N rows (one per target index), the
// same shape _cat/aliases returns.
func (r *Registry) Aliases(ctx context.Context, conn models.ESConnection) ([]AliasRow, error) {
	res, err := r.rawDo(ctx, conn, http.MethodGet,
		"/_cat/aliases?format=json&h=alias,index,filter,routing.index,routing.search,is_write_index", nil)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, upstreamError(res)
	}
	var rows []map[string]any
	if err := json.Unmarshal(res.Body, &rows); err != nil {
		return nil, fmt.Errorf("parse _cat/aliases: %w", err)
	}
	out := make([]AliasRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, AliasRow{
			Alias:         catStr(row, "alias"),
			Index:         catStr(row, "index"),
			Filter:        catStr(row, "filter"),
			RoutingIndex:  catStr(row, "routing.index"),
			RoutingSearch: catStr(row, "routing.search"),
			IsWriteIndex:  catStr(row, "is_write_index"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alias != out[j].Alias {
			return out[i].Alias < out[j].Alias
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// deriveFlavor resolves elasticsearch | opensearch from the configured value,
// falling back to the version.distribution / tagline when set to auto.
func deriveFlavor(configured, distribution, tagline string) string {
	c := strings.ToLower(strings.TrimSpace(configured))
	if c == "elasticsearch" || c == "opensearch" {
		return c
	}
	d := strings.ToLower(distribution)
	if strings.Contains(d, "opensearch") {
		return "opensearch"
	}
	if strings.Contains(strings.ToLower(tagline), "opensearch") {
		return "opensearch"
	}
	if d == "elasticsearch" || strings.Contains(strings.ToLower(tagline), "elasticsearch") {
		return "elasticsearch"
	}
	return "elasticsearch"
}

// DeriveFlavorPublic exposes deriveFlavor to handlers (Test probe).
func DeriveFlavorPublic(configured, distribution, tagline string) string {
	return deriveFlavor(configured, distribution, tagline)
}

func upstreamError(res *ExecResult) error {
	return fmt.Errorf("upstream status %d: %s", res.Status, string(res.Body))
}

// catStr coerces a _cat row cell to string.
func catStr(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// catInt coerces a _cat row cell to int64 (cells arrive as JSON strings).
func catInt(row map[string]any, key string) int64 {
	s := strings.TrimSpace(catStr(row, key))
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
