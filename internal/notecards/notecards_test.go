package notecards

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBodyNoteLinks(t *testing.T) {
	a, b := "11111111-2222-4333-8444-555555555555", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := "See [A](note:" + a + "), [B](note:" + b + " \"depends_on\"), [A again](note:" + a + "#setup) " +
		"and [B typo](note:" + b + " \"uses\") and [A](note:" + a + " \"refers_to\")."
	got := BodyNoteLinks(body)
	want := []BodyLink{{a, "related"}, {b, "depends_on"}, {b, "related"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestRenderBudgetAndScope(t *testing.T) {
	hits := []Hit{
		{ID: "x-11111111", Title: "Auth flow", Line: "JWT 15m", Body: strings.Repeat("body ", 400), Scope: "team: Backend",
			Rels: []Rel{{Out: true, Type: "depends_on", ID: "y-22222222", Title: "Session store"}, {Out: true, Type: "refers_to", Title: "patchman://request/1"}}},
		{ID: "y-22222222", Title: "Session store", Line: "Redis"},
	}
	out := Render(hits, 120, []string{"#33333333 is stale."})
	if len(out) > 480 {
		t.Errorf("over budget: %d", len(out))
	}
	for _, want := range []string{"#11111111 Auth flow | JWT 15m [team: Backend]", "  → depends_on #22222222 Session store", "  → refers_to patchman://request/1", "---\nbody"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if Render(nil, 800, nil) != NoMatch {
		t.Error("empty render")
	}
}

func TestSecretKindAndDeriveCard(t *testing.T) {
	for _, s := range []string{"key AKIAABCDEFGHIJKLMNOP", "ghp_" + strings.Repeat("a", 36), "password=hunter22"} {
		if SecretKind(s) == "" {
			t.Errorf("missed secret in %q", s)
		}
	}
	for _, s := range []string{"Passwords rotate every 90 days.", "The API key lives in Vault."} {
		if k := SecretKind(s); k != "" {
			t.Errorf("false positive %q in %q", k, s)
		}
	}
	title, summary, body := DeriveCard("# Cache TTL\nPages are cached 5 minutes. Purge with the admin API.")
	if title != "Cache TTL" || summary != "Pages are cached 5 minutes." || body != "Pages are cached 5 minutes. Purge with the admin API." {
		t.Errorf("derive = %q | %q | %q", title, summary, body)
	}
}

func TestSecretKindWiderShapesAndPlaceholders(t *testing.T) {
	for _, s := range []string{
		"- Pass: Xq7fakeValue9",
		"DB_PASSWORD=Xq7fakeValue9",
		"JWT_SECRET=Xq7fakeValue9",
		"Şifre: Xq7fakeValue9",
		"postgres://user:Xq7fakeValue9@10.0.0.1:5432/db",
		"token: Xq7fakeValue9abc",
		"docker run -e GITLAB_TOKEN_ENC_KEY=c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MTIzNDU2Nzg= img",
	} {
		if SecretKind(s) == "" {
			t.Errorf("missed secret in %q", s)
		}
	}
	for _, s := range []string{
		"`/realtime?token=<jwt>` WS, reconnect",
		"URL DSN `sqlserver://user:pass@host:port?database=x`",
		`export APPLE_PASSWORD="..."            # app-specific password`,
		`export TAURI_SIGNING_PRIVATE_KEY="$(cat src-tauri/.keys/patchman.key)"`,
		`export TAURI_SIGNING_PRIVATE_KEY_PASSWORD=""`,
		"token: string, secretName: my-tls-secret, passthrough=true",
		"GITLAB_TOKEN_ENC_KEY must be base64 of 32 bytes; random JWT_SECRET / GITLAB_TOKEN_ENC_KEY per run",
		"`fe/src-tauri/.keys/patchman.key` holds the updater key; password is an empty string.",
		"API_KEY_HEADER=X-Api-Key",
		"refresh header token: X-Refreshed-Token",
	} {
		if k := SecretKind(s); k != "" {
			t.Errorf("false positive %q in %q", k, s)
		}
	}
}

// The real memories migrated from Claude Code (getman project) are the
// negative corpus: none of them holds a secret, so none may be refused.
// PATCHMAN_MEMORY_CORPUS=/path/cards.json go test ./internal/notecards/
func TestSecretKindPassesMemoryCorpus(t *testing.T) {
	path := os.Getenv("PATCHMAN_MEMORY_CORPUS")
	if path == "" {
		t.Skip("set PATCHMAN_MEMORY_CORPUS to a cards.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cards []struct{ Title, Text string }
	if err := json.Unmarshal(raw, &cards); err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if k := SecretKind(c.Text); k != "" {
			t.Errorf("%q refused as %s", c.Title, k)
		}
	}
	t.Logf("%d cards checked", len(cards))
}

func TestDeriveCardDropsRepeatedSummary(t *testing.T) {
	title, summary, body := DeriveCard("Tauri signing\nThe key password is empty.\n\nExport both vars before tauri build.")
	if title != "Tauri signing" || summary != "The key password is empty." || body != "Export both vars before tauri build." {
		t.Errorf("derive = %q | %q | %q", title, summary, body)
	}
}

func TestExcerptKeepsMatchingParagraphs(t *testing.T) {
	body := "Intro line.\n\n" + strings.Repeat("filler words here. ", 30) + "\n\nThe answer: clientInfo names the agent.\n\nTail."
	got := Excerpt(body, []string{"clientinfo"}, 120)
	if !strings.Contains(got, "clientInfo names the agent") || strings.Contains(got, "filler") || !strings.HasPrefix(got, "…") {
		t.Errorf("excerpt = %q", got)
	}
	if got := Excerpt("short", []string{"x"}, 100); got != "short" {
		t.Errorf("short body changed: %q", got)
	}
}

func TestProjectKeyAndWikiLinks(t *testing.T) {
	for in, want := range map[string]string{"getman": "getman", "GetMan": "getman", "merchant payoffs/backoffice": "merchant-payoffs-backoffice",
		"Çalışma Alanı": "calisma-alani", "  ": "", "a.b_c": "a.b_c"} {
		if got := ProjectKey(in); got != want {
			t.Errorf("ProjectKey(%q) = %q, want %q", in, got, want)
		}
	}
	got := WikiLinks("See [[Tauri signing]], [[#abcd1234]] and [[tauri signing|again]]; not [[]].")
	if !reflect.DeepEqual(got, []string{"Tauri signing", "#abcd1234"}) {
		t.Errorf("wiki links = %v", got)
	}
}
