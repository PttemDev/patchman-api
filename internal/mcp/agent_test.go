package mcp

import "testing"

func TestAgentName(t *testing.T) {
	cases := []struct{ name, title, want string }{
		{"claude-code", "", "Claude Code"},
		{"cursor-vscode", "", "Cursor"},
		{"codex-mcp-client", "", "Codex"},
		{"anything", "Acme Assistant", "Acme Assistant"},
		{"acme-agent-mcp-client", "", "Acme Agent"},
		{"Visual Studio Code", "", "Visual Studio Code"},
		{"  ", "", ""},
		{"evil\x00name\n", "", "Evilname"},
	}
	for _, c := range cases {
		if got := AgentName(c.name, c.title); got != c.want {
			t.Errorf("AgentName(%q, %q) = %q, want %q", c.name, c.title, got, c.want)
		}
	}
}

func TestRememberAnswer(t *testing.T) {
	for in, want := range map[string]string{
		`{"data":{"status":"done","project":"getman","outcome":"created","card":{"short":"#abcd1234","title":"Release order"}}}`: `Saved #abcd1234 "Release order" in project getman.`,
		`{"data":{"status":"done","outcome":"duplicate","card":{"short":"#abcd1234","title":"X"}}}`:                              `Already remembered as #abcd1234 "X" — nothing new saved.`,
		`{"data":{"status":"pending","queued":true}}`:                                                                            "Queued — the AI provider is splitting it into cards; recall finds them in a few seconds.",
		`{"data":{"queued":"#1234abcd","inboxId":"x"}}`:                                                                          `{"data":{"queued":"#1234abcd","inboxId":"x"}}`, // an older backend: unchanged
	} {
		if got := rememberAnswer(in); got != want {
			t.Errorf("rememberAnswer(%s)\n got %q\nwant %q", in, got, want)
		}
	}
}
