package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// Test on a saved connection checks what the dialog shows: a typed token
// replaces the stored one, and the stored token never goes to a new address.
func TestGitConnectionTestUsesDraft(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{} // server → tokens it received
	gitlab := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("PRIVATE-TOKEN"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"version":"17.0.0"}`))
		}))
	}
	a, b := gitlab("a"), gitlab("b")
	defer a.Close()
	defer b.Close()

	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	enc, _ := cipher.Encrypt("stored-token")
	db.Create(&models.GitConnection{ID: "c1", UserID: "u1", Name: "c1", BaseURL: a.URL, AuthKind: "pat", CredentialEnc: enc})
	h := NewGitConnectionHandler(db, cipher)

	call := func(body string) map[string]any {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/git/connections/c1/test", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: "c1"}}
		c.Set("userID", "u1")
		h.Test(c)
		var res struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res.Data
	}

	if r := call(``); r["ok"] != true {
		t.Fatalf("no draft: %v", r)
	}
	if r := call(`{"baseUrl":"` + a.URL + `/","credential":" typed-token "}`); r["ok"] != true {
		t.Fatalf("typed token: %v", r)
	}
	if r := call(`{"baseUrl":"` + b.URL + `"}`); r["ok"] != false {
		t.Fatalf("new address without a token should not test: %v", r)
	}
	if r := call(`{"baseUrl":"` + b.URL + `","credential":"b-token"}`); r["ok"] != true {
		t.Fatalf("new address with its token: %v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen["a"], ","); got != "stored-token,typed-token" {
		t.Errorf("server a got %q", got)
	}
	if got := strings.Join(seen["b"], ","); got != "b-token" {
		t.Errorf("server b got %q (the stored token must never reach it)", got)
	}
}
