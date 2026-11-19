package handlers

import (
	"strings"
	"testing"

	"patchman-be/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMaskRequestSecrets(t *testing.T) {
	req := models.SavedRequest{
		URL:     "https://api.example.test/v1/items?api_key=K123&page=2",
		Headers: `[{"key":"Authorization","value":"Bearer tok-abc","enabled":true},{"key":"Content-Type","value":"application/json"},{"key":"X-Epa-Internal-Api-Key","value":"epa-key"},{"key":"Cookie","value":".AspNet.Cookies=xyz"},{"key":"X-Token","value":"{{token}}"}]`,
		Params:  `[{"key":"access_token","value":"p-tok"},{"key":"q","value":"shoes"}]`,
		Body:    `{"type":"raw","content":"<soap:Envelope><soap:Body><Login><user>u1</user><password>hunter2</password></Login></soap:Body></soap:Envelope>"}`,
		Auth:    `{"type":"basic","username":"alice","password":"s3cret"}`,
	}
	m := maskRequestSecrets(req)
	all := m.URL + m.Headers + m.Params + m.Body + m.Auth
	for _, leak := range []string{"K123", "tok-abc", "epa-key", "xyz", "p-tok", "hunter2", "s3cret"} {
		if strings.Contains(all, leak) {
			t.Errorf("leaked %q in %s", leak, all)
		}
	}
	for _, keep := range []string{"page=2", "application/json", "{{token}}", "shoes", "<user>u1</user>", `"username":"alice"`, `"type":"basic"`} {
		if !strings.Contains(all, keep) {
			t.Errorf("lost %q in %s", keep, all)
		}
	}
	// JSON body content (a JSON string inside the body JSON).
	jb := maskRequestSecrets(models.SavedRequest{Body: `{"type":"json","content":"{\"user\":\"u\",\"password\":\"pw1\",\"nested\":{\"client_secret\":\"cs\"}}"}`})
	if strings.Contains(jb.Body, "pw1") || strings.Contains(jb.Body, `\"cs\"`) || !strings.Contains(jb.Body, `\"user\":\"u\"`) {
		t.Errorf("json body: %s", jb.Body)
	}
	// API-key auth keeps the header name, hides the value.
	ak := maskAuth(`{"type":"apikey","key":"X-API-Key","value":"live-123","addTo":"header"}`)
	if strings.Contains(ak, "live-123") || !strings.Contains(ak, "X-API-Key") {
		t.Errorf("apikey auth: %s", ak)
	}
}

func TestRestoreRequestField(t *testing.T) {
	stored := `[{"key":"Authorization","value":"Bearer tok-abc"},{"key":"Accept","value":"*/*"}]`
	shown := (&masker{}).text(stored)
	// Unchanged field → the stored value.
	if got, err := restoreRequestField("headers", shown, stored); err != nil || got != stored {
		t.Fatalf("unchanged: %q %v", got, err)
	}
	// A header added, the masked one kept → its secret is restored by key.
	in := `[{"key":"Authorization","value":"***"},{"key":"Accept","value":"*/*"},{"key":"X-Trace","value":"1"}]`
	got, err := restoreRequestField("headers", in, stored)
	if err != nil || !strings.Contains(got, "Bearer tok-abc") || !strings.Contains(got, "X-Trace") {
		t.Fatalf("rows: %q %v", got, err)
	}
	// Auth fields by name.
	got, err = restoreRequestField("auth", `{"type":"basic","username":"bob","password":"***"}`, `{"type":"basic","username":"alice","password":"s3cret"}`)
	if err != nil || !strings.Contains(got, "s3cret") || !strings.Contains(got, "bob") {
		t.Fatalf("auth: %q %v", got, err)
	}
	// A changed body still holding *** can't be resolved → a clear error.
	if _, err := restoreRequestField("body", `{"type":"raw","content":"<password>***</password><x>new</x>"}`, `{"type":"raw","content":"<password>hunter2</password>"}`); err == nil {
		t.Error("changed body with *** must be refused")
	}
	// No *** → as sent.
	if got, _ := restoreRequestField("body", `{"type":"raw","content":"plain"}`, `x`); got != `{"type":"raw","content":"plain"}` {
		t.Errorf("passthrough: %q", got)
	}
	// Secret env vars by key.
	if got := restoreSecretVars(`[{"key":"ldap_password","value":"***","secret":true},{"key":"host","value":"h2"}]`, `[{"key":"ldap_password","value":"pw","secret":true},{"key":"host","value":"h1"}]`); !strings.Contains(got, `"pw"`) || !strings.Contains(got, "h2") {
		t.Errorf("env vars: %s", got)
	}
}

func TestRequestShowsSecretsFollowsTopLevel(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Collection{}); err != nil {
		t.Fatal(err)
	}
	root, mid := "root", "mid"
	db.Create(&models.Collection{ID: root, UserID: "u", Name: "R", MCPShowSecrets: true})
	db.Create(&models.Collection{ID: mid, UserID: "u", Name: "M", ParentID: &root})
	db.Create(&models.Collection{ID: "leaf", UserID: "u", Name: "L", ParentID: &mid})
	db.Create(&models.Collection{ID: "other", UserID: "u", Name: "O"})
	if !requestShowsSecrets(db, "leaf") {
		t.Error("a folder follows its top-level collection's setting")
	}
	if requestShowsSecrets(db, "other") || requestShowsSecrets(db, "missing") {
		t.Error("off by default")
	}
}
