package apm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"
)

func TestNRQLString(t *testing.T) {
	if got := nrqlString(`it's a \ test`); got != `'it\'s a \\ test'` {
		t.Fatalf("nrqlString = %s", got)
	}
}

func TestNewRelicServiceDetail(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		q, _ := req.Variables["q"].(string)
		mu.Lock()
		seen = append(seen, q)
		mu.Unlock()
		res := `[]`
		switch {
		case strings.Contains(q, "NrAiIncident"):
			res = `[{"incidentId":"7","state":"open","title":"Error rate > 5%","entity":"o'brien"}]`
		case strings.Contains(q, "TransactionError"):
			res = `[{"facet":"java.io.IOException","error.class":"java.io.IOException","count":12,"message":"reset by peer","lastSeen":1700000000000},{"facet":"Timeout","count":30}]`
		case strings.Contains(q, "FACET name"):
			res = `[{"facet":"WebTransaction/GET /a","name":"WebTransaction/GET /a","rps":1,"avg":0.1,"p95":{"95":0.3},"errPct":0,"total":30},{"name":"WebTransaction/POST /b","rps":0.5,"avg":0.5,"p95":0.9,"errPct":10,"total":90}]`
		case strings.Contains(q, "FACET host"):
			res = `[{"facet":"host-1","host":"host-1","rps":1.5,"avg":0.2,"errPct":2}]`
		case strings.Contains(q, "TIMESERIES"):
			res = `[{"beginTimeSeconds":1700000000,"endTimeSeconds":1700000060,"rps":2,"errPct":5,"avg":0.1,"p95":{"95":0.2},"p99":{"99":0.4},"apS":8,"apT":2,"apN":10},
				{"beginTimeSeconds":1700000060,"endTimeSeconds":1700000120,"rps":null,"errPct":null,"avg":null,"p95":null,"p99":null,"apS":0,"apT":0,"apN":0}]`
		case strings.Contains(q, "FROM Transaction"):
			res = `[{"rps":1.5,"errPct":2,"avg":0.12,"p95":{"95":0.25},"p99":0.5,"apS":90,"apT":10,"apN":100}]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"actor":{"account":{"nrql":{"results":`+res+`}}}}}`)
	}))
	defer srv.Close()
	cipher := testCipher(t)
	enc, _ := EncryptSecrets(cipher, Secrets{APIKey: "NRAK-test"})
	cl, err := NewRegistry(cipher).Client(models.ObservabilityConnection{Provider: "newrelic", URL: srv.URL, AuthKind: "apikey", SecretsEnc: enc, Scope: "111"})
	if err != nil {
		t.Fatal(err)
	}
	det := cl.Provider.(ServiceDetailer)
	d, err := det.ServiceDetail(context.Background(), cl, DetailRequest{Ref: servicemap.SourceRef{Name: "o'brien"}, Range: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PanelErrors) > 0 {
		t.Fatalf("panel errors: %v", d.PanelErrors)
	}
	for _, q := range seen {
		if !strings.Contains(q, `'o\'brien'`) {
			t.Errorf("app name not quoted: %s", q)
		}
	}
	s := d.Summary
	if s == nil || *s.RPS != 1.5 || *s.ErrorRate != 0.02 || *s.AvgMs != 120 || *s.P95Ms != 250 || *s.P99Ms != 500 || *s.Apdex != 0.95 {
		t.Fatalf("summary = %+v", s)
	}
	if len(d.Charts) != 4 || d.Charts[0].ID != "throughput" || d.Charts[1].ID != "latency" || d.Charts[3].ID != "apdex" {
		t.Fatalf("charts = %+v", d.Charts)
	}
	lat := d.Charts[1]
	if len(lat.Series) != 3 || len(lat.Series[1].Points) != 1 || lat.Series[1].Points[0] != [2]float64{1700000000000, 200} {
		t.Fatalf("latency (nulls are gaps) = %+v", lat.Series)
	}
	if d.Charts[3].Series[0].Points[0][1] != 0.9 {
		t.Fatalf("apdex = %+v", d.Charts[3])
	}
	if len(d.Endpoints) != 2 || d.Endpoints[0].Name != "WebTransaction/POST /b" || *d.Endpoints[0].TimeShare != 0.75 || *d.Endpoints[0].ErrorRate != 0.1 {
		t.Fatalf("endpoints (by time share) = %+v", d.Endpoints)
	}
	if len(d.Errors) != 2 || d.Errors[0].Class != "Timeout" || d.Errors[1].Message != "reset by peer" {
		t.Fatalf("errors = %+v", d.Errors)
	}
	if len(d.Instances) != 1 || d.Instances[0].Name != "host-1" || len(d.Alerts) != 1 {
		t.Fatalf("instances / alerts = %+v %+v", d.Instances, d.Alerts)
	}
}

