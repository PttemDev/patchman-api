package qalink

import "testing"

func TestExtractJiraKey(t *testing.T) {
	cases := []struct {
		title, branch, desc string
		keys                []string
		pattern, want       string
	}{
		{"MER-1089 || Test environment MongoDB connection string update", "feature/MER-1089", "", []string{"MER"}, "", "MER-1089"},
		{"TEST MR", "feature/MER-936-p", "", []string{"MER"}, "", "MER-936"},
		{"Resolve \"Feature/\"", "feature/mer-936", "", nil, "", "MER-936"},
		{"[MER-12] fix", "main", "", nil, "", "MER-12"},
		{"bump UTF-8 lib", "deps", "Closes MER-77", []string{"MER"}, "", "MER-77"}, // UTF-8 is not a MER key
		{"OPS-1 and MER-2", "x", "", []string{"MER"}, "", "MER-2"},                 // first allowed project wins
		{"no key here", "feature/cleanup", "", []string{"MER"}, "", ""},
		{"v2MER-5 glued to a word", "", "", []string{"MER"}, "", ""},
		{"see mer-9", "", "", []string{"mer"}, `MER-\d+`, ""}, // custom pattern is case-sensitive
		{"see MER-9", "", "", nil, `MER-\d+`, "MER-9"},
		{"broken pattern MER-3", "", "", nil, `([`, "MER-3"}, // invalid pattern → default
	}
	for _, c := range cases {
		if got := ExtractJiraKey(c.title, c.branch, c.desc, c.keys, c.pattern); got != c.want {
			t.Errorf("ExtractJiraKey(%q, %q, %q, %v, %q) = %q, want %q", c.title, c.branch, c.desc, c.keys, c.pattern, got, c.want)
		}
	}
}
