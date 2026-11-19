package dbquery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

// auth "credential": the real client builder (not the test stub) signs every
// call with the token of the connection OWNER's shared Google credential; a
// connection pointing at anyone else's credential never reaches Google.
func TestBigQueryCredentialAuth(t *testing.T) {
	f := &fakeBigQuery{bytes: 1 << 20}
	var hits atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		f.handler(t).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldTS := bigQueryBase, GCPTokenSource
	bigQueryBase = srv.URL
	t.Cleanup(func() { bigQueryBase, GCPTokenSource = oldBase, oldTS })
	GCPTokenSource = func(owner, cred string) (oauth2.TokenSource, error) {
		if owner == "u1" && cred == "c1" {
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.bq"}), nil
		}
		return nil, errors.New("not one of the connection owner's credentials")
	}
	cfg := func(owner, cred string) ConnectionConfig {
		return ConnectionConfig{Kind: KindBigQuery, Database: "proj", OwnerID: owner,
			Options: map[string]any{"auth": "credential", "gcpCredentialId": cred}}
	}
	if got := BigQueryAuth(cfg("u1", "c1")); got != "credential" {
		t.Fatalf("auth mode %q", got)
	}

	s, err := NewBigQueryDriver().Connect(context.Background(), cfg("u1", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := auth.Load().(string); a != "Bearer ya29.bq" {
		t.Fatalf("Authorization %q", a)
	}
	cur, err := s.Execute(context.Background(), "SELECT * FROM sales.orders", ExecOpts{MaxBytes: 10 << 30, AskBytes: 1 << 30})
	if err != nil || len(bqDrain(t, cur)) != 2 {
		t.Fatalf("query: %v", err)
	}
	// Cost guard unchanged: the dry run still decides.
	if _, err := s.Execute(context.Background(), "SELECT 1", ExecOpts{MaxBytes: 1 << 10}); !errors.As(err, new(*CostLimitError)) {
		t.Fatalf("cost limit with a shared credential: %v", err)
	}

	hits.Store(0)
	for name, c := range map[string]ConnectionConfig{
		"foreign owner": cfg("u2", "c1"),
		"other cred":    cfg("u1", "c2"),
		"no cred":       cfg("u1", ""),
	} {
		if _, err := NewBigQueryDriver().Connect(context.Background(), c); err == nil {
			t.Errorf("%s: connect must fail", name)
		}
	}
	// Even with the HTTP client swapped (BIGQUERY_API_BASE / emulator) the
	// owner check still runs.
	restore := UseBigQueryEndpoint(srv.URL, srv.Client())
	if _, err := NewBigQueryDriver().Connect(context.Background(), cfg("u2", "c1")); err == nil {
		t.Error("foreign credential through an emulator client must fail")
	}
	restore()
	if n := hits.Load(); n != 0 {
		t.Fatalf("a refused credential reached BigQuery %d times", n)
	}

	GCPTokenSource = nil
	if _, err := NewBigQueryDriver().Connect(context.Background(), cfg("u1", "c1")); err == nil || !strings.Contains(err.Error(), "aren't available") {
		t.Fatalf("no Google Cloud on the server: %v", err)
	}
}
