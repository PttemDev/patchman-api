package handlers

import (
	"testing"

	"patchman-be/internal/models"
)

func TestValidKibanaURL(t *testing.T) {
	for _, ok := range []string{"", "kibana:5601", "http://k:5601", "https://k.example.com/base"} {
		if !validKibanaURL(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "ftp://k", "https://"} {
		if validKibanaURL(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

// A stored ES secret must not be redirected to a new Kibana host unless the
// secret is re-entered in the same update.
func TestKibanaSecretGuard(t *testing.T) {
	pw := "s3cret"
	empty := ""
	conn := models.ESConnection{AuthKind: "basic", PasswordEnc: "enc", OptionsJSON: `{"kibanaUrl":"https://old:5601"}`}
	opts := func(u string) map[string]any { return map[string]any{"kibanaUrl": u} }

	cases := []struct {
		name string
		in   updateESInput
		want bool
	}{
		{"no options", updateESInput{}, true},
		{"same host", updateESInput{Options: opts("https://OLD:5601/")}, true},
		{"cleared", updateESInput{Options: opts("")}, true},
		{"new host, no secret", updateESInput{Options: opts("https://evil:5601")}, false},
		{"new host, empty secret", updateESInput{Options: opts("https://evil:5601"), Password: &empty}, false},
		{"new host, secret given", updateESInput{Options: opts("https://new:5601"), Password: &pw}, true},
	}
	for _, c := range cases {
		if got := kibanaSecretGuard(conn, c.in); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// First Kibana URL on an existing basic connection also needs the secret.
	fresh := models.ESConnection{AuthKind: "apiKey", ApiKeyEnc: "enc", OptionsJSON: `{}`}
	if kibanaSecretGuard(fresh, updateESInput{Options: opts("https://k:5601")}) {
		t.Fatal("adding a Kibana URL must require the API key")
	}
	// kibanaProxy and none are exempt.
	if !kibanaSecretGuard(models.ESConnection{AuthKind: "kibanaProxy", PasswordEnc: "e"}, updateESInput{Options: opts("https://x")}) {
		t.Fatal("kibanaProxy must be exempt")
	}
	if !kibanaSecretGuard(models.ESConnection{AuthKind: "none"}, updateESInput{Options: opts("https://x")}) {
		t.Fatal("none must be exempt")
	}
}
