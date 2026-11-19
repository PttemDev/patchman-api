package middleware

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/models"
	"patchman-be/internal/sessions"
)

// RFC L8 end to end through JWTAuth: a legacy token is upgraded to a
// session on first use; revoking that session (or signing out everywhere)
// refuses both the new and the old token.
func TestJWTAuthSessions(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:jwt_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.UserSession{}, &models.SessionCutoff{}); err != nil {
		t.Fatal(err)
	}
	prev := sessions.Default
	sessions.Default = sessions.New(db, 0)
	defer func() { sessions.Default = prev }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", JWTAuth("sec", time.Hour), func(c *gin.Context) { c.String(200, c.GetString("sessionID")) })
	call := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	legacy, _ := authtoken.Mint("sec", "u1", time.Hour)
	w := call(legacy)
	upgraded := w.Header().Get(RefreshedTokenHeader)
	if w.Code != 200 || upgraded == "" {
		t.Fatalf("legacy token: %d, refreshed=%q", w.Code, upgraded)
	}
	p, _ := jwt.Parse(upgraded, func(*jwt.Token) (any, error) { return []byte("sec"), nil })
	sid, _ := p.Claims.(jwt.MapClaims)["sid"].(string)
	if sid == "" {
		t.Fatal("upgraded token has no sid")
	}
	if w := call(upgraded); w.Code != 200 || w.Body.String() != sid {
		t.Fatalf("upgraded token: %d %q", w.Code, w.Body.String())
	}

	if _, err := sessions.Default.Revoke("u1", nil, ""); err != nil {
		t.Fatal(err)
	}
	if w := call(upgraded); w.Code != 401 {
		t.Fatalf("revoked session: want 401, got %d", w.Code)
	}
	if w := call(legacy); w.Code != 401 {
		t.Fatalf("legacy token after sign-out-everywhere: want 401, got %d", w.Code)
	}
}
