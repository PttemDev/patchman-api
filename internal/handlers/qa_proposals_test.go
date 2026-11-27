package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"patchman-be/internal/database"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

func TestQAProposalsAndPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "prop.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	h := NewQAProposalHandler(db, cipher)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-User"))
		c.Next()
	})
	r.POST("/qa/proposals", h.Create)
	r.GET("/qa/proposals", h.List)
	r.GET("/qa/proposals/:id/request", h.Request)
	r.POST("/qa/proposals/:id/resolve", h.Resolve)
	r.GET("/policy", h.GetAgentPolicy)
	r.PUT("/policy", h.PutAgentPolicy)
	do := func(user, method, path, body string, hdr ...string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	// Only CR + QA setup routes.
	if code, _ := do("alice", "POST", "/qa/proposals", `{"summary":"x","request":{"method":"DELETE","path":"/collections/1"}}`); code != 400 {
		t.Fatalf("non-setup route accepted: %d", code)
	}
	code, body := do("alice", "POST", "/qa/proposals", `{"summary":"add jira","request":{"method":"POST","path":"/jira-connections","body":{"secret":"TOKEN-XYZ"}}}`)
	if code != 201 || strings.Contains(body, "TOKEN-XYZ") {
		t.Fatalf("create: %d %s", code, body)
	}
	var p models.QASetupProposal
	db.First(&p)
	if strings.Contains(p.RequestEnc, "TOKEN-XYZ") {
		t.Fatal("request stored in clear")
	}
	// An agent can't read or apply it; someone else can't either; the owner can.
	if code, _ := do("alice", "GET", "/qa/proposals/"+p.ID+"/request", "", mcpClientHeader, mcpClientHeaderValue); code != 403 {
		t.Fatalf("agent reads its proposal: %d", code)
	}
	if code, _ := do("bob", "GET", "/qa/proposals/"+p.ID+"/request", ""); code != 404 {
		t.Fatalf("bob reads: %d", code)
	}
	if code, body := do("alice", "GET", "/qa/proposals/"+p.ID+"/request", ""); code != 200 || !strings.Contains(body, "TOKEN-XYZ") {
		t.Fatalf("owner reads: %d %s", code, body)
	}
	if code, _ := do("alice", "POST", "/qa/proposals/"+p.ID+"/resolve", `{"status":"rejected"}`); code != 200 {
		t.Fatal("reject")
	}
	db.First(&p, "id = ?", p.ID)
	if p.RequestEnc != "" || p.Status != "rejected" {
		t.Fatalf("rejected proposal keeps its request: %+v", p)
	}

	// Policy: default ask; an agent can't turn it off; the person can.
	if _, body := do("alice", "GET", "/policy", ""); !strings.Contains(body, `"ask":true`) {
		t.Fatalf("default: %s", body)
	}
	if code, _ := do("alice", "PUT", "/policy", `{"ask":false}`, mcpClientHeader, mcpClientHeaderValue); code != 403 {
		t.Fatalf("agent changed the policy: %d", code)
	}
	if code, _ := do("alice", "PUT", "/policy", `{"ask":false}`); code != 200 {
		t.Fatal("person can't change policy")
	}
	var out struct {
		Data qaAgentSetupPolicy `json:"data"`
	}
	_, body = do("alice", "GET", "/policy", "")
	_ = json.Unmarshal([]byte(body), &out)
	if out.Data.Ask {
		t.Fatal("policy not saved")
	}
}
