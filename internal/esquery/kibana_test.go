package esquery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/models"
)

func newTestRegistry() *Registry {
	return &Registry{clients: map[string]*pooledClient{}}
}

func directConn(id, kibanaURL, space string) models.ESConnection {
	opts := map[string]any{"kibanaUrl": kibanaURL}
	if space != "" {
		opts["kibanaSpace"] = space
	}
	b, _ := json.Marshal(opts)
	return models.ESConnection{ID: id, AuthKind: "none", Nodes: "http://es.invalid:9200", OptionsJSON: string(b)}
}

func TestSanitizeSpace(t *testing.T) {
	for in, want := range map[string]string{"": "", "default": "", " ops ": "ops", "/team-a/": "team-a"} {
		got, err := SanitizeSpace(in)
		if err != nil || got != want {
			t.Fatalf("SanitizeSpace(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../x", "a b", "A", "x/y"} {
		if _, err := SanitizeSpace(bad); err == nil {
			t.Fatalf("SanitizeSpace(%q) should fail", bad)
		}
	}
}

// Kibana 8.x: list via /api/data_views, detail via /api/data_views/data_view/{id};
// the space prefix is applied and the ES auth header is forwarded.
func TestDataViews8x(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("kbn-xsrf") == "" {
			t.Errorf("missing kbn-xsrf")
		}
		switch r.URL.Path {
		case "/s/ops/api/data_views":
			_, _ = io.WriteString(w, `{"data_view":[{"id":"b","name":"","title":"metrics-*"},{"id":"a","name":"Logs","title":"logs-*,-logs-debug*","namespaces":["ops"]}]}`)
		case "/s/ops/api/data_views/data_view/a":
			_, _ = io.WriteString(w, `{"data_view":{"id":"a","name":"Logs","title":"logs-*","timeFieldName":"@timestamp","runtimeFieldMap":{"lvl":{"type":"keyword","script":{"source":"emit('x')"}}}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	r := newTestRegistry()
	conn := directConn("c1", srv.URL, "ops")

	list, err := r.DataViews(context.Background(), conn, ConfiguredSpace(conn))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "Logs" || list[0].Pattern != "logs-*,-logs-debug*" || list[1].Name != "metrics-*" {
		t.Fatalf("unexpected list: %+v", list)
	}
	dv, err := r.DataView(context.Background(), conn, "ops", "a")
	if err != nil {
		t.Fatal(err)
	}
	if dv.TimeField != "@timestamp" || !dv.Detailed || dv.RuntimeFields["lvl"] == nil {
		t.Fatalf("unexpected detail: %+v", dv)
	}
	// Cached: a second list call must not hit Kibana.
	n := len(paths)
	if _, err := r.DataViews(context.Background(), conn, "ops"); err != nil || len(paths) != n {
		t.Fatalf("expected cache hit (calls %d→%d, err %v)", n, len(paths), err)
	}
	r.Close("c1")
	if _, err := r.DataViews(context.Background(), conn, "ops"); err != nil || len(paths) != n+1 {
		t.Fatalf("Close must drop the cache")
	}
}

// Kibana 7.x / OpenSearch Dashboards: /api/data_views 404 → saved objects.
func TestDataViews7xFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/saved_objects/_find":
			if r.URL.Query().Get("type") != "index-pattern" {
				t.Errorf("bad type %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"page":1,"per_page":1000,"total":1,"saved_objects":[{"id":"x","type":"index-pattern","attributes":{"title":"filebeat-*","timeFieldName":"@timestamp"}}]}`)
		case "/api/saved_objects/index-pattern/x":
			_, _ = io.WriteString(w, `{"id":"x","type":"index-pattern","attributes":{"title":"filebeat-*","timeFieldName":"@timestamp","runtimeFieldMap":"{\"host_up\":{\"type\":\"keyword\",\"script\":{\"source\":\"emit(doc['host.name'].value.toUpperCase())\"}}}"}}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"statusCode":404,"error":"Not Found"}`)
		}
	}))
	defer srv.Close()
	r := newTestRegistry()
	conn := directConn("c2", srv.URL, "")

	list, err := r.DataViews(context.Background(), conn, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Pattern != "filebeat-*" || list[0].TimeField != "@timestamp" {
		t.Fatalf("unexpected list: %+v", list)
	}
	dv, err := r.DataView(context.Background(), conn, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if dv.RuntimeFields["host_up"] == nil {
		t.Fatalf("runtimeFieldMap string attribute not decoded: %+v", dv)
	}
	sp, err := r.Spaces(context.Background(), conn)
	if err != nil || sp.Supported || len(sp.Spaces) != 1 || sp.Spaces[0].ID != "default" {
		t.Fatalf("spaces 404 must yield one default space: %+v %v", sp, err)
	}
}

func TestKibanaErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	r := newTestRegistry()
	if _, err := r.DataViews(context.Background(), directConn("c3", srv.URL, ""), ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
	if _, err := r.DataViews(context.Background(), models.ESConnection{ID: "c4", AuthKind: "basic"}, ""); err != ErrNoKibana {
		t.Fatalf("want ErrNoKibana, got %v", err)
	}
}

// The kibanaProxy mode reuses its Kibana URL + Kibana basic auth.
func TestDataViewsViaKibanaProxyConn(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data_view":[]}`)
	}))
	defer srv.Close()
	r := newTestRegistry()
	r.cipher = nil
	conn := models.ESConnection{ID: "c5", AuthKind: "kibanaProxy", Username: "u",
		OptionsJSON: `{"kibanaUrl":"` + srv.URL + `","kibanaAuthMode":"basic"}`}
	if _, err := r.DataViews(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("kibanaProxy basic auth not forwarded: %q", auth)
	}
}

