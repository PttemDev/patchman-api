package handlers

import (
	"reflect"
	"testing"
)

func cmds(es []historyEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Command
	}
	return out
}

// zsh extended + plain lines, a multi-line command, a metafied "ı" and repeats:
// newest first, each command once.
func TestZshHistory(t *testing.T) {
	// "ı" is 0xC4 0xB1; zsh stores 0xB1 as 0x83 0x91.
	raw := []byte(": 1700000000:0;ls -la\n: 1700000001:0;echo \xc4\x83\x91\n: 1700000002:0;for i in 1 2; do\\\necho $i\\\ndone\n: 1700000003:0;ls -la\nplain cmd\n")
	got := newestDistinct(parseZshHistory(raw), 10)
	want := []string{"plain cmd", "ls -la", "for i in 1 2; do\necho $i\ndone", "echo ı"}
	if !reflect.DeepEqual(cmds(got), want) {
		t.Fatalf("got %q, want %q", cmds(got), want)
	}
	if got[1].Time == nil || *got[1].Time != 1700000003 {
		t.Fatalf("ls -la should carry its latest time, got %v", got[1].Time)
	}
	if got[0].Time != nil {
		t.Fatal("a plain line has no time")
	}
	if n := len(newestDistinct(parseZshHistory(raw), 2)); n != 2 {
		t.Fatalf("limit: %d", n)
	}
}

func TestBashAndFishHistory(t *testing.T) {
	b := parseBashHistory([]byte("#1700000000\ngit status\nmake\n"))
	if len(b) != 2 || b[0].Time == nil || *b[0].Time != 1700000000 || b[1].Time != nil {
		t.Fatalf("bash: %+v", b)
	}
	f := parseFishHistory([]byte("- cmd: echo a\\nb\n  when: 1700000000\n- cmd: ls\n"))
	if len(f) != 2 || f[0].Command != "echo a\nb" || f[0].Time == nil || f[1].Command != "ls" {
		t.Fatalf("fish: %+v", f)
	}
}
