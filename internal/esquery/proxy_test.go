package esquery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Kibana with server.restrictInternalApis answers the Console proxy with a
// 400 unless the request carries x-elastic-internal-origin (as Kibana's own
// Dev Tools does). The proxy must send it, plus both xsrf headers.
func TestKibanaProxyHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.Header.Get("x-elastic-internal-origin") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"statusCode":400,"error":"Bad Request","message":"uri [/api/console/proxy] with method [post] exists but is not available with the current configuration"}`))
			return
		}
		w.Header().Set("x-console-proxy-status-code", "200")
		_, _ = w.Write([]byte(`{"status":"green"}`))
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/console/proxy?method=GET&path=_cluster%2Fhealth", nil)
	setKibanaHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (internal-origin header missing?)", resp.StatusCode)
	}
	for _, h := range []string{"kbn-xsrf", "osd-xsrf", "x-elastic-internal-origin"} {
		if got.Get(h) == "" {
			t.Errorf("header %s not sent", h)
		}
	}
}

func TestKibanaProxyErrorMessages(t *testing.T) {
	restricted := kibanaProxyError(400, []byte(`{"message":"uri [/api/console/proxy] with method [post] exists but is not available with the current configuration"}`))
	if !strings.Contains(restricted.Error(), "restrictInternalApis") {
		t.Errorf("restricted-API 400 = %q", restricted)
	}
	other := kibanaProxyError(400, []byte(`{"message":"[request query.path]: expected value of type [string]"}`))
	if strings.Contains(other.Error(), "kbn-xsrf") || !strings.Contains(other.Error(), "expected value of type") {
		t.Errorf("generic 400 should show Kibana's message, got %q", other)
	}
}
