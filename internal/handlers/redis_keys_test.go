package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/models"
	"patchman-be/internal/redisquery"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func redisOpsTestHandler(t *testing.T) (*RedisOpsHandler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:redisops?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.RedisConnection{}, &models.RedisConnectionGrant{}, &models.RedisActivityHistory{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("DELETE FROM redis_connections")
	db.Exec("DELETE FROM redis_connection_grants")
	reg := redisquery.NewRegistry(nil)
	return NewRedisOpsHandler(db, reg, NewRedisConnectionHandler(db, nil, reg)), db
}

func redisOpsCall(h gin.HandlerFunc, userID, method, path, connID string, body any) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", userID)
	c.Params = gin.Params{{Key: "id", Value: connID}}
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	return w
}

// Element removal (and key delete) is refused before any Redis call when the
// connection's read-write switch is off — for the owner too — and for a
// read-only grant holder even when the switch is on. The connection points
// at an unroutable address, so reaching Redis would surface as 502, not 403.
func TestRedisRemoveElementWriteGate(t *testing.T) {
	h, db := redisOpsTestHandler(t)
	ro := models.RedisConnection{ID: "conn-ro", UserID: "owner", Name: "ro", Address: "127.0.0.1:1", Mode: "standalone", AllowWrite: false}
	rw := models.RedisConnection{ID: "conn-rw", UserID: "owner", Name: "rw", Address: "127.0.0.1:1", Mode: "standalone"}
	for _, c := range []models.RedisConnection{ro, rw} {
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}
	// gorm skips a false bool on create (column default) — set it explicitly.
	db.Model(&models.RedisConnection{}).Where("id = ?", "conn-rw").Update("allow_write", true)
	db.Create(&models.RedisConnectionGrant{ID: "g1", ConnectionID: "conn-rw", SubjectKind: "user", SubjectID: "reader", Role: "read", GrantedBy: "owner"})

	body := map[string]any{"db": 0, "key": "h", "type": "hash", "field": "f"}

	w := redisOpsCall(h.RemoveElement, "owner", "POST", "/api/redis/conn-ro/key/element/remove", "conn-ro", body)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("read-only conn: %d %s, want 403 read-only", w.Code, w.Body.String())
	}
	w = redisOpsCall(h.DeleteKey, "owner", "DELETE", "/api/redis/conn-ro/key?key=h", "conn-ro", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-only conn key delete: %d, want 403", w.Code)
	}
	w = redisOpsCall(h.RemoveElement, "reader", "POST", "/api/redis/conn-rw/key/element/remove", "conn-rw", body)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "write access") {
		t.Fatalf("read grant: %d %s, want 403 write access", w.Code, w.Body.String())
	}
	w = redisOpsCall(h.RemoveElement, "stranger", "POST", "/api/redis/conn-rw/key/element/remove", "conn-rw", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no access: %d, want 404", w.Code)
	}
	// The owner on a read-write connection passes the gate; a malformed ref
	// is then rejected by validation (400) without touching Redis.
	w = redisOpsCall(h.RemoveElement, "owner", "POST", "/api/redis/conn-rw/key/element/remove", "conn-rw", map[string]any{"key": "h", "type": "hash"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("owner rw, missing field: %d %s, want 400", w.Code, w.Body.String())
	}
	w = redisOpsCall(h.RemoveElement, "owner", "POST", "/api/redis/conn-rw/key/element/remove", "conn-rw", map[string]any{"key": "s", "type": "string"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("owner rw, string type: %d, want 400", w.Code)
	}
}
