package esquery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"patchman-be/internal/models"
)

// fakeOSD mimics OpenSearch Dashboards with the security plugin: the account
// route lists tenants, and saved objects differ per securitytenant header.
func fakeOSD(t *testing.T, seen *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("osd-xsrf") == "" {
			t.Errorf("missing osd-xsrf")
		}
		tenant := r.Header.Get("securitytenant")
		mu.Lock()
		*seen = append(*seen, r.URL.Path+"#"+tenant)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/configuration/account":
			_, _ = io.WriteString(w, `{"data":{"user_name":"devuser2","user_requested_tenant":"__user__","tenants":{"merchant":false,"global_tenant":true,"devuser2":true,"Analysts":true}}}`)
		case "/api/saved_objects/_find":
			title := map[string]string{"": "default-*", "global": "global-*", "private": "mine-*", "merchant": "merchant-logs-*"}[tenant]
			if title == "" {
				title = "unknown-*"
			}
			_, _ = io.WriteString(w, `{"total":1,"saved_objects":[{"id":"ip-`+tenant+`","attributes":{"title":"`+title+`","timeFieldName":"@timestamp"}}]}`)
		case "/api/saved_objects/index-pattern/ip-merchant":
			if tenant != "merchant" {
				w.WriteHeader(404)
				return
			}
			_, _ = io.WriteString(w, `{"id":"ip-merchant","attributes":{"title":"merchant-logs-*","timeFieldName":"@timestamp"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func osdConn(id, url, tenant string) models.ESConnection {
	opts := map[string]any{"kibanaUrl": url}
	if tenant != "" {
		opts["kibanaTenant"] = tenant
	}
	b, _ := json.Marshal(opts)
	return models.ESConnection{ID: id, AuthKind: "none", Nodes: "http://es.invalid:9200", OptionsJSON: string(b)}
}

func TestOSDTenantsList(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := fakeOSD(t, &seen, &mu)
	defer srv.Close()
	r := newTestRegistry()
	res, err := r.Tenants(context.Background(), osdConn("t1", srv.URL, "merchant"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Supported || res.Default != "merchant" || res.Current != TenantPrivate || res.User != "devuser2" {
		t.Fatalf("unexpected result: %+v", res)
	}
	var ids []string
	for _, tn := range res.Tenants {
		ids = append(ids, tn.ID+":"+tn.Kind)
	}
	if got := strings.Join(ids, ","); got != "global:global,private:private,Analysts:custom,merchant:custom" {
		t.Fatalf("tenant order/kinds: %s", got)
	}
	if res.Tenants[3].Writable {
		t.Fatalf("merchant is read-only")
	}
	if seen[0] != "/api/v1/configuration/account#" {
		t.Fatalf("account call must not carry a tenant header: %v", seen)
	}
}

func TestOSDDataViewsPerTenant(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := fakeOSD(t, &seen, &mu)
	defer srv.Close()
	r := newTestRegistry()
	conn := osdConn("t2", srv.URL, "")
	for tenant, want := range map[string]string{"": "default-*", "global": "global-*", "private": "mine-*", "merchant": "merchant-logs-*"} {
		list, err := r.DataViews(context.Background(), conn, KibanaScope{Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Pattern != want || list[0].Tenant != tenant {
			t.Fatalf("tenant %q: got %+v, want %s", tenant, list, want)
		}
	}
	// The detail call carries the tenant too (404 in any other tenant).
	dv, err := r.DataView(context.Background(), conn, KibanaScope{Tenant: "merchant"}, "ip-merchant")
	if err != nil || dv.Pattern != "merchant-logs-*" || dv.Tenant != "merchant" {
		t.Fatalf("detail in merchant: %+v %v", dv, err)
	}
	if _, err := r.DataView(context.Background(), conn, KibanaScope{Tenant: "global"}, "ip-merchant"); err == nil {
		t.Fatalf("detail in global must miss")
	}
	// Cache is split per tenant: repeating every tenant hits no server.
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	for _, tenant := range []string{"", "global", "private", "merchant"} {
		if _, err := r.DataViews(context.Background(), conn, KibanaScope{Tenant: tenant}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != n {
		t.Fatalf("expected cache hits, calls %d→%d", n, len(seen))
	}
}

// Kibana (no security plugin): account route 404 and the ES fallback 404 →
// unsupported, no error.
func TestOSDTenantsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	r := newTestRegistry()
	conn := osdConn("t3", srv.URL, "")
	conn.Nodes = srv.URL // the ES fallback hits the same 404 server
	res, err := r.Tenants(context.Background(), conn)
	if err != nil || res.Supported || res.Tenants == nil || len(res.Tenants) != 0 {
		t.Fatalf("want unsupported with [] tenants: %+v %v", res, err)
	}
}

// Direct OpenSearch fallback: OSD route 404 → _plugins/_security/api/account
// on the cluster (unwrapped body).
func TestOSDTenantsClusterFallback(t *testing.T) {
	osd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer osd.Close()
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_plugins/_security/api/account" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"user_name":"admin","user_requested_tenant":null,"tenants":{"global_tenant":true,"admin":true}}`)
	}))
	defer es.Close()
	r := newTestRegistry()
	conn := osdConn("t4", osd.URL, "")
	conn.Nodes = es.URL
	res, err := r.Tenants(context.Background(), conn)
	if err != nil || !res.Supported || len(res.Tenants) != 2 || res.Current != "" {
		t.Fatalf("cluster fallback: %+v %v", res, err)
	}
}

