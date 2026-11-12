package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func tokenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.MCPToken{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Shared-cache in-memory DB persists across connections for the
	// process; wipe any rows a previous test left behind.
	db.Exec("DELETE FROM mcp_tokens")
	return db
}

func createToken(t *testing.T, h *MCPTokenHandler, userID string) (rawToken, id string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", userID)
	c.Request = httptest.NewRequest("POST", "/api/mcp/tokens", bytes.NewReader([]byte(`{"name":"t"}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Create(c)
	if w.Code != 201 {
		t.Fatalf("create status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
			ID    string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Data.Token, resp.Data.ID
}

func TestMCPTokenLifecycle(t *testing.T) {
	db := tokenTestDB(t)
	h := NewMCPTokenHandler(db)

	raw, id := createToken(t, h, "user-1")

	// Created token must look like an MCP PAT and resolve to its owner.
	if len(raw) < 10 || raw[:5] != "pmcp_" {
		t.Errorf("token = %q, want pmcp_ prefix", raw)
	}
	if uid, _, ok := ResolveMCPToken(db, raw); !ok || uid != "user-1" {
		t.Errorf("resolve = (%q,%v), want (user-1,true)", uid, ok)
	}

	// The raw value is never persisted — only its hash.
	var stored models.MCPToken
	if err := db.First(&stored, "id = ?", id).Error; err != nil {
		t.Fatalf("load token: %v", err)
	}
	if stored.TokenHash == raw || stored.TokenHash != hashToken(raw) {
		t.Error("stored hash should be SHA-256 of raw token, not the raw token")
	}

	// Unknown / empty tokens are rejected.
	if _, _, ok := ResolveMCPToken(db, "pmcp_wrong"); ok {
		t.Error("unknown token resolved")
	}
	if _, _, ok := ResolveMCPToken(db, ""); ok {
		t.Error("empty token resolved")
	}

	// Revoking the token makes it unusable.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("DELETE", "/api/mcp/tokens/"+id, nil)
	h.Delete(c)
	if w.Code != 200 {
		t.Fatalf("delete status = %d, body=%s", w.Code, w.Body.String())
	}
	if _, _, ok := ResolveMCPToken(db, raw); ok {
		t.Error("revoked token still resolves")
	}
}

// TestMCPTokenCreateMalformedBody rejects a request with a body that's
// present but not valid JSON, instead of silently falling back to the
// default name.
func TestMCPTokenCreateMalformedBody(t *testing.T) {
	db := tokenTestDB(t)
	h := NewMCPTokenHandler(db)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/api/mcp/tokens", bytes.NewReader([]byte(`{not json`)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Create(c)
	if w.Code != 400 {
		t.Errorf("malformed body status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}

// TestMCPTokenCreateEmptyBody accepts an empty body and falls back to the
// default token name — the documented "minimal" mint path.
func TestMCPTokenCreateEmptyBody(t *testing.T) {
	db := tokenTestDB(t)
	h := NewMCPTokenHandler(db)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/api/mcp/tokens", nil)
	h.Create(c)
	if w.Code != 201 {
		t.Fatalf("empty body status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
}

// TestEnsureLocalMCPToken covers the embedded local-token upsert: it must
// create the row when absent, be idempotent for the same plaintext, and
// re-hash the same row (never orphan) when the plaintext is regenerated.
func TestEnsureLocalMCPToken(t *testing.T) {
	db := tokenTestDB(t)
	const user = "local-user"
	const name = "Local (desktop bridge)"

	countRows := func() int64 {
		var n int64
		db.Model(&models.MCPToken{}).Where("user_id = ? AND name = ?", user, name).Count(&n)
		return n
	}
	loadRow := func() models.MCPToken {
		var row models.MCPToken
		if err := db.Where("user_id = ? AND name = ?", user, name).First(&row).Error; err != nil {
			t.Fatalf("load row: %v", err)
		}
		return row
	}

	// (a) Create-if-absent → resolvable by the raw token.
	raw1 := "pmcp_" + "aaaaaaaa11111111"
	if err := EnsureLocalMCPToken(db, user, raw1, name); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if uid, _, ok := ResolveMCPToken(db, raw1); !ok || uid != user {
		t.Fatalf("resolve raw1 = (%q,%v), want (%s,true)", uid, ok, user)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("row count after create = %d, want 1", n)
	}
	firstID := loadRow().ID

	// (b) Same plaintext re-run → no-op: same row id, still exactly one row.
	if err := EnsureLocalMCPToken(db, user, raw1, name); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("row count after re-run = %d, want 1", n)
	}
	if got := loadRow().ID; got != firstID {
		t.Fatalf("row id changed on re-run: %q → %q", firstID, got)
	}

	// (c) Regenerated plaintext → same row id, hash+prefix updated in place,
	// still one row (no orphan), old token no longer resolves.
	raw2 := "pmcp_" + "bbbbbbbb22222222"
	if err := EnsureLocalMCPToken(db, user, raw2, name); err != nil {
		t.Fatalf("regenerate ensure: %v", err)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("row count after regenerate = %d, want 1 (orphaned row?)", n)
	}
	row := loadRow()
	if row.ID != firstID {
		t.Fatalf("regenerate created a new row: %q → %q", firstID, row.ID)
	}
	if row.TokenHash != hashToken(raw2) {
		t.Error("hash not updated to the regenerated token")
	}
	if row.Prefix != raw2[:13] {
		t.Errorf("prefix = %q, want %q", row.Prefix, raw2[:13])
	}
	if _, _, ok := ResolveMCPToken(db, raw2); !ok {
		t.Error("regenerated token does not resolve")
	}
	if _, _, ok := ResolveMCPToken(db, raw1); ok {
		t.Error("old token still resolves after regeneration")
	}
}

// TestMCPTokenOwnershipIsolation ensures a user can't revoke another
// user's token.
func TestMCPTokenOwnershipIsolation(t *testing.T) {
	db := tokenTestDB(t)
	h := NewMCPTokenHandler(db)

	_, id := createToken(t, h, "owner")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "attacker")
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("DELETE", "/api/mcp/tokens/"+id, nil)
	h.Delete(c)
	if w.Code != 404 {
		t.Errorf("cross-user delete status = %d, want 404", w.Code)
	}
}

// TestMCPTokenUpdateScopes covers the per-token scope editor: valid groups
// persist, null clears back to all, an unknown group is a 400, and a user
// can't edit another user's token.
func TestMCPTokenUpdateScopes(t *testing.T) {
	db := tokenTestDB(t)
	h := NewMCPTokenHandler(db)
	raw, id := createToken(t, h, "owner")

	patch := func(userID, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("userID", userID)
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest("PATCH", "/api/mcp/tokens/"+id, bytes.NewReader([]byte(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateScopes(c)
		return w
	}

	// New token defaults to NULL scopes (all groups) → resolves with nil.
	if _, sc, ok := ResolveMCPToken(db, raw); !ok || sc != nil {
		t.Fatalf("fresh token scopes = %v, want nil", sc)
	}

	// Restrict to db,http (+ a dup that must be collapsed).
	if w := patch("owner", `{"scopes":["db","http","db"]}`); w.Code != 200 {
		t.Fatalf("update scopes status = %d, body=%s", w.Code, w.Body.String())
	}
	if _, sc, ok := ResolveMCPToken(db, raw); !ok || sc == nil || *sc != "db,http" {
		t.Fatalf("scopes after update = %v, want \"db,http\"", sc)
	}

	// Unknown group → 400, no change.
	if w := patch("owner", `{"scopes":["dbb"]}`); w.Code != 400 {
		t.Errorf("unknown group status = %d, want 400", w.Code)
	}

	// null → back to all (nil).
	if w := patch("owner", `{"scopes":null}`); w.Code != 200 {
		t.Fatalf("clear scopes status = %d", w.Code)
	}
	if _, sc, _ := ResolveMCPToken(db, raw); sc != nil {
		t.Fatalf("scopes after null = %v, want nil", sc)
	}

	// Cross-user edit is a 404 (ownership-scoped).
	if w := patch("attacker", `{"scopes":[]}`); w.Code != 404 {
		t.Errorf("cross-user update status = %d, want 404", w.Code)
	}
}
