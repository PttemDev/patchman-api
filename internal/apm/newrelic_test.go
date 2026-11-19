package apm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"
)

// fakeNerdGraph answers the NerdGraph requests the New Relic provider makes.
func fakeNerdGraph(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("API-Key") != "NRAK-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Invalid API key"}]}`)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		q := req.Query
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "user { email"):
			_, _ = io.WriteString(w, `{"data":{"actor":{"user":{"email":"a@example.test","name":"Ada"},"accounts":[{"id":111,"name":"Prod"},{"id":222,"name":"Test"}]}}}`)
		case strings.Contains(q, "entitySearch"):
			if !strings.Contains(req.Variables["q"].(string), "accountId = 111") && !strings.Contains(req.Variables["q"].(string), "LIKE") {
				t.Errorf("entity search without account: %v", req.Variables["q"])
			}
			_, _ = io.WriteString(w, `{"data":{"actor":{"entitySearch":{"results":{"nextCursor":null,"entities":[
				{"guid":"g1","name":"checkout","entityType":"APM_APPLICATION_ENTITY","domain":"APM","accountId":111,"reporting":true,"alertSeverity":"CRITICAL"},
				{"guid":"g2","name":"payments","entityType":"APM_APPLICATION_ENTITY","domain":"APM","accountId":111,"reporting":true,"alertSeverity":"NOT_ALERTING"}]}}}}}`)
		case strings.Contains(q, "relatedEntities"):
			_, _ = io.WriteString(w, `{"data":{"actor":{"entities":[{"guid":"g1","relatedEntities":{"results":[
				{"type":"CALLS","source":{"entity":{"guid":"g1","name":"checkout"}},"target":{"entity":{"guid":"g2","name":"payments"}}},
				{"type":"HOSTS","source":{"entity":{"guid":"h1","name":"host-1"}},"target":{"entity":{"guid":"g1","name":"checkout"}}},
				{"type":"CALLS","source":{"entity":{"guid":"g1","name":"checkout"}},"target":{"entity":{"guid":"g9","name":"stripe-api"}}}]}}]}}}`)
		case strings.Contains(q, "nrql("):
			nrql := req.Variables["q"].(string)
			if req.Variables["id"].(float64) != 111 {
				t.Errorf("account = %v", req.Variables["id"])
			}
			switch {
			case strings.Contains(nrql, "FROM Transaction FACET appName"):
				_, _ = io.WriteString(w, `{"data":{"actor":{"account":{"nrql":{"results":[{"appName":"checkout","facet":"checkout","rps":2.5,"errPct":4,"p95":{"95":0.25}}]}}}}}`)
			case strings.Contains(nrql, "NrAiIncident"):
				_, _ = io.WriteString(w, `{"data":{"actor":{"account":{"nrql":{"results":[{"incidentId":"1","state":"close","title":"old"},{"incidentId":"2","state":"open","title":"High error rate"}]}}}}}`)
			case strings.Contains(nrql, "broken"):
				_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"NRQL Syntax Error"}]}`)
			default:
				_, _ = io.WriteString(w, `{"data":{"actor":{"account":{"nrql":{"results":[{"count":42}]}}}}}`)
			}
		default:
			t.Errorf("unexpected query: %s", q)
			http.Error(w, "bad", http.StatusBadRequest)
		}
	}))
}

func TestNewRelicProvider(t *testing.T) {
	srv := fakeNerdGraph(t)
	defer srv.Close()
	cipher := testCipher(t)
	reg := NewRegistry(cipher)
	enc, _ := EncryptSecrets(cipher, Secrets{APIKey: "NRAK-test"})
	conn := models.ObservabilityConnection{Provider: "newrelic", URL: srv.URL, AuthKind: "apikey", SecretsEnc: enc, Scope: "111"}
	cl, err := reg.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Test lists the user's accounts as scopes.
	res, err := cl.Provider.Test(ctx, cl)
	if err != nil || !res.OK || res.User != "Ada" || len(res.Scopes) != 2 || res.Scopes[0].ID != "111" {
		t.Fatalf("test = %+v %v", res, err)
	}

	// The map: two apps, health from alert severity, RED from NRQL, CALLS
	// edges only (HOSTS dropped), an external callee added.
	b := servicemap.NewBuilder()
	st := &servicemap.SourceStatus{OK: true}
	if err := cl.Provider.Map(ctx, cl, MapRequest{Range: 15 * time.Minute}, b, st); err != nil {
		t.Fatal(err)
	}
	g := b.Graph(nil)
	nodes := map[string]*servicemap.Node{}
	for _, n := range g.Nodes {
		nodes[n.Name] = n
	}
	if n := nodes["checkout"]; n == nil || n.Health != "critical" || n.Stats.RPS != 2.5 || n.Stats.ErrorRate != 0.04 || n.Stats.P95Ms != 250 {
		t.Errorf("checkout = %+v", n)
	}
	if n := nodes["payments"]; n == nil || n.Health != "ok" {
		t.Errorf("payments = %+v", n)
	}
	edges := edgeMap(g)
	if edges["apm:checkout > apm:payments"] == nil || edges["apm:checkout > apm:stripe-api"] == nil || len(g.Edges) != 2 {
		t.Errorf("edges = %v", names(edges))
	}
	if nodes["host-1"] != nil {
		t.Error("a HOSTS relationship must not become a node")
	}

	// NRQL, alerts (open first / open only) and a query error.
	rows, err := NewRelicNRQL(ctx, cl, 111, "SELECT count(*) FROM Transaction SINCE 1 hour ago")
	if err != nil || len(rows) != 1 || rows[0]["count"].(float64) != 42 {
		t.Fatalf("nrql = %v %v", rows, err)
	}
	if _, err := NewRelicNRQL(ctx, cl, 111, "broken"); err == nil || !strings.Contains(err.Error(), "NRQL Syntax Error") {
		t.Errorf("nrql error = %v", err)
	}
	all, err := NewRelicAlerts(ctx, cl, 111, 24*time.Hour, false)
	if err != nil || len(all) != 2 || all[0]["state"] != "open" {
		t.Fatalf("alerts = %v %v", all, err)
	}
	open, _ := NewRelicAlerts(ctx, cl, 111, 24*time.Hour, true)
	if len(open) != 1 || open[0]["title"] != "High error rate" {
		t.Errorf("open alerts = %v", open)
	}
	// A plain word is a name search.
	if _, err := NewRelicEntities(ctx, cl, "checkout", 0); err != nil {
		t.Errorf("entities: %v", err)
	}
	// Account resolution.
	if a, err := NewRelicAccount(cl, ""); err != nil || a != 111 {
		t.Errorf("account = %v %v", a, err)
	}
	if _, err := NewRelicAccount(cl, "abc"); err == nil {
		t.Error("non-numeric account must be refused")
	}

	// A wrong key is an auth error.
	bad, _ := EncryptSecrets(cipher, Secrets{APIKey: "nope"})
	conn.SecretsEnc = bad
	bc, _ := reg.Client(conn)
	if _, err := bc.Provider.Test(ctx, bc); err == nil {
		t.Error("a wrong key must fail")
	}
	// The full …/graphql URL works too.
	conn.SecretsEnc = enc
	conn.URL = srv.URL + "/graphql"
	gc, _ := reg.Client(conn)
	if _, err := gc.Provider.Test(ctx, gc); err != nil {
		t.Errorf("…/graphql URL: %v", err)
	}
}
