package dbquery

import (
	"reflect"
	"testing"
)

func TestSplitSQL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single statement no semicolon",
			in:   "SELECT * FROM users",
			want: []string{"SELECT * FROM users"},
		},
		{
			name: "single statement trailing semicolon",
			in:   "SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "two real statements",
			in:   "SELECT 1; SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			// THE BUG: a `;` inside a leading line comment must NOT split,
			// and the comment must not become a bogus empty statement.
			name: "leading line comment with semicolon",
			in:   "-- fetch all users;\nSELECT * FROM users",
			want: []string{"-- fetch all users;\nSELECT * FROM users"},
		},
		{
			name: "trailing line comment with semicolon dropped",
			in:   "SELECT 1; -- LIMIT 10;",
			want: []string{"SELECT 1"},
		},
		{
			name: "comment only input yields nothing",
			in:   "-- just a note\n   ",
			want: nil,
		},
		{
			name: "block comment only input yields nothing",
			in:   "/* nothing here */",
			want: nil,
		},
		{
			name: "block comment containing semicolons does not split",
			in:   "/* a; b; c */ SELECT 1;",
			want: []string{"/* a; b; c */ SELECT 1"},
		},
		{
			name: "semicolon inside single-quoted string does not split",
			in:   "SELECT ';' AS s; SELECT 2",
			want: []string{"SELECT ';' AS s", "SELECT 2"},
		},
		{
			name: "semicolon inside double-quoted identifier does not split",
			in:   `SELECT "a;b" FROM t; SELECT 2`,
			want: []string{`SELECT "a;b" FROM t`, "SELECT 2"},
		},
		{
			name: "dollar-quoted body with semicolons",
			in:   "SELECT $$a;b;c$$ AS x; SELECT 2",
			want: []string{"SELECT $$a;b;c$$ AS x", "SELECT 2"},
		},
		{
			name: "comment between two real statements",
			in:   "SELECT 1;\n-- separator;\nSELECT 2;",
			want: []string{"SELECT 1", "-- separator;\nSELECT 2"},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "only whitespace and semicolons",
			in:   "  ;  ;  ",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitSQL(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitSQL(%q)\n got  = %#v\n want = %#v", tc.in, got, tc.want)
			}
		})
	}
}
