package handlers

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// A PAT connection's clone/pull/push use its own token, whatever the FE sent;
// other connections keep the FE-minted token.
func TestGitOpsTokenPrefersConnectionPAT(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}); err != nil {
		t.Fatal(err)
	}
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := cipher.Encrypt("glpat-own")
	db.Create(&models.GitConnection{ID: "pat", UserID: "u1", Name: "pat", BaseURL: "https://git.example.com", AuthKind: "pat", CredentialEnc: enc})
	db.Create(&models.GitConnection{ID: "oauth", UserID: "u1", Name: "oauth", BaseURL: "https://gitlab.example.com", AuthKind: "oauth"})

	h := NewGitOpsHandler(db, cipher, true)
	cases := []struct{ name, user, conn, supplied, want string }{
		{"pat wins over a minted token", "u1", "pat", "minted", "glpat-own"},
		{"pat without a minted token", "u1", "pat", "", "glpat-own"},
		{"oauth keeps the minted token", "u1", "oauth", " minted ", "minted"},
		{"someone else's connection", "u2", "pat", "minted", "minted"},
		{"no connection", "u1", "", "", ""},
	}
	for _, c := range cases {
		if got := h.opsToken(c.user, c.conn, c.supplied); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