func TestBuildSearchBody(t *testing.T) {
	dv := DataView{Pattern: "logs-*", TimeField: "@timestamp",
		RuntimeFields: map[string]any{
			"lvl":  map[string]any{"type": "keyword", "script": map[string]any{"source": "emit('x')"}},
			"comp": map[string]any{"type": "composite"},
		}}
	req := DataViewSearchRequest{Query: "level:error", Histogram: true}
	if err := normalizeDVRequest(&req); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(buildSearchBody(dv, req, "", false))
	s := string(b)
	for _, want := range []string{
		`"range":{"@timestamp":{"format":"strict_date_optional_time","gte":"now-15m","lte":"now"}}`,
		`"query_string":{"analyze_wildcard":true,"lenient":true,"query":"level:error"}`,
		`"sort":[{"@timestamp":{"order":"desc","unmapped_type":"boolean"}}]`,
		`"runtime_mappings":{"lvl":{"script":{"source":"emit('x')"},"type":"keyword"}}`,
		`"fields":["lvl"]`,
		`"auto_date_histogram":{"buckets":60,"field":"@timestamp"}`,
		`"track_total_hits":true`,
		`"size":100`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %s\n%s", want, s)
		}
	}

	// OpenSearch: no runtime_mappings. Next page: search_after + pit, no aggs.
	req.SearchAfter = []any{1700000000000, 42}
	b, _ = json.Marshal(buildSearchBody(dv, req, "PIT1", true))
	s = string(b)
	if strings.Contains(s, "runtime_mappings") || strings.Contains(s, "aggs") || strings.Contains(s, "track_total_hits") {
		t.Fatalf("unexpected keys on page 2 / opensearch: %s", s)
	}
	if !strings.Contains(s, `"pit":{"id":"PIT1","keep_alive":"1m"}`) || !strings.Contains(s, `"search_after":[1700000000000,42]`) {
		t.Fatalf("missing pit/search_after: %s", s)
	}

	// No time field: match_all, no range / sort / histogram.
	b, _ = json.Marshal(buildSearchBody(DataView{Pattern: "x"}, DataViewSearchRequest{Size: 10, Histogram: true, From: "a", To: "b"}, "", false))
	s = string(b)
	if !strings.Contains(s, "match_all") || strings.Contains(s, "range") || strings.Contains(s, "sort") || strings.Contains(s, "aggs") {
		t.Fatalf("no-time-field body wrong: %s", s)
	}
}