func TestSanitizeTenant(t *testing.T) {
	for in, want := range map[string]string{"": "", " Global ": "global", "global_tenant": "global", "__user__": "private", "Private": "private", "merchant team": "merchant team"} {
		if got, err := SanitizeTenant(in); err != nil || got != want {
			t.Fatalf("SanitizeTenant(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"a\nb", strings.Repeat("x", 101)} {
		if _, err := SanitizeTenant(bad); err == nil {
			t.Fatalf("SanitizeTenant(%q) should fail", bad)
		}
	}
}

// A tenant the user lacks is rejected instead of OSD silently falling back
// to another tenant (whose views would carry the wrong label).
func TestOSDUnknownTenantRejected(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := fakeOSD(t, &seen, &mu)
	defer srv.Close()
	r := newTestRegistry()
	conn := osdConn("t5", srv.URL, "")
	_, err := r.DataViews(context.Background(), conn, KibanaScope{Tenant: "merchnt"})
	if !errors.Is(err, ErrUnknownTenant) || !strings.Contains(err.Error(), "merchant") {
		t.Fatalf("want ErrUnknownTenant listing the real tenants, got %v", err)
	}
	if _, err := r.DataView(context.Background(), conn, KibanaScope{Tenant: "nope"}, "x"); !errors.Is(err, ErrUnknownTenant) {
		t.Fatalf("detail: want ErrUnknownTenant, got %v", err)
	}
}

// OSD with multi-tenancy switched off → unsupported; private off → dropped.
func TestOSDTenancyInfo(t *testing.T) {
	for _, tc := range []struct {
		info      string
		supported bool
		n         int
	}{
		{`{"multitenancy_enabled":false,"opensearch_dashboards_mt_enabled":true}`, false, 0},
		{`{"multitenancy_enabled":true,"opensearch_dashboards_mt_enabled":true,"private_tenant_enabled":false}`, true, 1},
		{`{"multitenancy_enabled":true,"opensearch_dashboards_mt_enabled":true,"private_tenant_enabled":true}`, true, 2},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/configuration/account":
				_, _ = io.WriteString(w, `{"data":{"user_name":"u","tenants":{"global_tenant":true,"u":true}}}`)
			case "/api/v1/multitenancy/info":
				_, _ = io.WriteString(w, tc.info)
			default:
				w.WriteHeader(404)
			}
		}))
		res, err := newTestRegistry().Tenants(context.Background(), osdConn("", srv.URL, ""))
		srv.Close()
		if err != nil || res.Supported != tc.supported || len(res.Tenants) != tc.n {
			t.Fatalf("%s: %+v %v", tc.info, res, err)
		}
	}
}
