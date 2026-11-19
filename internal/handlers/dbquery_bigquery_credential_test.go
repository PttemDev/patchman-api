package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/gcp"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// A BigQuery connection signing in with a shared Google Cloud credential:
// only the owner's credential is accepted (create, test, update, and again
// at connect time for a row that bypassed the handlers); a linked Google
// account without the BigQuery scope is refused with a "reconnect" hint.
func TestBigQueryCredentialConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/datasets"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			_, _ = w.Write([]byte(`{"statistics":{"totalBytesProcessed":"1024","query":{"statementType":"SELECT"}}}`))
		case strings.HasSuffix(r.URL.Path, "/queries"):
			_, _ = w.Write([]byte(`{"jobComplete":true,"jobReference":{"jobId":"j"},"schema":{"fields":[{"name":"n","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	defer dbquery.UseBigQueryEndpoint(srv.URL, srv.Client())()
	gcp.UseEndpoint(srv.URL) // static fake token, never Google
	t.Cleanup(func() { gcp.UseEndpoint("") })

	h, db := connCopyTestHandler(t)
	if err := db.AutoMigrate(&models.GCPCredential{}, &models.UserSetting{}, &models.DBQueryHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("DELETE FROM gcp_credentials")
	db.Exec("DELETE FROM user_settings")
	gcpH := NewGCPHandler(db, nil, nil)
	old := dbquery.GCPTokenSource
	dbquery.GCPTokenSource = gcpH.BigQueryTokenSource
	t.Cleanup(func() { dbquery.GCPTokenSource = old })

	db.Create(&models.GCPCredential{ID: "c-bq", UserID: "u1", Name: "me", Kind: "oauth", Account: "me@example.com",
		Scopes: "openid email " + gcp.ScopeReadOnly + " " + gcp.ScopeBigQuery})
	db.Create(&models.GCPCredential{ID: "c-gke", UserID: "u1", Name: "gke", Kind: "oauth", Account: "gke@example.com",
		Scopes: "openid email " + gcp.ScopeCloud})
	db.Create(&models.GCPCredential{ID: "c-nobq", UserID: "u1", Name: "storage", Kind: "oauth", Account: "st@example.com",
		Scopes: "openid email " + gcp.ScopeReadOnly + " " + gcp.ScopeStorageRead})
	db.Create(&models.GCPCredential{ID: "c-u2", UserID: "u2", Name: "theirs", Kind: "oauth", Account: "u2@example.com",
		Scopes: gcp.ScopeBigQuery})

	body := func(cred string) map[string]any {
		return map[string]any{"name": "bq-" + cred, "kind": "bigquery", "database": "proj",
			"options": map[string]any{"auth": "credential", "gcpCredentialId": cred}}
	}
	// Create: someone else's credential / a missing one / one without BigQuery.
	if code, _, out := createConn(t, h, "u1", body("c-u2")); code != 400 || !strings.Contains(out, "owner") {
		t.Fatalf("foreign credential: %d %s", code, out)
	}
	if code, _, out := createConn(t, h, "u1", body("")); code != 400 || !strings.Contains(out, "pick the Google Cloud credential") {
		t.Fatalf("missing credential: %d %s", code, out)
	}
	if code, _, out := createConn(t, h, "u1", body("c-nobq")); code != 400 || !strings.Contains(out, "BigQuery ticked") {
		t.Fatalf("no bigquery scope: %d %s", code, out)
	}
	if code, conn, out := createConn(t, h, "u1", body("c-gke")); code != 201 && code != 200 || conn.Username != "gke@example.com" {
		t.Fatalf("cloud-platform scope covers BigQuery: %d %s", code, out)
	}
	code, conn, out := createConn(t, h, "u1", body("c-bq"))
	if code != 201 && code != 200 || conn.Username != "me@example.com" {
		t.Fatalf("create: %d %s", code, out)
	}
	if h, _ := conn.Options.(map[string]any); h["gcpCredentialId"] != "c-bq" {
		t.Fatalf("options %v", conn.Options)
	}

	call := func(fn gin.HandlerFunc, user, method, id string, in any) (int, map[string]any) {
		b, _ := json.Marshal(in)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("userID", user)
		c.Request = httptest.NewRequest(method, "/x", bytes.NewReader(b))
		c.Request.Header.Set("Content-Type", "application/json")
		if id != "" {
			c.Params = gin.Params{{Key: "id", Value: id}}
		}
		fn(c)
		var o map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &o)
		return w.Code, o
	}
	// Test before save: dials with the caller's own credential only.
	if code, o := call(h.Test, "u1", "POST", "", body("c-bq")); code != 200 || o["data"].(map[string]any)["ok"] != true {
		t.Fatalf("test own credential: %d %v", code, o)
	}
	if _, o := call(h.Test, "u1", "POST", "", body("c-u2")); o["data"].(map[string]any)["ok"] != false {
		t.Fatalf("test foreign credential: %v", o)
	}
	// Update: pointing the row at a credential that isn't the OWNER's.
	if code, o := call(h.Update, "u1", "PUT", conn.ID, map[string]any{"options": map[string]any{"auth": "credential", "gcpCredentialId": "c-u2"}}); code != 400 {
		t.Fatalf("update to a foreign credential: %d %v", code, o)
	}

	exec := NewDBQueryExecuteHandler(db, h.registry, h)
	run := func(id string) (int, map[string]any) {
		return call(exec.Execute, "u1", "POST", "", map[string]any{"connectionId": id, "query": "SELECT n FROM sales.orders"})
	}
	if code, o := run(conn.ID); code != 200 {
		t.Fatalf("execute with own credential: %d %v", code, o)
	}
	// Shared with u2: the query runs — signed in as the OWNER's (u1's)
	// credential, u2 has none of their own.
	db.Create(&models.DBConnectionGrant{ID: "g-bq", ConnectionID: conn.ID, SubjectKind: "user", SubjectID: "u2", Role: "read", GrantedBy: "u1"})
	if code, o := call(exec.Execute, "u2", "POST", "", map[string]any{"connectionId": conn.ID, "query": "SELECT n FROM sales.orders"}); code != 200 {
		t.Fatalf("grantee runs as the owner: %d %v", code, o)
	}
	// A row written around the handlers (old BE, direct DB edit) that points
	// at another user's credential is still refused when it connects.
	emptyEnc, _ := h.cipher.Encrypt("")
	db.Create(&models.DBConnection{ID: "bq-forged", UserID: "u1", Name: "forged", Kind: "bigquery", Host: "bigquery.googleapis.com", Port: 443,
		Database: "proj", PasswordEnc: emptyEnc, OptionsJSON: `{"auth":"credential","gcpCredentialId":"c-u2"}`})
	hits.Store(0)
	if code, o := run("bq-forged"); code == 200 || !strings.Contains(o["error"].(string), "owner") {
		t.Fatalf("forged row: %d %v", code, o)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a foreign credential reached BigQuery %d times", n)
	}

	// The credential's delete guard names the BigQuery connection.
	if used := gcpH.usedBy(models.GCPCredential{ID: "c-bq", UserID: "u1"}); len(used) != 1 || used[0] != "BigQuery: bq-c-bq" {
		t.Fatalf("usedBy %v", used)
	}
	// DTO tells the FE which credentials can run BigQuery.
	var nobq models.GCPCredential
	db.First(&nobq, "id = ?", "c-nobq")
	if gcpDTO(nobq)["bigQuery"] != false {
		t.Error("an account without the BigQuery scope must say so")
	}
}
