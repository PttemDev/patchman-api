package dbquery

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidateReadOnly returns nil when `q` is an accepted read-only call
// for the given driver kind, or a user-facing error explaining why it
// was rejected. The handler uses this in place of the boolean check so
// surfaces "syntax not recognized" vs "this op writes data" separately.
func ValidateReadOnly(kind, q string) error {
	switch kind {
	case "postgres", "mysql", "cockroachdb", "yugabytedb", "redshift", "mariadb", "tidb", "sqlserver":
		if IsReadOnlySQL(q) {
			return nil
		}
		return fmt.Errorf("only read-only SQL statements are accepted — SELECT, WITH, EXPLAIN (EXPLAIN ANALYZE only of a read), SHOW, DESCRIBE")
	case "mongo":
		return ValidateReadOnlyMongo(q)
	}
	return fmt.Errorf("unknown driver kind: %s", kind)
}

// IsReadOnly reports whether q is a provably read-only statement for the
// given driver kind. Unlike ValidateReadOnly it returns a plain bool with
// no error messaging — the execute handler uses it (via IsWriteStatement)
// to CLASSIFY a statement for the per-connection AllowWrite gate rather
// than to reject it outright.
func IsReadOnly(kind, q string) bool {
	switch kind {
	case "postgres", "mysql", "cockroachdb", "yugabytedb", "redshift", "mariadb", "tidb", "sqlserver":
		return IsReadOnlySQL(q)
	case "mongo":
		return IsReadOnlyMongo(q)
	}
	return false
}

// IsWriteStatement reports whether q is a WELL-FORMED statement that
// mutates data for the given kind. It is the classifier behind the
// per-connection AllowWrite gate: only a true result routes through the
// "AllowWrite must be on AND the caller can manage the connection" check
// in the execute handler.
//
// Crucially it returns false for structurally-invalid input — a syntax
// error, an unrecognized Mongo shape, or a multi-statement Mongo `;`
// injection. Those are NOT writes: ValidateReadOnly still owns rejecting
// them, and must reject them regardless of AllowWrite. The handler pattern
// is therefore: run ValidateReadOnly first; if it errors, route the
// statement through the AllowWrite gate ONLY when IsWriteStatement is true,
// otherwise surface ValidateReadOnly's precise structural error. This
// guarantees AllowWrite can never bypass the injection / multi-statement
// guard.
func IsWriteStatement(kind, q string) bool {
	switch kind {
	case "postgres", "mysql", "cockroachdb", "yugabytedb", "redshift", "mariadb", "tidb", "sqlserver":
		// For SQL there is no separate structural-error branch — the
		// conservative guard refuses anything it can't classify as
		// SELECT-class, and multi-statement SQL is a supported feature — so
		// "not provably read-only" is exactly "treat as a write".
		return !IsReadOnlySQL(q)
	case "mongo":
		return isWriteMongo(q)
	}
	return false
}

// isWriteMongo reports whether q is a well-formed Mongo call whose op
// mutates data. Structural problems (empty, multi-statement `;`,
// unrecognized syntax) return false — they are not writes and stay
// ValidateReadOnlyMongo's job to reject. Uses the same mongoFirstOp +
// forbiddenMongoOps as the validator so the two never diverge on what
// counts as a write.
func isWriteMongo(q string) bool {
	trimmed := strings.TrimSpace(q)
	if trimmed == "" || hasUnquotedSemicolon(trimmed) {
		return false
	}
	op, ok := mongoFirstOp(trimmed)
	if !ok {
		return false
	}
	_, isWrite := forbiddenMongoOps[op]
	return isWrite
}

// IsReadOnlySQL returns true when `q` looks like a SELECT-class
// statement that can't mutate state. It's deliberately a conservative
// regex check (no proper SQL AST parser) — we'd rather refuse a legit
// query than let DML slip through under a delegated-execution grant.
// The conservative trade-off + an "Override read-only check" owner
// toggle (audit-logged) cover the false-positive escape hatch.
//
// Algorithm:
//  1. Strip block + line comments, normalize whitespace, lowercase
//  2. Reject if any forbidden DML/DDL keyword shows up as a bare word
//  3. Split on `;`, require every statement to start with one of the
//     allow-listed verbs (SELECT, WITH...SELECT, EXPLAIN, SHOW, DESC*)
//
// String literals + quoted identifiers are NOT respected — a column
// named "delete_user" is fine because we match `\bdelete\b` boundaries
// and `delete_user` is one word, but `'delete user' = name` would
// reject the query. v2 swaps this for an AST-based parser when we open
// up write-delegated grants.
func IsReadOnlySQL(q string) bool {
	clean := stripSQLComments(q)
	clean = strings.ToLower(strings.TrimSpace(clean))
	if clean == "" {
		return false
	}
	// Split into individual statements. `strings.Split` is too eager
	// for `;` inside string literals, but combined with the keyword
	// allow-list below it stays safe — a `;` inside a string would
	// only hurt if the rest of the "statement" still started with an
	// allowed verb, in which case the query is read-only anyway.
	for _, stmt := range strings.Split(clean, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if !isReadOnlyStatement(stmt) {
			return false
		}
	}
	return true
}

