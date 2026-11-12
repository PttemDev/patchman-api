package dbquery

import "strings"

// SplitSQL splits a multi-statement SQL string on top-level `;` while
// respecting single-quoted, double-quoted, and dollar-quoted strings AND
// `--` line comments / `/* ... */` block comments. A `;` that lives inside
// a string or a comment does NOT split the statement.
//
// Fragments that contain only comments and/or whitespace (no executable
// code) are dropped, so a leading or trailing comment can never produce a
// bogus empty statement — which previously surfaced as an extra empty
// result tab in the UI (the `-- foo;` on line 1 was split at its own
// semicolon into a comment-only "statement").
//
// Comment handling matches stripSQLComments in intent.go (`--` runs to end
// of line, `/* ... */` blocks, non-nested) so the executor's statement
// split and the read-only intent guard agree on statement boundaries.
func SplitSQL(q string) []string {
	var stmts []string
	var b strings.Builder
	var (
		inSingle, inDouble     bool
		inLineComment, inBlock bool
		dollarTag              string
		hasCode                bool // fragment carries non-comment, non-whitespace content
		prev                   byte
	)
	flush := func() {
		s := strings.TrimSpace(b.String())
		if s != "" && hasCode {
			stmts = append(stmts, s)
		}
		b.Reset()
		hasCode = false
	}
	for i := 0; i < len(q); i++ {
		c := q[i]

		// ── inside a `--` line comment: consume to end of line ──
		if inLineComment {
			b.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			prev = c
			continue
		}
		// ── inside a `/* */` block comment: consume to closing `*/` ──
		if inBlock {
			b.WriteByte(c)
			if prev == '*' && c == '/' {
				inBlock = false
			}
			prev = c
			continue
		}
		// ── inside a dollar-quoted string ($tag$...$tag$) ──
		if dollarTag != "" {
			b.WriteByte(c)
			if c == '$' {
				endTag := "$" + dollarTag[1:len(dollarTag)-1] + "$"
				if strings.HasPrefix(q[i:], endTag) {
					b.WriteString(endTag[1:])
					i += len(endTag) - 1
					dollarTag = ""
				}
			}
			prev = c
			continue
		}
		// ── inside a single-quoted string ──
		if inSingle {
			b.WriteByte(c)
			if c == '\'' && prev != '\\' {
				inSingle = false
			}
			prev = c
			continue
		}
		// ── inside a double-quoted string / identifier ──
		if inDouble {
			b.WriteByte(c)
			if c == '"' && prev != '\\' {
				inDouble = false
			}
			prev = c
			continue
		}

		// ── normal state ──
		// line comment start: --
		if c == '-' && i+1 < len(q) && q[i+1] == '-' {
			inLineComment = true
			b.WriteByte(c)
			b.WriteByte(q[i+1])
			i++
			prev = q[i]
			continue
		}
		// block comment start: /*
		if c == '/' && i+1 < len(q) && q[i+1] == '*' {
			inBlock = true
			b.WriteByte(c)
			b.WriteByte(q[i+1])
			i++
			prev = 0 // neutral: the '*' we just wrote must not pair as a closing */
			continue
		}
		// dollar-quote start: $tag$ or $$ (Postgres; harmless on MySQL)
		if c == '$' {
			j := i + 1
			for j < len(q) && (q[j] == '_' || isAlnum(q[j])) {
				j++
			}
			if j < len(q) && q[j] == '$' {
				tag := q[i : j+1]
				dollarTag = tag
				b.WriteString(tag)
				i = j
				hasCode = true
				prev = '$'
				continue
			}
		}
		switch {
		case c == '\'':
			inSingle = true
			b.WriteByte(c)
			hasCode = true
		case c == '"':
			inDouble = true
			b.WriteByte(c)
			hasCode = true
		case c == ';':
			flush()
		default:
			b.WriteByte(c)
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				hasCode = true
			}
		}
		prev = c
	}
	flush()
	return stmts
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