func TestConsoleQueryPath(t *testing.T) {
	q, err := ConsoleQuery(DataView{Pattern: "logs-*,-logs-debug*", TimeField: "ts"}, DataViewSearchRequest{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if q.Method != "POST" || q.Path != "/logs-*,-logs-debug*/_search?ignore_unavailable=true&allow_no_indices=true" {
		t.Fatalf("unexpected console query: %s %s", q.Method, q.Path)
	}
}

// Search opens a PIT on the first page, searches /_search with it, and parses
// hits / total / histogram; when PIT fails it falls back to the pattern path.
func TestDataViewSearchPIT(t *testing.T) {
	var searchPath string
	var searchBody map[string]any
	var closedPit string
	pitOK := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/_pit":
			var b struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			closedPit = b.ID
			_, _ = io.WriteString(w, `{"succeeded":true}`)
		case strings.HasSuffix(r.URL.Path, "/_pit"):
			if !pitOK {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"no pit"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"PIT-A"}`)
		case strings.HasSuffix(r.URL.Path, "/_search"):
			searchPath = r.URL.Path
			searchBody = nil
			_ = json.NewDecoder(r.Body).Decode(&searchBody)
			_, _ = io.WriteString(w, `{"pit_id":"PIT-B","hits":{"total":{"value":1234,"relation":"eq"},"hits":[{"_id":"1","_index":"logs-1","_source":{"msg":"hi"},"sort":[1,2]}]},"aggregations":{"histogram":{"interval":"1m","buckets":[{"key":1,"key_as_string":"t","doc_count":5}]}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	r := newTestRegistry()
	conn := models.ESConnection{ID: "c6", AuthKind: "none", Nodes: srv.URL}
	dv := DataView{Pattern: "logs-*", TimeField: "@timestamp"}

	res, err := r.DataViewSearch(context.Background(), conn, dv, DataViewSearchRequest{Size: 1, Histogram: true})
	if err != nil {
		t.Fatal(err)
	}
	if searchPath != "/_search" || searchBody["pit"] == nil {
		t.Fatalf("PIT search expected, got path %s body %v", searchPath, searchBody)
	}
	if res.Total != 1234 || len(res.Hits) != 1 || res.PitID != "PIT-B" || len(res.Buckets) != 1 || res.Interval != "1m" || !res.HasMore {
		t.Fatalf("unexpected result: %+v", res)
	}
	if closedPit != "" {
		t.Fatalf("a full page must keep the PIT open")
	}
	// A short (last) page closes the PIT and returns no pit id.
	last, err := r.DataViewSearch(context.Background(), conn, dv, DataViewSearchRequest{Size: 5, PitID: "PIT-B", SearchAfter: []any{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if closedPit != "PIT-B" || last.PitID != "" || last.HasMore {
		t.Fatalf("last page must close the PIT: closed=%q res=%+v", closedPit, last)
	}

	pitOK = false
	if _, err := r.DataViewSearch(context.Background(), conn, dv, DataViewSearchRequest{}); err != nil {
		t.Fatal(err)
	}
	if searchPath != "/logs-*/_search" || searchBody["pit"] != nil {
		t.Fatalf("fallback must search the pattern without pit, got %s", searchPath)
	}

	// Dry run never touches ES.
	searchPath = ""
	dr, err := r.DataViewSearch(context.Background(), conn, dv, DataViewSearchRequest{DryRun: true})
	if err != nil || searchPath != "" || !strings.HasPrefix(dr.Query.Path, "/logs-*/_search") {
		t.Fatalf("dry run wrong: %+v %v path=%s", dr, err, searchPath)
	}
}
