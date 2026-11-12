package handlers

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// A PAT connection to another GitLab is browsed on THAT GitLab with its own
// token; the configured instance is never contacted.
func TestGitRemoteUsesPATConnectionBaseURL(t *testing.T) {
	var mu sync.Mutex
	hits := map[string][]string{} // server → "path auth"
	srv := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name] = append(hits[name], r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"id":7,"name":"wms","path":"wms","path_with_namespace":"team/wms","default_branch":"main"}]`))
		}))
	}
	configured, other := srv("configured"), srv("other")
	defer configured.Close()
	defer other.Close()

	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitConnectionGrant{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt("glpat-other")
	db.Create(&models.GitConnection{ID: "c1", UserID: "u1", Name: "other", BaseURL: other.URL + "/", AuthKind: "pat", CredentialEnc: enc})

	h := NewGitRemoteHandler(db, &config.Config{GitLabBaseURL: configured.URL}, NewGitConnectionHandler(db, cipher), nil)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/git/connections/c1/repos", nil)
	c.Params = gin.Params{{Key: "id", Value: "c1"}}
	c.Set("userID", "u1")
	h.Repos(c)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "team/wms") {
		t.Fatalf("repos = %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits["configured"]) != 0 {
		t.Errorf("the configured GitLab was contacted: %v", hits["configured"])
	}
	if len(hits["other"]) != 1 || hits["other"][0] != "/api/v4/projects Bearer glpat-other" {
		t.Errorf("connection's GitLab got %v", hits["other"])
	}
}
