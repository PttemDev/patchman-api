package handlers

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"patchman-be/internal/database"
	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

// The pod switcher endpoint is gated like its neighbours: another user's
// connection is a 404 (no existence leak), a missing pod a 400, and an
// allowedNamespaces violation is refused before any cluster call.
func TestK8sPodSiblings_AccessAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "k8s.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	reg := k8sclient.NewRegistry(cipher, false)
	h := NewK8sOpsHandler(db, reg, NewK8sConnectionHandler(db, cipher, reg), false, nil)
	if err := db.Create(&models.K8sConnection{
		ID: "k1", UserID: "alice", Name: "c", AuthMode: "kubeconfig",
		OptionsJSON: `{"allowedNamespaces":["prod"]}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/k8s/:id/pod-siblings", h.PodSiblings)
	call := func(user, path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-User", user)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, _ := call("bob", "/k8s/k1/pod-siblings?namespace=prod&pod=api-1"); code != 404 {
		t.Errorf("other user: %d, want 404", code)
	}
	if code, _ := call("alice", "/k8s/k1/pod-siblings?namespace=prod&pod=%20"); code != 400 {
		t.Errorf("empty pod: %d, want 400", code)
	}
	if code, body := call("alice", "/k8s/k1/pod-siblings?namespace=kube-system&pod=api-1"); code != 502 || !strings.Contains(body, "not permitted") {
		t.Errorf("forbidden namespace: %d %s, want 502 refused by allowedNamespaces", code, body)
	}
}
