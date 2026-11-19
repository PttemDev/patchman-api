package sessions

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/models"
)

func testStore(t *testing.T, idle time.Duration) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:sess_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.UserSession{}, &models.SessionCutoff{}); err != nil {
		t.Fatal(err)
	}
	return New(db, idle)
}

// RFC L8: a revoked device is refused on its next request even though its
// token is still valid; other devices keep working.
func TestRevokeEndsOnlyThatDevice(t *testing.T) {
	s := testStore(t, 0)
	laptop, _ := s.Create("u1", "PatchMan/1.60 mac", "10.0.0.1", "1.60.0")
	phone, _ := s.Create("u1", "Safari", "10.0.0.2", "")
	if err := s.Validate(laptop, "u1"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Revoke("u1", []string{laptop}, ""); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if err := s.Validate(laptop, "u1"); err != ErrRevoked {
		t.Fatalf("revoked laptop: %v", err)
	}
	if err := s.Validate(phone, "u1"); err != nil {
		t.Fatalf("phone must keep working: %v", err)
	}
	if err := s.Validate(phone, "someone-else"); err != ErrRevoked {
		t.Fatal("a sid must not validate for another user")
	}
	list, _ := s.List("u1")
	if len(list) != 1 || list[0].ID != phone {
		t.Fatalf("list: %+v", list)
	}
	// revoke-others keeps the current one
	tablet, _ := s.Create("u1", "iPad", "", "")
	if n, _ := s.Revoke("u1", nil, tablet); n != 1 {
		t.Fatalf("revoke others: %d", n)
	}
	if s.Validate(tablet, "u1") != nil || s.Validate(phone, "u1") != ErrRevoked {
		t.Fatal("revoke-others result wrong")
	}
}

func TestIdleTTL(t *testing.T) {
	s := testStore(t, time.Hour)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	sid, _ := s.Create("u1", "", "", "")
	if err := s.Validate(sid, "u1"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if err := s.Validate(sid, "u1"); err != ErrIdle {
		t.Fatalf("idle: %v", err)
	}
}

// The JWT carries the sid; a token for a revoked session is still a valid
// signature — the middleware is what refuses it (covered in middleware).
func TestMintSessionCarriesSid(t *testing.T) {
	tok, err := authtoken.MintSession("secret", "u1", "sid-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jwt.Parse(tok, func(*jwt.Token) (any, error) { return []byte("secret"), nil })
	if parsed.Claims.(jwt.MapClaims)["sid"] != "sid-1" {
		t.Fatal("sid claim missing")
	}
	_ = gin.New
	_ = http.StatusOK
	_ = httptest.NewRecorder
}

// Tokens minted before sessions existed have no row: "sign out everywhere"
// must still end them, via the per-user cutoff.
func TestLegacyTokensCutOff(t *testing.T) {
	s := testStore(t, 0)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	issued := clock.Add(-time.Hour)
	if err := s.CheckToken("u1", "", issued); err != nil {
		t.Fatalf("legacy token before any cutoff: %v", err)
	}
	if _, err := s.Revoke("u1", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckToken("u1", "", issued); err != ErrRevoked {
		t.Fatalf("legacy token after sign-out-everywhere: %v", err)
	}
	if err := s.CheckToken("u2", "", issued); err != nil {
		t.Fatalf("other users unaffected: %v", err)
	}
	clock = clock.Add(time.Minute)
	if err := s.CheckToken("u1", "", clock); err != nil {
		t.Fatalf("token issued after the cutoff: %v", err)
	}
	// A single-device revoke sets no cutoff.
	s2 := testStore(t, 0)
	sid, _ := s2.Create("u3", "", "", "")
	_, _ = s2.Revoke("u3", []string{sid}, "")
	if err := s2.CheckToken("u3", "", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("single revoke must not cut legacy tokens: %v", err)
	}
}
