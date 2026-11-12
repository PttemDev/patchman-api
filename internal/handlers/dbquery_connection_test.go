package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func connCopyTestHandler(t *testing.T) (*DBConnectionHandler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:dbconncopy?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.DBConnection{}, &models.DBConnectionGrant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// dbConnRole's team-grant subquery only needs these two columns.
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("DELETE FROM db_connections")
	db.Exec("DELETE FROM db_connection_grants")
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return NewDBConnectionHandler(db, cipher, dbquery.NewRegistry(cipher)), db
}

func createConn(t *testing.T, h *DBConnectionHandler, userID string, body map[string]any) (int, connectionDTO, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", userID)
	c.Request = httptest.NewRequest("POST", "/api/db-connections", bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Create(c)
	var resp struct {
		Data connectionDTO `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Data, w.Body.String()
}

func storedSecrets(t *testing.T, h *DBConnectionHandler, db *gorm.DB, id string) (pw, dsn string) {
	t.Helper()
	var row models.DBConnection
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	pw, _ = h.cipher.Decrypt(row.PasswordEnc)
	if row.ConnectionStringEnc != "" {
		dsn, _ = h.cipher.Decrypt(row.ConnectionStringEnc)
	}
	return pw, dsn
}

func TestCreateCopySecretsFrom(t *testing.T) {
	h, db := connCopyTestHandler(t)

	code, src, body := createConn(t, h, "owner", map[string]any{
		"id": "src", "name": "orders", "kind": "postgres",
		"host": "db.internal", "port": 5432, "database": "orders",
		"username": "app", "password": "s3cret",
	})
	if code != 201 {
		t.Fatalf("create source = %d %s", code, body)
	}
	if src.CopiedFrom != "" {
		t.Errorf("plain create echoed copiedFrom=%q", src.CopiedFrom)
	}

	// Owner duplicates onto another database, leaving the password blank.
	code, dup, body := createConn(t, h, "owner", map[string]any{
		"id": "dup", "name": "orders (copy)", "kind": "postgres",
		"host": "db.internal", "port": 5432, "database": "billing",
		"username": "app", "copySecretsFrom": "src",
	})
	if code != 201 || dup.CopiedFrom != "src" {
		t.Fatalf("duplicate = %d copiedFrom=%q %s", code, dup.CopiedFrom, body)
	}
	if pw, _ := storedSecrets(t, h, db, "dup"); pw != "s3cret" {
		t.Errorf("duplicate password = %q, want the source's", pw)
	}

	// A typed password wins over the source's.
	code, _, body = createConn(t, h, "owner", map[string]any{
		"id": "dup2", "name": "x", "kind": "postgres", "host": "h", "port": 1,
		"database": "d", "password": "other", "copySecretsFrom": "src",
	})
	if code != 201 {
		t.Fatalf("duplicate w/ password = %d %s", code, body)
	}
	if pw, _ := storedSecrets(t, h, db, "dup2"); pw != "other" {
		t.Errorf("typed password = %q, want other", pw)
	}

	// A query-only grantee may not mint a copy of the credentials …
	db.Create(&models.DBConnectionGrant{ID: "g1", ConnectionID: "src", SubjectKind: "user", SubjectID: "reader", Role: "read", GrantedBy: "owner"})
	code, _, _ = createConn(t, h, "reader", map[string]any{
		"name": "steal", "kind": "postgres", "host": "h", "port": 1, "database": "d", "copySecretsFrom": "src",
	})
	if code != 403 {
		t.Errorf("read-grantee copy = %d, want 403", code)
	}
	// … nor can a stranger (no access at all → not found).
	code, _, _ = createConn(t, h, "stranger", map[string]any{
		"name": "steal", "kind": "postgres", "host": "h", "port": 1, "database": "d", "copySecretsFrom": "src",
	})
	if code != 404 {
		t.Errorf("stranger copy = %d, want 404", code)
	}
	// A 'write' co-owner can already reveal the password, so copying is fine.
	db.Create(&models.DBConnectionGrant{ID: "g2", ConnectionID: "src", SubjectKind: "user", SubjectID: "coowner", Role: "write", GrantedBy: "owner"})
	code, _, body = createConn(t, h, "coowner", map[string]any{
		"name": "mine", "kind": "postgres", "host": "h", "port": 1, "database": "d", "copySecretsFrom": "src",
	})
	if code != 201 {
		t.Errorf("co-owner copy = %d %s", code, body)
	}
}

func TestCreateCopyConnectionString(t *testing.T) {
	h, db := connCopyTestHandler(t)

	// DSN-only source: no host/port/database on the row.
	code, _, body := createConn(t, h, "owner", map[string]any{
		"id": "srv", "name": "atlas", "kind": "mongo",
		"connectionString": "mongodb+srv://u:p@cluster.example/db",
	})
	if code != 201 {
		t.Fatalf("create DSN source = %d %s", code, body)
	}

	// Copying the DSN satisfies the host/port requirement.
	code, _, body = createConn(t, h, "owner", map[string]any{
		"id": "srv2", "name": "atlas (copy)", "kind": "mongo",
		"copySecretsFrom": "srv", "copyConnectionString": true,
	})
	if code != 201 {
		t.Fatalf("DSN duplicate = %d %s", code, body)
	}
	if _, dsn := storedSecrets(t, h, db, "srv2"); dsn != "mongodb+srv://u:p@cluster.example/db" {
		t.Errorf("copied DSN = %q", dsn)
	}

	// Switched to the structured form: the DSN is NOT carried over (it would
	// override the edited fields), so host/port are required again.
	code, _, _ = createConn(t, h, "owner", map[string]any{
		"name": "atlas (form)", "kind": "mongo", "copySecretsFrom": "srv",
	})
	if code != 400 {
		t.Errorf("form-mode duplicate without host = %d, want 400", code)
	}
	code, _, body = createConn(t, h, "owner", map[string]any{
		"id": "srv3", "name": "atlas (form)", "kind": "mongo", "host": "h", "port": 27017, "copySecretsFrom": "srv",
	})
	if code != 201 {
		t.Fatalf("form-mode duplicate = %d %s", code, body)
	}
	if _, dsn := storedSecrets(t, h, db, "srv3"); dsn != "" {
		t.Errorf("form-mode duplicate kept DSN %q", dsn)
	}
}
