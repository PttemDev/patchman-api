package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

const fakeSAKey = `{"type":"service_account","project_id":"proj","client_email":"bq-reader@proj.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"}`

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"10GB": 10 << 30, "500 mb": 500 << 20, "1TB": 1 << 40, "2048": 2048, "": 0, "abc": 0, "1.5GB": 3 << 29} {
		if got := ParseByteSize(in); got != want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNormalizeBigQueryInput(t *testing.T) {
	SetEmbeddedMode(false)
	in := createConnectionInput{Kind: "bigquery", Database: "proj", Password: fakeSAKey}
	if err := normalizeBigQueryInput(&in, true); err != nil || in.Username != "bq-reader@proj.iam.gserviceaccount.com" || in.Host == "" || in.Port != 443 {
		t.Fatalf("service account: %v %+v", err, in)
	}
	bad := createConnectionInput{Kind: "bigquery", Database: "proj", Password: "not json"}
	if err := normalizeBigQueryInput(&bad, true); err == nil {
		t.Error("a non-JSON key must be refused")
	}
	adc := createConnectionInput{Kind: "bigquery", Database: "proj", Options: map[string]any{"auth": "adc"}}
	if err := normalizeBigQueryInput(&adc, false); err == nil || !strings.Contains(err.Error(), "desktop") {
		t.Errorf("gcloud credentials must be refused on a server: %v", err)
	}
	SetEmbeddedMode(true)
	t.Cleanup(func() { SetEmbeddedMode(false) })
	adc = createConnectionInput{Kind: "bigquery", Database: "proj", Options: map[string]any{"auth": "adc"}}
	if err := normalizeBigQueryInput(&adc, false); err != nil || adc.Username != "gcloud" {
		t.Errorf("gcloud credentials on the desktop: %v", err)
	}
	noProject := createConnectionInput{Kind: "bigquery", Password: fakeSAKey}
	if err := normalizeBigQueryInput(&noProject, true); err == nil {
		t.Error("project id is required")
	}
}

// End to end through the execute handler: the scan limits come from the
// user's settings, the connection and the admin ceiling; over the ask
// threshold answers 409 costConfirm, over the limit 400 costLimit.
func TestBigQueryExecuteCostGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var ran int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/datasets"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			_, _ = w.Write([]byte(`{"statistics":{"totalBytesProcessed":"5368709120","query":{"statementType":"SELECT"}}}`)) // 5 GB
		case strings.HasSuffix(r.URL.Path, "/queries"):
			ran++
			_, _ = w.Write([]byte(`{"jobComplete":true,"jobReference":{"jobId":"j"},"schema":{"fields":[{"name":"n","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	defer dbquery.UseBigQueryEndpoint(srv.URL, srv.Client())()

	h, db := connCopyTestHandler(t)
	if err := db.AutoMigrate(&models.UserSetting{}, &models.DBQueryHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("DELETE FROM user_settings")
	code, conn, body := createConn(t, h, "u1", map[string]any{"name": "bq", "kind": "bigquery", "database": "proj", "password": fakeSAKey})
	if code != 200 && code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	exec := NewDBQueryExecuteHandler(db, h.registry, h)
	run := func(extra map[string]any) (int, map[string]any) {
		in := map[string]any{"connectionId": conn.ID, "query": "SELECT n FROM sales.orders"}
		for k, v := range extra {
			in[k] = v
		}
		b, _ := json.Marshal(in)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("userID", "u1")
		c.Request = httptest.NewRequest("POST", "/api/db-query/execute", bytes.NewReader(b))
		c.Request.Header.Set("Content-Type", "application/json")
		exec.Execute(c)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// Defaults: limit 10 GB, ask above 1 GB → 5 GB needs a yes.
	if code, out := run(nil); code != http.StatusConflict || out["costConfirm"] != true || ran != 0 {
		t.Fatalf("default: want 409 costConfirm, got %d %v", code, out)
	}
	if code, _ := run(map[string]any{"confirmCost": true}); code != 200 || ran != 1 {
		t.Fatalf("confirmed: %d", code)
	}
	// The user's own limit (Settings → BigQuery): 2 GB → refused.
	db.Save(&models.UserSetting{UserID: "u1", Key: bigQuerySettingsKey, Value: `{"maxBytes":2147483648}`})
	if code, out := run(map[string]any{"confirmCost": true}); code != 400 || out["costLimit"] != true {
		t.Fatalf("user limit: want 400 costLimit, got %d %v", code, out)
	}
	// The connection's own limit overrides the user's.
	db.Model(&models.DBConnection{}).Where("id = ?", conn.ID).Update("options_json", `{"auth":"service_account","maxBytesBilled":21474836480,"askBytes":10737418240}`)
	if code, _ := run(nil); code != 200 {
		t.Fatalf("connection limit 20 GB / ask 10 GB should run 5 GB directly: %d", code)
	}
	// The admin ceiling caps everything.
	SetBigQueryCeiling(1 << 30)
	t.Cleanup(func() { SetBigQueryCeiling(0) })
	if code, out := run(map[string]any{"confirmCost": true}); code != 400 || out["costLimit"] != true {
		t.Fatalf("ceiling: want 400, got %d %v", code, out)
	}
}