func TestCorootServiceDetail(t *testing.T) {
	raw := []byte(`{"context":{},"data":{"app_map":{},"reports":[
		{"name":"SLO","status":"warning","checks":[{"id":"SLOLatency","title":"Latency","status":"warning","message":"p99 above 500ms"}],
		 "widgets":[{"chart":{"ctx":{"from":1700000000000,"to":1700000120000,"step":60000},"title":"Errors, per second","series":[{"name":"500","data":[1,null,3]}]}},
		            {"table":{"header":["Instance","Status"],"rows":[{"cells":[{"value":"api-1"},{"value":"up"}]}]}}]},
		{"name":"CPU","status":"ok","checks":[],"widgets":[{"chart_group":{"title":"CPU usage","charts":[
			{"ctx":{"from":1700000000000,"step":60000},"title":"overview","series":[{"name":"api-1","data":[0.2,0.3]}]},
			{"ctx":{"from":1700000000000,"step":60000},"title":"api-1","series":[{"name":"x","data":[1]}]}]}}]},
		{"name":"Profiling","widgets":[{"chart":{"ctx":{"from":1,"step":1},"title":"skip","series":[{"name":"a","data":[1]}]}}]}]}}`)
	v, err := decodeCorootApp(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := corootDetail(v, "api", DetailRequest{Range: time.Hour})
	if len(d.Charts) != 2 {
		t.Fatalf("charts = %+v", d.Charts)
	}
	errs := d.Charts[0]
	if errs.Group != "SLO" || len(errs.Series[0].Points) != 2 || errs.Series[0].Points[1] != [2]float64{1700000120000, 3} {
		t.Fatalf("errors chart (positioned by step, nulls skipped) = %+v", errs)
	}
	if d.Charts[1].Title != "CPU usage" || len(d.Charts[1].Series) != 1 {
		t.Fatalf("chart group → its overview chart = %+v", d.Charts[1])
	}
	if len(d.Checks) != 1 || d.Checks[0].Status != "warning" || len(d.Tables) != 1 || d.Tables[0].Rows[0][0] != "api-1" {
		t.Fatalf("checks / tables = %+v %+v", d.Checks, d.Tables)
	}
}

// detailProm answers instant and range queries by matching expression parts.
type detailProm struct {
	mu   sync.Mutex
	seen []string
}

func (f *detailProm) Query(_ context.Context, q string, _ time.Time) ([]servicemap.Sample, error) {
	f.mu.Lock()
	f.seen = append(f.seen, q)
	f.mu.Unlock()
	switch {
	case strings.Contains(q, "count by (__name__)"):
		return []servicemap.Sample{{Metric: map[string]string{"__name__": "istio_requests_total"}, Value: 3}}, nil
	case strings.Contains(q, "histogram_quantile(0.95"):
		return []servicemap.Sample{{Value: 120}}, nil
	case strings.Contains(q, `response_code=~"5..|0"`):
		return []servicemap.Sample{{Value: 0.1}}, nil
	case strings.Contains(q, "istio_requests_total"):
		return []servicemap.Sample{{Value: 4}}, nil
	}
	return nil, nil
}

func (f *detailProm) QueryRange(_ context.Context, q string, _, _ time.Time, _ time.Duration) ([]servicemap.RangeSample, error) {
	f.mu.Lock()
	f.seen = append(f.seen, q)
	f.mu.Unlock()
	if strings.Contains(q, "istio") {
		return []servicemap.RangeSample{{Points: [][2]float64{{1700000000000, 2}, {1700000060000, 3}}}}, nil
	}
	return nil, nil
}

func TestPromServiceDetail(t *testing.T) {
	f := &detailProm{}
	d, err := PromServiceDetail(context.Background(), f, f, DetailRequest{Ref: servicemap.SourceRef{Name: `api"x`, Namespace: "shop"}, Range: time.Hour}, "prometheus")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Charts) != 3 || d.Charts[0].ID != "throughput" || len(d.Charts[0].Series[0].Points) != 2 {
		t.Fatalf("charts = %+v", d.Charts)
	}
	if d.Summary == nil || *d.Summary.RPS != 4 || *d.Summary.P95Ms != 120 {
		t.Fatalf("summary = %+v", d.Summary)
	}
	found := false
	for _, q := range f.seen {
		if strings.Contains(q, `destination_workload="api\"x"`) && strings.Contains(q, `destination_workload_namespace="shop"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("service selector not escaped / scoped: %v", f.seen)
	}
	if !strings.Contains(strings.Join(d.Notes, ","), "family:istio") {
		t.Fatalf("notes = %v", d.Notes)
	}
}

func TestBuilderRefs(t *testing.T) {
	b := servicemap.NewBuilder()
	b.SetOrigin("conn-1", "111")
	id := b.ServiceNode("", "checkout", "newrelic")
	b.AddRef(id, "newrelic", "checkout", "", "guid-1")
	b.ServiceNode("", "checkout", "newrelic") // repeat: no duplicate
	b.SetOrigin("", "")
	b.ServiceNode("", "checkout", "prometheus") // no origin: no ref
	n := b.Node(id)
	if len(n.Refs) != 1 || n.Refs[0].Conn != "conn-1" || n.Refs[0].Key != "guid-1" || n.Refs[0].Scope != "111" {
		t.Fatalf("refs = %+v", n.Refs)
	}
}

func TestPromSelQuotesValues(t *testing.T) {
	got := sel("server", `=~".*"`, "ns", "")
	if got != `server="=~\".*\""` {
		t.Fatalf("sel = %s", got)
	}
}

func TestDetailNormalizeNoNulls(t *testing.T) {
	d := &ServiceDetail{Charts: []DetailChart{{ID: "errors", Series: []DetailSeries{{Name: "errors"}}}, {ID: "other"}}}
	d.Normalize()
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "null") {
		t.Fatalf("null in %s", raw)
	}
}
