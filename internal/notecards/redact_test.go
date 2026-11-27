package notecards

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := "+DB_PASSWORD=Sup3rS3cret!\n+token := \"glpat-abcdefghijklmnopqrstuv\"\n+url = \"postgres://app:Pa55word9@db:5432/x\"\n+name: string\n+x = \"AKIAABCDEFGHIJKLMNOP\""
	out, n := Redact(in)
	for _, leak := range []string{"Sup3rS3cret", "glpat-abc", "Pa55word9", "AKIAABCD"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %s:\n%s", leak, out)
		}
	}
	if n != 4 || !strings.Contains(out, "name: string") {
		t.Errorf("n=%d out:\n%s", n, out)
	}
	if o, n := Redact("func password() string { return cfg.Password }"); n != 0 || !strings.Contains(o, "cfg.Password") {
		t.Errorf("false positive: %s", o)
	}
}
