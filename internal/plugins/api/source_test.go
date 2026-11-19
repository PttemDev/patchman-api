package api

import "testing"

func TestParseSource(t *testing.T) {
	ok := map[string]string{
		"https://github.com/acme/plugins/tree/v1.2.0/statuspage":             "https://raw.githubusercontent.com/acme/plugins/v1.2.0/statuspage",
		"https://github.com/acme/plugins":                                    "https://raw.githubusercontent.com/acme/plugins/HEAD",
		"https://gitlab.corp.local/tools/patchman-plugins/-/tree/main/hello": "https://gitlab.corp.local/tools/patchman-plugins/-/raw/main/hello",
		"https://gitlab.corp.local/a/b/c/-/tree/abc123":                      "https://gitlab.corp.local/a/b/c/-/raw/abc123",
		"https://files.example.com/plugins/hello/":                           "https://files.example.com/plugins/hello",
	}
	for in, want := range ok {
		s, err := parseSource(in, "", false)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := s.(*urlSource).base; got != want {
			t.Errorf("%s: base %s, want %s", in, got, want)
		}
	}
	if s, err := parseSource("/Users/me/plugin", "", false); err != nil || s.kind() != "path" {
		t.Errorf("local path: %v", err)
	}
	for _, in := range []string{"", "relative/dir", "http://github.com/a/b", "https://user:pw@github.com/a/b", "https://github.com/a/b?x=1",
		"https://github.com/a", "https://github.com/a/b/issues/1", "ftp://x/y", "https://gitlab.corp/a/-/tree"} {
		if _, err := parseSource(in, "", false); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	gl, _ := parseSource("https://gitlab.corp/a/b/-/tree/main/x", "tok", false)
	if h := gl.(*urlSource).header; h[0] != "PRIVATE-TOKEN" || h[1] != "tok" {
		t.Errorf("gitlab token header: %v", h)
	}
}
