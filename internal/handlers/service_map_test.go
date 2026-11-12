package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"patchman-be/internal/apm"
	"patchman-be/internal/database"
	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

func TestServiceMapEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "sm.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")

	// A Jaeger answering frontend → orders-db, and a saved DB connection
	// whose host is that database.
	jaeger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/dependencies" {
			_, _ = w.Write([]byte(`{"data":[{"parent":"frontend","child":"orders-db","callCount":120}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer jaeger.Close()

	apmReg := apm.NewRegistry(cipher)
	obs := NewObservabilityConnectionHandler(db, cipher, apmReg)
	k8sReg := k8sclient.NewRegistry(cipher, false)
	sm := NewServiceMapHandler(db, cipher, k8sReg, NewK8sConnectionHandler(db, cipher, k8sReg), apmReg, obs)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.POST("/observability-connections", obs.Create)
	r.GET("/observability-connections", obs.List)
	r.GET("/service-map", sm.Get)
	call := func(user, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, res := call("alice", "POST", "/observability-connections", `{"name":"J","provider":"jaeger","url":"`+jaeger.URL+`","authKind":"none"}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, res)
	}
	id := res["data"].(map[string]any)["id"].(string)
	code, res = call("alice", "POST", "/observability-connections", `{"name":"bad","provider":"jaeger","url":"`+jaeger.URL+`","authKind":"datadog"}`)
	if code != 400 {
		t.Errorf("auth kind not allowed for provider: %d %v", code, res)
	}
	// Secrets never come back, only which ones are set.
	call("alice", "POST", "/observability-connections", `{"name":"S","provider":"signoz","url":"https://signoz.example.com","authKind":"apikey","secrets":{"apiKey":"topsecret"}}`)
	_, res = call("alice", "GET", "/observability-connections", "")
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "topsecret") || !strings.Contains(string(raw), `"hasSecrets":["apiKey"]`) {
		t.Errorf("list leaks or misses secrets: %s", raw)
	}

	if err := db.Create(&models.DBConnection{ID: "db1", UserID: "alice", Name: "Orders DB", Kind: "postgres", Host: "orders-db", Port: 5432}).Error; err != nil {
		t.Fatal(err)
	}

	code, res = call("alice", "GET", "/service-map?apm="+id+"&range=15m", "")
	if code != 200 {
		t.Fatalf("map: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	edges := data["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges %v", edges)
	}
	var dbNode map[string]any
	for _, n := range data["nodes"].([]any) {
		if n.(map[string]any)["id"] == "apm:orders-db" {
			dbNode = n.(map[string]any)
		}
	}
	if dbNode == nil || dbNode["connection"] == nil || dbNode["kind"] != "datastore" || dbNode["subtype"] != "postgres" {
		t.Errorf("saved DB connection not matched: %v", dbNode)
	}

	// Bob has no access to alice's connection: the source reports it.
	_, res = call("bob", "GET", "/service-map?apm="+id, "")
	src := res["data"].(map[string]any)["sources"].(map[string]any)["apm:"+id].(map[string]any)
	if src["ok"] == true || len(res["data"].(map[string]any)["edges"].([]any)) != 0 {
		t.Errorf("bob sees alice's map: %v", res)
	}
	if code, _ := call("alice", "GET", "/service-map", ""); code != 400 {
		t.Errorf("no source: %d", code)
	}
}