// isReadOnlyStatement checks one comment-stripped, lower-cased statement.
// The forbidden-keyword scan runs with string literals and quoted
// identifiers blanked out, so a column named "update" / `set` / `order`, or
// a literal 'delete me', can't trip the DML guard. Bare (unquoted) DML
// keywords — including data-modifying CTEs like WITH x AS (DELETE ...) —
// are still caught. The verb check runs on the statement itself
// (statements begin with a bare verb, never a quoted identifier).
func isReadOnlyStatement(stmt string) bool {
	bare := strings.TrimSpace(stripSQLStringsAndIdents(stmt))
	if explainHeadRE.MatchString(bare) {
		return isReadOnlyExplain(bare[len("explain"):])
	}
	return !hasForbiddenSQL(bare) && startsWithReadVerb(stmt)
}

var (
	explainHeadRE = regexp.MustCompile(`^explain\b`)
	// The first statement verb after EXPLAIN starts the explained statement;
	// everything before it is EXPLAIN's own options (Postgres `(ANALYZE,
	// FORMAT JSON)` / `ANALYZE VERBOSE`, MySQL `FORMAT=JSON`, …).
	explainedVerbRE = regexp.MustCompile(`\b(select|with|values|table|insert|update|delete|merge|replace)\b`)
	// Option text may only be words, numbers, `=`, `,` and parentheses.
	explainOptionsRE = regexp.MustCompile(`^[\sa-z0-9_=,()]*$`)
	explainAnalyzeRE = regexp.MustCompile(`\banaly[sz]e\b`)
)

// isReadOnlyExplain decides an EXPLAIN statement (rest = text after the
// keyword). A plain EXPLAIN only plans — the explained statement never runs,
// so even `EXPLAIN UPDATE …` is safe on a read-only connection and shows the
// plan of a write before anyone runs it. EXPLAIN ANALYZE (Postgres, MySQL,
// CockroachDB, TiDB) EXECUTES the statement to time it, so then the explained
// statement must itself be read-only — otherwise it counts as a write and
// goes through the read-write gate like any other DML.
func isReadOnlyExplain(rest string) bool {
	loc := explainedVerbRE.FindStringIndex(rest)
	if loc == nil {
		return false
	}
	options, inner := rest[:loc[0]], rest[loc[0]:]
	if !explainOptionsRE.MatchString(options) {
		return false
	}
	if explainAnalyzeRE.MatchString(options) {
		return !hasForbiddenSQL(inner) && startsWithReadVerb(inner)
	}
	return true
}

// `into` catches SELECT … INTO, which creates a table (Postgres, SQL
// Server) or writes a server file (MySQL INTO OUTFILE/DUMPFILE) — not a read.
var forbiddenSQLRE = regexp.MustCompile(`\b(insert|update|delete|drop|alter|truncate|create|grant|revoke|copy|vacuum|analyze|rename|lock|set|do|merge|into|comment\s+on)\b`)

func hasForbiddenSQL(s string) bool {
	return forbiddenSQLRE.MatchString(s)
}

var readVerbRE = regexp.MustCompile(`^(select|with\s+|explain|show|describe|desc|values|table\s)`)

func startsWithReadVerb(s string) bool {
	return readVerbRE.MatchString(s)
}

