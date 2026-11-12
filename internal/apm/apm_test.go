package apm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"
	"patchman-be/internal/services"
)

func testCipher(t *testing.T) *services.TokenCipher {
	t.Helper()
	c, err := services.NewTokenCipher("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type series struct {
	m map[string]string
	v float64
}

// fakeProm answers instant queries by the first rule whose needle the
// query contains.
func fakeProm(t *testing.T, rules []struct {
	needle string
	rows   []series
}, check func(r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		if !strings.HasSuffix(r.URL.Path, "/query") && !strings.HasSuffix(r.URL.Path, "/query_range") {
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query().Get("query")
		var res []map[string]any
		for _, rule := range rules {
			if strings.Contains(q, rule.needle) {
				for _, s := range rule.rows {
					v := []any{float64(time.Now().Unix()), jsonNum(s.v)}
					if strings.HasSuffix(r.URL.Path, "/query_range") {
						res = append(res, map[string]any{"metric": s.m, "values": []any{v}})
					} else {
						res = append(res, map[string]any{"metric": s.m, "value": v})
					}
				}
				break
			}
		}
		if res == nil {
			res = []map[string]any{}
		}
		rt := "vector"
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			rt = "matrix"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": rt, "result": res}})
	}))
}

func jsonNum(v float64) string { b, _ := json.Marshal(v); return string(b) }

func edgeMap(g *servicemap.Graph) map[string]*servicemap.Edge {
	m := map[string]*servicemap.Edge{}
	for _, e := range g.Edges {
		m[e.From+" > "+e.To] = e
	}
	return m
}

