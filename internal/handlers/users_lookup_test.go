package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Share lists resolve grantee names by id through /users/lookup.
func TestLookupUsers(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatal(err)
	}
	db.Create(&models.User{ID: "u1", Email: "a@example.test", Name: "Ada"})
	db.Create(&models.User{ID: "u2", Email: "b@example.test", Name: "Bo"})
	h := &AuthHandler{db: db}
	r := gin.New()
	r.GET("/users/lookup", h.LookupUsers)
	get := func(q string) []map[string]string {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/users/lookup"+q, nil))
		var out struct{ Data []map[string]string }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Data
	}
	got := get("?ids=u1,%20missing,u2")
	if len(got) != 2 {
		t.Fatalf("lookup: %+v", got)
	}
	names := map[string]string{}
	for _, u := range got {
		names[u["id"]] = u["name"]
	}
	if names["u1"] != "Ada" || names["u2"] != "Bo" {
		t.Fatalf("names: %+v", names)
	}
	if got := get(""); len(got) != 0 {
		t.Fatalf("no ids: %+v", got)
	}
}