// stripSQLStringsAndIdents blanks out single-quoted strings, double-quoted
// and backtick-quoted identifiers, and dollar-quoted bodies (each replaced
// with a single space). Used ONLY for the forbidden-keyword scan so a
// keyword-shaped column name or a string literal that happens to contain
// "update"/"delete"/etc. doesn't read as DML. Input is expected to already
// be comment-stripped + lowercased.
func stripSQLStringsAndIdents(s string) string {
	var b strings.Builder
	var (
		inSingle, inDouble, inBack bool
		dollar                     string
		prev                       byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if dollar != "" {
			if c == '$' && strings.HasPrefix(s[i:], dollar) {
				i += len(dollar) - 1
				dollar = ""
			}
			prev = c
			continue
		}
		if inSingle {
			if c == '\'' && prev != '\\' {
				inSingle = false
			}
			prev = c
			continue
		}
		if inDouble {
			if c == '"' && prev != '\\' {
				inDouble = false
			}
			prev = c
			continue
		}
		if inBack {
			if c == '`' {
				inBack = false
			}
			prev = c
			continue
		}
		switch {
		case c == '\'':
			inSingle = true
			b.WriteByte(' ')
		case c == '"':
			inDouble = true
			b.WriteByte(' ')
		case c == '`':
			inBack = true
			b.WriteByte(' ')
		case c == '$':
			j := i + 1
			for j < len(s) && (s[j] == '_' || isAlnum(s[j])) {
				j++
			}
			if j < len(s) && s[j] == '$' {
				dollar = s[i : j+1]
				i = j
				b.WriteByte(' ')
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
		prev = c
	}
	return b.String()
}

// stripSQLComments removes `--`-to-EOL and `/* ... */` blocks. Not
// state-machine perfect (a `--` inside a string literal is still
// dropped) but good enough for the keyword scanner below.
func stripSQLComments(s string) string {
	// /* ... */ blocks first so a "--" inside one doesn't confuse the
	// line-comment regex.
	blockRE := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = blockRE.ReplaceAllString(s, " ")
	lineRE := regexp.MustCompile(`(?m)--.*$`)
	return lineRE.ReplaceAllString(s, " ")
}

// IsReadOnlyMongo accepts a JS-ish Mongo expression and returns true
// when it's NOT a known write op. Patterns we recognise:
//   db.<coll>.<op>(...)
//   db.<dbname>.<coll>.<op>(...)              ← shorthand for getSiblingDB
//   db.getCollection('coll').<op>(...)
//   db.getSiblingDB('name').<coll>.<op>(...)
//   db.getSiblingDB('name').getCollection('coll').<op>(...)
// Multi-statement input is rejected because Mongo CLI sessions accept
// arbitrary JS — we don't want a `; db.<coll>.drop()` shoved after a find.
//
// Policy: instead of an op allowlist, we run a denylist of writes. The
// driver still only implements a fixed set of read ops, so the safety
// net is layered — validator blocks writes early, driver rejects
// anything it can't actually execute with a "did you mean…" hint.
var mongoFirstCallRE = regexp.MustCompile(`(?s)^\s*db\.([a-zA-Z0-9_\-]+)\.([a-zA-Z]+)\s*\(`)
var mongoFirstThreeDotRE = regexp.MustCompile(`(?s)^\s*db\.([a-zA-Z0-9_\-]+)\.([a-zA-Z0-9_\-.]+)\.([a-zA-Z]+)\s*\(`)
var mongoFirstSiblingCallRE = regexp.MustCompile(`(?s)^\s*db\.getSiblingDB\(\s*['"][^'"]+['"]\s*\)\.([a-zA-Z0-9_\-.]+)\.([a-zA-Z]+)\s*\(`)
var mongoFirstGetCollRE = regexp.MustCompile(`(?s)^\s*db\.(?:getSiblingDB\(\s*['"][^'"]+['"]\s*\)\.)?getCollection\(\s*['"][^'"]+['"]\s*\)\.([a-zA-Z]+)\s*\(`)

// Hints for common syntax mistakes — used by mongoSyntaxHint to
// produce a "did you mean…" pointer instead of a generic rejection.
var mongoLooksLikeGetCollChainRE = regexp.MustCompile(`^\s*db\.getCollection\([^)]+\)\.\w+\.\w+\s*\(`)

// forbiddenMongoOps blocks every write op the driver could plausibly
// execute. The driver's executor still has the final say (it only
// implements read ops), but this layer surfaces the rejection
// pre-execution with a clear "this writes" message instead of a
// generic "unsupported op". Keep names lowercased.
var forbiddenMongoOps = map[string]string{
	"insert":               "insert",
	"insertone":            "insertOne",
	"insertmany":           "insertMany",
	"update":               "update",
	"updateone":            "updateOne",
	"updatemany":           "updateMany",
	"replaceone":           "replaceOne",
	"save":                 "save",
	"delete":               "delete",
	"deleteone":            "deleteOne",
	"deletemany":           "deleteMany",
	"remove":               "remove",
	"findandmodify":        "findAndModify",
	"findoneandupdate":     "findOneAndUpdate",
	"findoneandreplace":    "findOneAndReplace",
	"findoneanddelete":     "findOneAndDelete",
	"bulkwrite":            "bulkWrite",
	"drop":                 "drop",
	"dropdatabase":         "dropDatabase",
	"dropindex":            "dropIndex",
	"dropindexes":          "dropIndexes",
	"createcollection":     "createCollection",
	"createindex":          "createIndex",
	"createindexes":        "createIndexes",
	"renamecollection":     "renameCollection",
	"mapreduce":            "mapReduce", // `out:` can write — block to be safe
	"copyto":               "copyTo",
	"eval":                 "eval",
}

func IsReadOnlyMongo(q string) bool {
	return ValidateReadOnlyMongo(q) == nil
}

// ValidateReadOnlyMongo returns nil when `q` doesn't look like a known
// write op, or an error describing why it was rejected. The error
// messages are user-facing — they're surfaced in the FE error toast,
// so they prioritise actionable hints over precision.
func ValidateReadOnlyMongo(q string) error {
	trimmed := strings.TrimSpace(q)
	if trimmed == "" {
		return fmt.Errorf("empty query")
	}
	if hasUnquotedSemicolon(trimmed) {
		return fmt.Errorf("multi-statement Mongo queries are not supported — drop the semicolons and chain methods instead (e.g. db.x.find().limit(10))")
	}

	matchedOp, ok := mongoFirstOp(trimmed)
	if !ok {
		return fmt.Errorf("query syntax not recognized.\n\nSupported forms:\n  db.<collection>.<op>(...)\n  db.<dbname>.<collection>.<op>(...)\n  db.getSiblingDB('dbname').<collection>.<op>(...)\n  db.getCollection('collection-name').<op>(...)%s", mongoSyntaxHint(trimmed))
	}

	if canon, ok := forbiddenMongoOps[matchedOp]; ok {
		return fmt.Errorf("operation '%s' is a write — blocked.\n\nThis tool is read-only; only queries that don't mutate data are allowed.", canon)
	}
	return nil
}

// mongoFirstOp extracts the lowercased operation name from the first
// db.<...>.<op>( call in a recognized Mongo shape. ok=false means the input
// didn't match any supported form (a structural problem, NOT a write). It
// is the single source of truth for "which op is this call", shared by
// ValidateReadOnlyMongo (rejection messaging) and isWriteMongo (the
// AllowWrite gate) so the two can never disagree on what counts as a write.
// Input is expected pre-trimmed.
func mongoFirstOp(trimmed string) (string, bool) {
	if m := mongoFirstGetCollRE.FindStringSubmatch(trimmed); m != nil {
		return strings.ToLower(m[1]), true
	}
	if m := mongoFirstSiblingCallRE.FindStringSubmatch(trimmed); m != nil {
		return strings.ToLower(m[2]), true
	}
	if m := mongoFirstThreeDotRE.FindStringSubmatch(trimmed); m != nil {
		return strings.ToLower(m[3]), true
	}
	if m := mongoFirstCallRE.FindStringSubmatch(trimmed); m != nil {
		return strings.ToLower(m[2]), true
	}
	return "", false
}

// mongoSyntaxHint produces a "did you mean…" addendum when the user's
// query matches a recognisable wrong-shape pattern.
func mongoSyntaxHint(q string) string {
	if mongoLooksLikeGetCollChainRE.MatchString(q) {
		return "\n\nNote: getCollection returns a collection. To switch databases, use db.getSiblingDB('dbname').<collection>.<op>(...) instead."
	}
	return ""
}

// hasUnquotedSemicolon walks the string and reports a `;` that isn't
// inside `'...'` / `"..."` / template-literal-style spans. Used to
// veto multi-statement Mongo input.
func hasUnquotedSemicolon(s string) bool {
	var inSingle, inDouble, inBackTick bool
	var prev rune
	for _, r := range s {
		switch {
		case r == '\\' && prev != '\\':
			// next char is escaped — fall through to next iteration
		case r == '\'' && !inDouble && !inBackTick && prev != '\\':
			inSingle = !inSingle
		case r == '"' && !inSingle && !inBackTick && prev != '\\':
			inDouble = !inDouble
		case r == '`' && !inSingle && !inDouble && prev != '\\':
			inBackTick = !inBackTick
		case r == ';' && !inSingle && !inDouble && !inBackTick:
			return true
		}
		prev = r
	}
	return false
}