func names(m map[string]*servicemap.Edge) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPrometheusFamilies(t *testing.T) {
	type rule = struct {
		needle string
		rows   []series
	}
	srv := fakeProm(t, []rule{
		{`count by (__name__)`, []series{
			{map[string]string{"__name__": "container_http_requests_total"}, 10},
			{map[string]string{"__name__": "istio_requests_total"}, 4},
			{map[string]string{"__name__": "traces_service_graph_request_total"}, 2},
			{map[string]string{"__name__": "hubble_http_requests_total"}, 2},
			{map[string]string{"__name__": "response_total"}, 9},
		}},
		{`response_total{direction="outbound", dst_namespace!=""}`, []series{{map[string]string{}, 3}}},
		// coroot-node-agent
		{`container_http_requests_total[`, []series{
			{map[string]string{"proto": "http", "container_id": "/k8s/shop/web-5d4f9c7b8-x2x9z/web", "actual_destination": "10.0.0.5:8080", "status": "200"}, 9},
			{map[string]string{"proto": "http", "container_id": "/k8s/shop/web-5d4f9c7b8-x2x9z/web", "actual_destination": "10.0.0.5:8080", "status": "500"}, 1},
			{map[string]string{"proto": "postgres", "container_id": "/k8s/shop/api-7c9d8f6b5-abcde/api", "actual_destination": "10.0.0.9:5432", "status": "ok"}, 20},
			{map[string]string{"proto": "rabbitmq", "container_id": "/k8s/shop/worker-6b7c8d9e0-qwert/worker", "actual_destination": "10.0.0.7:5672", "method": "publish"}, 4},
			{map[string]string{"proto": "rabbitmq", "container_id": "/k8s/shop/worker-6b7c8d9e0-qwert/worker", "actual_destination": "10.0.0.7:5672", "method": "deliver"}, 4},
		}},
		{`histogram_quantile(0.95, sum by (le, container_id`, []series{
			{map[string]string{"proto": "http", "container_id": "/k8s/shop/web-5d4f9c7b8-x2x9z/web", "actual_destination": "10.0.0.5:8080"}, 0.120},
		}},
		{`kube_pod_info`, []series{
			{map[string]string{"namespace": "shop", "pod": "api-7c9d8f6b5-abcde", "pod_ip": "10.0.0.5", "created_by_kind": "ReplicaSet", "created_by_name": "api-7c9d8f6b5"}, 1},
			{map[string]string{"namespace": "shop", "pod": "postgres-0", "pod_ip": "10.0.0.9", "created_by_kind": "StatefulSet", "created_by_name": "postgres"}, 1},
		}},
		{`kube_replicaset_owner`, []series{
			{map[string]string{"namespace": "shop", "replicaset": "api-7c9d8f6b5", "owner_kind": "Deployment", "owner_name": "api"}, 1},
		}},
		{`ip_to_fqdn`, []series{{map[string]string{"ip": "10.0.0.7", "fqdn": "rabbit.internal."}, 1}}},
		// Istio
		{`istio_requests_total{reporter="destination"}`, []series{
			{map[string]string{"source_workload": "productpage-v1", "source_workload_namespace": "bookinfo", "destination_workload": "reviews-v1", "destination_workload_namespace": "bookinfo", "response_code": "200", "request_protocol": "http"}, 8},
			{map[string]string{"source_workload": "productpage-v1", "source_workload_namespace": "bookinfo", "destination_workload": "reviews-v1", "destination_workload_namespace": "bookinfo", "response_code": "503", "request_protocol": "http"}, 2},
		}},
		{`istio_request_duration_milliseconds_bucket`, []series{
			{map[string]string{"source_workload": "productpage-v1", "source_workload_namespace": "bookinfo", "destination_workload": "reviews-v1", "destination_workload_namespace": "bookinfo"}, 42},
		}},
		// Linkerd
		{`rate(response_total{direction="outbound"}`, []series{
			{map[string]string{"namespace": "emojivoto", "deployment": "web", "dst_namespace": "emojivoto", "dst_deployment": "emoji", "classification": "success"}, 5},
		}},
		// OTel service graph
		{`traces_service_graph_request_total[`, []series{
			{map[string]string{"client": "frontend", "server": "checkout", "connection_type": ""}, 3},
			{map[string]string{"client": "checkout", "server": "orders-postgres", "connection_type": "database"}, 6},
		}},
		{`traces_service_graph_request_failed_total`, []series{
			{map[string]string{"client": "frontend", "server": "checkout", "connection_type": ""}, 0.3},
		}},
		// Hubble
		{`hubble_http_requests_total[`, []series{
			{map[string]string{"source": "default/client", "destination": "default/server", "status": "200"}, 1.5},
		}},
	}, nil)
	defer srv.Close()

	reg := NewRegistry(testCipher(t))
	cl, err := reg.Client(models.ObservabilityConnection{Provider: "prometheus", URL: srv.URL, AuthKind: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b := servicemap.NewBuilder()
	st := &servicemap.SourceStatus{OK: true}
	if err := cl.Provider.Map(context.Background(), cl, MapRequest{Range: 5 * time.Minute}, b, st); err != nil {
		t.Fatal(err)
	}
	g := b.Graph(nil)
	edges := edgeMap(g)
	want := []string{
		"shop/Deployment/web > shop/Deployment/api",
		"shop/Deployment/api > shop/StatefulSet/postgres",
		"shop/Deployment/worker > ext:rabbit.internal:5672",
		"apm:bookinfo/productpage-v1 > apm:bookinfo/reviews-v1",
		"apm:emojivoto/web > apm:emojivoto/emoji",
		"apm:frontend > apm:checkout",
		"apm:checkout > apm:orders-postgres",
		"apm:default/client > apm:default/server",
	}
	for _, w := range want {
		if edges[w] == nil {
			t.Errorf("missing edge %s; have %v", w, names(edges))
		}
	}
	if e := edges["shop/Deployment/web > shop/Deployment/api"]; e != nil {
		if e.Stats.RPS != 10 || e.Stats.ErrorRate != 0.1 || e.Stats.P95Ms != 120 || e.Protocol != "http" {
			t.Errorf("coroot http edge stats %+v proto %s", e.Stats, e.Protocol)
		}
		if e.Stats.Statuses["200"] != 9 || e.Stats.Statuses["500"] != 1 {
			t.Errorf("coroot http edge statuses %v", e.Stats.Statuses)
		}
	}
	if e := edges["apm:bookinfo/productpage-v1 > apm:bookinfo/reviews-v1"]; e != nil && (e.Stats.RPS != 10 || e.Stats.ErrorRate != 0.2 || e.Stats.P95Ms != 42 || e.Stats.Statuses["503"] != 2) {
		t.Errorf("istio stats %+v", e.Stats)
	}
	if e := edges["apm:frontend > apm:checkout"]; e != nil && (e.Stats.ErrorRate < 0.0999 || e.Stats.ErrorRate > 0.1001) {
		t.Errorf("otel error rate %+v", e.Stats)
	}
	// Publish and consume are separate edges onto the broker.
	n := 0
	for _, e := range g.Edges {
		if e.From == "shop/Deployment/worker" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("worker edges %d, want publish + consume", n)
	}
	if node := b.Node("shop/StatefulSet/postgres"); node == nil || node.Kind != servicemap.KindDatastore || node.Stats.RPS != 20 {
		t.Errorf("postgres node %+v", node)
	}
	if node := b.Node("apm:orders-postgres"); node == nil || node.Kind != servicemap.KindDatastore || node.Subtype != "postgres" {
		t.Errorf("otel db node %+v", node)
	}
	if !strings.Contains(strings.Join(st.Notes, ","), "family:coroot") || !strings.Contains(strings.Join(st.Notes, ","), "family:linkerd") {
		t.Errorf("notes %v", st.Notes)
	}
}

func TestCorootProvider(t *testing.T) {
	mapJSON, _ := os.ReadFile("testdata/coroot_overview_map.json")
	appsJSON, _ := os.ReadFile("testdata/coroot_overview_applications.json")
	const project = "i1ip3tvq"
	logins := 0
	var panelQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["email"] != "viewer@pmtest.local" || body["password"] != "pw" {
				w.WriteHeader(404) // Coroot: "Invalid email or password."
				return
			}
			logins++
			http.SetCookie(w, &http.Cookie{Name: "coroot_session", Value: fmt.Sprintf("s%d", logins)})
			return
		}
		if r.URL.Path == "/api/v1/query_range" {
			if r.Header.Get("X-API-Key") != "projkey" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
			return
		}
		authed := r.Header.Get("Authorization") == "Bearer crt_viewer"
		if ck, err := r.Cookie("coroot_session"); err == nil && ck.Value == fmt.Sprintf("s%d", logins) {
			authed = true
		}
		if !authed {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/user":
			_, _ = w.Write([]byte(`{"id":2,"email":"viewer@pmtest.local","projects":[{"id":"i1ip3tvq","name":"default"}]}`))
		case "/api/project/" + project + "/overview/map":
			_, _ = w.Write(mapJSON)
		case "/api/project/" + project + "/overview/applications":
			_, _ = w.Write(appsJSON)
		case "/api/project/" + project + "/panel/data":
			var panel struct {
				Source struct {
					Metrics struct {
						Queries []struct{ Query, Legend string } `json:"queries"`
					} `json:"metrics"`
				} `json:"source"`
			}
			_ = json.Unmarshal([]byte(r.URL.Query().Get("query")), &panel)
			q := panel.Source.Metrics.Queries[0]
			panelQueries = append(panelQueries, q.Legend)
			// Series are labelled through the legend the querier asked for,
			// exactly like Coroot renders it.
			type row struct {
				m    map[string]string
				data string
			}
			var rows []row
			if strings.Contains(q.Query, "container_http_requests_total[") {
				rows = append(rows, []row{
					{map[string]string{"actual_destination": "10.244.0.30:8080", "container_id": "/k8s/shop/web-5d4f9c7b8-x2x9z/web", "destination": "10.96.0.10:8080", "proto": "http", "status": "2xx"}, "[4,4]"},
					{map[string]string{"actual_destination": "10.244.0.30:8080", "container_id": "/k8s/shop/web-5d4f9c7b8-x2x9z/web", "destination": "10.96.0.10:8080", "proto": "http", "status": "5xx"}, "[1,null]"},
				}...)
			}
			if strings.Contains(q.Query, "container_postgres_queries_total[") {
				rows = append(rows, row{map[string]string{"actual_destination": "10.244.0.22:5432", "container_id": "/k8s/shop/api-9959b4fb8-qr7d8/api", "destination": "10.96.158.141:5432", "proto": "postgres", "status": "ok"}, "[6]"})
			}
			if strings.Contains(q.Query, "kube_pod_info") {
				rows = append(rows, []row{
					{map[string]string{"created_by_kind": "ReplicaSet", "created_by_name": "api-9959b4fb8", "namespace": "shop", "pod": "api-9959b4fb8-qr7d8", "pod_ip": "10.244.0.30"}, "[1]"},
					{map[string]string{"created_by_kind": "StatefulSet", "created_by_name": "postgres", "namespace": "shop", "pod": "postgres-0", "pod_ip": "10.244.0.22"}, "[1]"},
				}...)
			}
			if strings.Contains(q.Query, "kube_replicaset_owner") {
				rows = append(rows, row{map[string]string{"namespace": "shop", "owner_kind": "Deployment", "owner_name": "api", "replicaset": "api-9959b4fb8"}, "[1]"})
			}
			var parts []string
			for _, rw := range rows {
				name := q.Legend
				for k, v := range rw.m {
					name = strings.ReplaceAll(name, "{{"+k+"}}", v)
				}
				name = regexp.MustCompile(`\{\{\w+\}\}`).ReplaceAllString(name, "")
				parts = append(parts, fmt.Sprintf(`{"name":%q,"data":%s}`, name, rw.data))
			}
			series := "[" + strings.Join(parts, ",") + "]"
			_, _ = w.Write([]byte(`{"chart":{"series":` + series + `}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cipher := testCipher(t)
	reg := NewRegistry(cipher)

	enc, _ := EncryptSecrets(cipher, Secrets{Token: "crt_viewer"})
	conn := models.ObservabilityConnection{ID: "c1", Provider: "coroot", URL: srv.URL, AuthKind: "bearer", SecretsEnc: enc}
	cl, err := reg.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cl.Provider.Test(context.Background(), cl)
	if err != nil || res.User != "viewer@pmtest.local" || len(res.Scopes) != 1 || res.Scopes[0].ID != project {
		t.Fatalf("test: %+v %v", res, err)
	}
	b := servicemap.NewBuilder()
	st := &servicemap.SourceStatus{OK: true}
	if err := cl.Provider.Map(context.Background(), cl, MapRequest{Range: 15 * time.Minute}, b, st); err != nil {
		t.Fatal(err)
	}
	g := b.Graph(nil)
	edges := edgeMap(g)
	for _, want := range []string{
		"shop/Deployment/api > shop/StatefulSet/postgres", // map + metrics
		"shop/Deployment/api > shop/Deployment/redis",     // map only
		"shop/Deployment/worker > shop/Deployment/rabbitmq",
		"shop/Deployment/web > shop/Deployment/api", // metrics only (web has no map links)
	} {
		if edges[want] == nil {
			t.Errorf("missing %s; have %v", want, names(edges))
		}
	}
	if e := edges["shop/Deployment/web > shop/Deployment/api"]; e != nil && (e.Stats.RPS != 5 || e.Stats.ErrorRate != 0.2) {
		t.Errorf("web→api RED %+v", e.Stats)
	}
	if e := edges["shop/Deployment/api > shop/StatefulSet/postgres"]; e != nil && (e.Stats.RPS != 6 || e.Protocol != "postgres") {
		t.Errorf("api→postgres %+v %s", e.Stats, e.Protocol)
	}
	pg := b.Node("shop/StatefulSet/postgres")
	if pg == nil || pg.Kind != servicemap.KindDatastore || pg.Subtype != "postgres" || pg.Coroot == nil || pg.Coroot.AppID != "i1ip3tvq:shop:StatefulSet:postgres" {
		t.Errorf("postgres node %+v", pg)
	}
	if n := b.Node("shop/Deployment/rabbitmq"); n == nil || n.Kind != servicemap.KindMessaging {
		t.Errorf("rabbit node %+v", n)
	}
	if n := b.Node("shop/Deployment/api"); n == nil || n.Replicas != "1/1" || n.Category != "application" {
		t.Errorf("api node %+v", n)
	}
	if n := b.Node("coroot/StatefulSet/coroot-coroot"); n == nil || n.Category != "monitoring" {
		t.Errorf("monitoring node %+v", n)
	}
	if len(panelQueries) == 0 || !strings.Contains(panelQueries[0], "{{container_id}}") {
		t.Errorf("panel legends %v", panelQueries)
	}

	// Email + password: signs in, then again when the session expires.
	enc, _ = EncryptSecrets(cipher, Secrets{Password: "pw"})
	lconn := models.ObservabilityConnection{ID: "c2", Provider: "coroot", URL: srv.URL, AuthKind: "login", Username: "viewer@pmtest.local", SecretsEnc: enc}
	lc, _ := reg.Client(lconn)
	if _, err := lc.Provider.Scopes(context.Background(), lc); err != nil {
		t.Fatalf("login: %v", err)
	}
	logins++ // server forgets the session
	lc2, _ := reg.Client(lconn)
	if _, err := lc2.Provider.Scopes(context.Background(), lc2); err != nil || logins != 3 {
		t.Fatalf("re-login: %v (logins %d)", err, logins)
	}
	enc, _ = EncryptSecrets(cipher, Secrets{Password: "nope"})
	bad, _ := reg.Client(models.ObservabilityConnection{Provider: "coroot", URL: srv.URL, AuthKind: "login", Username: "viewer@pmtest.local", SecretsEnc: enc})
	if _, err := bad.Provider.Test(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "email or password") {
		t.Errorf("bad password: %v", err)
	}
	// A project API key alone: metrics only, through /api/v1/query_range.
	enc, _ = EncryptSecrets(cipher, Secrets{APIKey: "projkey"})
	kc, _ := reg.Client(models.ObservabilityConnection{Provider: "coroot", URL: srv.URL, AuthKind: "apikey", SecretsEnc: enc})
	if res, err := kc.Provider.Test(context.Background(), kc); err != nil || res.Notes[0] != "metrics_only" {
		t.Errorf("project key: %+v %v", res, err)
	}
}

func TestPanelQueriesStayShort(t *testing.T) {
	var lens []int
	total := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lens = append(lens, len(r.URL.String()))
		var panel struct {
			Source struct {
				Metrics struct {
					Queries []struct{ Query string } `json:"queries"`
				} `json:"metrics"`
			} `json:"source"`
		}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("query")), &panel)
		total += strings.Count(panel.Source.Metrics.Queries[0].Query, "label_replace(")
		_, _ = w.Write([]byte(`{"chart":{"series":[{"name":"x","data":[1]}]}}`))
	}))
	defer srv.Close()
	cipher := testCipher(t)
	enc, _ := EncryptSecrets(cipher, Secrets{Token: "t"})
	cl, _ := NewRegistry(cipher).Client(models.ObservabilityConnection{Provider: "coroot", URL: srv.URL, AuthKind: "bearer", SecretsEnc: enc})
	q := panelQuerier{c: cl, project: "p"}
	expr := servicemap.MetricQueries("900s")["requests"]
	rows, err := q.Query(context.Background(), expr, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(lens) < 2 || total != strings.Count(expr, "label_replace(") || len(rows) != len(lens) {
		t.Errorf("chunks %d, clauses %d/%d, rows %d", len(lens), total, strings.Count(expr, "label_replace("), len(rows))
	}
	for _, l := range lens {
		if l > 7000 {
			t.Errorf("panel/data URL %d bytes — too long for an ingress", l)
		}
	}
	if got := splitTopLevelOr(`a or label_replace(x, "l", "a or b", "", "") or sum by (x) (y{z="or"})`); len(got) != 3 {
		t.Errorf("split %q", got)
	}
}

func TestQueryLabels(t *testing.T) {
	got := queryLabels(`label_replace(histogram_quantile(0.95, sum by (le, container_id, destination) (rate(x_bucket[5m]))), "proto", "http", "", "") or label_replace(sum by (container_id, method) (rate(y[5m])), "proto", "rabbitmq", "", "")`)
	if strings.Join(got, ",") != "container_id,destination,method,proto" {
		t.Errorf("labels %v", got)
	}
}

func TestTracingProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dependencies": // Jaeger
			_, _ = w.Write([]byte(`{"data":[{"parent":"frontend","child":"driver","callCount":900},{"parent":"frontend","child":"frontend","callCount":3}]}`))
		case "/api/v1/dependency_graph": // SigNoz
			if r.Header.Get("SIGNOZ-API-KEY") != "k" || r.Method != http.MethodPost {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`[{"parent":"frontend","child":"cart","callCount":60,"callRate":2,"errorRate":5,"p95":250000000}]`))
		case "/api/namespaces/graph": // Kiali
			if r.URL.Query().Get("namespaces") != "bookinfo" || r.URL.Query().Get("graphType") != "workload" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"elements":{"nodes":[
				{"data":{"id":"n0","nodeType":"workload","namespace":"bookinfo","workload":"productpage-v1"}},
				{"data":{"id":"n1","nodeType":"workload","namespace":"bookinfo","workload":"details-v1"}},
				{"data":{"id":"n2","nodeType":"service","namespace":"bookinfo","service":"api.github.com","isOutside":true}}],
				"edges":[
				{"data":{"source":"n0","target":"n1","responseTime":"18.5","traffic":{"protocol":"http","rates":{"http":"4.00","http5xx":"0.40","httpNoResponse":"0.20"}}}},
				{"data":{"source":"n0","target":"n2","traffic":{"protocol":"tcp","rates":{"tcp":"120.0"}}}}]}}`))
		case "/api/v1/service_dependencies": // Datadog
			if r.Header.Get("DD-API-KEY") != "a" || r.Header.Get("DD-APPLICATION-KEY") != "b" || r.URL.Query().Get("env") != "prod" {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte(`{"web-store":{"calls":["ad-server","payments-go"]},"ad-server":{"calls":["ad-server-redis"]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cipher := testCipher(t)
	reg := NewRegistry(cipher)
	run := func(conn models.ObservabilityConnection, req MapRequest) *servicemap.Graph {
		t.Helper()
		cl, err := reg.Client(conn)
		if err != nil {
			t.Fatal(err)
		}
		b := servicemap.NewBuilder()
		st := &servicemap.SourceStatus{OK: true}
		if err := cl.Provider.Map(context.Background(), cl, req, b, st); err != nil {
			t.Fatalf("%s: %v", conn.Provider, err)
		}
		return b.Graph(nil)
	}
	g := run(models.ObservabilityConnection{Provider: "jaeger", URL: srv.URL, AuthKind: "none"}, MapRequest{Range: 15 * time.Minute})
	e := edgeMap(g)["apm:frontend > apm:driver"]
	if e == nil || !e.Stats.RateOnly || e.Stats.RPS != 1 || len(g.Edges) != 1 {
		t.Errorf("jaeger %+v", g.Edges)
	}

	enc, _ := EncryptSecrets(cipher, Secrets{APIKey: "k"})
	g = run(models.ObservabilityConnection{Provider: "signoz", URL: srv.URL, AuthKind: "apikey", SecretsEnc: enc}, MapRequest{Range: 30 * time.Minute})
	e = edgeMap(g)["apm:frontend > apm:cart"]
	if e == nil || e.Stats.RPS != 2 || e.Stats.ErrorRate != 0.05 || e.Stats.P95Ms != 250 {
		t.Errorf("signoz %+v", e)
	}

	g = run(models.ObservabilityConnection{Provider: "kiali", URL: srv.URL, AuthKind: "none", Scope: "bookinfo"}, MapRequest{Range: 10 * time.Minute})
	edges := edgeMap(g)
	e = edges["apm:bookinfo/productpage-v1 > apm:bookinfo/details-v1"]
	if e == nil || e.Stats.RPS != 4 || e.Stats.ErrorRate < 0.149 || e.Stats.ErrorRate > 0.151 || e.Stats.P95Ms != 18.5 {
		t.Errorf("kiali http %+v", e)
	}
	if e := edges["apm:bookinfo/productpage-v1 > ext:api.github.com"]; e == nil || e.Protocol != "tcp" {
		t.Errorf("kiali tcp %v", names(edges))
	}

	enc, _ = EncryptSecrets(cipher, Secrets{APIKey: "a", AppKey: "b"})
	g = run(models.ObservabilityConnection{Provider: "datadog", URL: srv.URL, AuthKind: "datadog", SecretsEnc: enc, Scope: "prod"}, MapRequest{})
	if len(g.Edges) != 3 || edgeMap(g)["apm:ad-server > apm:ad-server-redis"] == nil {
		t.Errorf("datadog %v", names(edgeMap(g)))
	}
}
