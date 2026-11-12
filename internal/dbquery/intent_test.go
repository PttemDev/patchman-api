package dbquery

import (
	"strings"
	"testing"
)

func TestValidateReadOnlyMongo(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantErr string // substring; empty = expect nil
	}{
		{
			name:  "plain find",
			query: `db.users.find({})`,
		},
		{
			name:  "three-dot dbname.coll.find",
			query: `db.merchant.users.find({})`,
		},
		{
			name:  "getSiblingDB find",
			query: `db.getSiblingDB('merchant').users.find({})`,
		},
		{
			name:  "aggregate pipeline",
			query: `db.orders.aggregate([{ $match: { status: 'paid' } }])`,
		},
		{
			name:  "filter passes denylist (executor will reject)",
			query: `db.getSiblingDB('merchant').conversations.filter("$gte", new Date())`,
		},
		{
			name:    "insertOne blocked",
			query:   `db.users.insertOne({ name: 'x' })`,
			wantErr: "write",
		},
		{
			name:    "updateMany blocked",
			query:   `db.users.updateMany({}, { $set: { x: 1 } })`,
			wantErr: "write",
		},
		{
			name:    "drop blocked",
			query:   `db.users.drop()`,
			wantErr: "write",
		},
		{
			name:    "drop blocked via three-dot",
			query:   `db.merchant.users.drop()`,
			wantErr: "write",
		},
		{
			name:    "bulkWrite blocked",
			query:   `db.users.bulkWrite([{ insertOne: { document: {} } }])`,
			wantErr: "write",
		},
		{
			name:    "multi-statement rejected",
			query:   `db.users.find(); db.users.drop()`,
			wantErr: "multi-statement",
		},
		{
			name:    "garbage rejected",
			query:   `please find users`,
			wantErr: "not recognized",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateReadOnlyMongo(c.query)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestParseMongoExprThreeDot(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantDB   string
		wantColl string
		wantOp   string
	}{
		{
			name:     "two-dot uses default db",
			query:    `db.users.find({})`,
			wantDB:   "",
			wantColl: "users",
			wantOp:   "find",
		},
		{
			name:     "three-dot extracts dbname",
			query:    `db.merchant.users.find({})`,
			wantDB:   "merchant",
			wantColl: "users",
			wantOp:   "find",
		},
		{
			name:     "getSiblingDB form",
			query:    `db.getSiblingDB('merchant').users.find({})`,
			wantDB:   "merchant",
			wantColl: "users",
			wantOp:   "find",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op, err := parseMongoExpr(c.query)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if op.Database != c.wantDB {
				t.Errorf("database: got %q want %q", op.Database, c.wantDB)
			}
			if op.Collection != c.wantColl {
				t.Errorf("collection: got %q want %q", op.Collection, c.wantColl)
			}
			if op.Op != c.wantOp {
				t.Errorf("op: got %q want %q", op.Op, c.wantOp)
			}
		})
	}
}

func TestUnsupportedMongoOpHint(t *testing.T) {
	err := unsupportedMongoOpError("filter")
	if err == nil || !strings.Contains(err.Error(), "find") {
		t.Fatalf("filter error should mention find(), got: %v", err)
	}
}

func TestIsReadOnlySQL_QuotedKeywords(t *testing.T) {
	readOnly := []string{
		`SELECT id, "order", "update" FROM t`,             // pg quoted keyword columns
		"SELECT `set`, `delete` FROM t",                   // mysql backtick keyword columns
		`SELECT * FROM t WHERE note = 'please delete me'`, // keyword inside a string literal
		`SELECT 1 AS "insert into x"`,                     // keyword-shaped alias
		`WITH x AS (SELECT 1) SELECT * FROM x`,            // plain CTE
	}
	for _, q := range readOnly {
		if !IsReadOnlySQL(q) {
			t.Errorf("expected read-only: %q", q)
		}
	}
	writes := []string{
		`UPDATE t SET x = 1`,
		`DELETE FROM t`,
		`INSERT INTO t VALUES (1)`,
		`SELECT 1; DELETE FROM t`, // multi-statement write
		`WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x`, // data-modifying CTE
		`DROP TABLE t`,
	}
	for _, q := range writes {
		if IsReadOnlySQL(q) {
			t.Errorf("expected rejected (write): %q", q)
		}
	}
}

// TestIsWriteStatement asserts the classifier behind the AllowWrite gate.
// The security-critical rows are the structural ones: a Mongo
// multi-statement `;` and an unrecognized shape must classify as NON-write
// (false) so the execute handler routes them to ValidateReadOnly's
// unconditional rejection instead of the AllowWrite bypass. "What
// ValidateReadOnly rejects as a write" and "what IsWriteStatement treats as
// a write" share mongoFirstOp, so they can never diverge.
func TestIsWriteStatement(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		query string
		want  bool
	}{
		{"sql select is not a write", "postgres", `SELECT * FROM t`, false},
		{"sql multi-statement drop is a write", "postgres", `SELECT 1; DROP TABLE t`, true},
		{"sql update is a write", "mysql", `UPDATE t SET x = 1`, true},
		{"mongo find is not a write", "mongo", `db.x.find({})`, false},
		{"mongo deleteMany is a write", "mongo", `db.x.deleteMany({})`, true},
		// Structural: multi-statement `;` is NOT a write — it must stay
		// ValidateReadOnly's rejection so AllowWrite can never bypass it,
		// even though the second statement is a drop().
		{"mongo multi-statement is structural not write", "mongo", `db.x.find(); db.x.drop()`, false},
		{"mongo garbage is structural not write", "mongo", `please find users`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsWriteStatement(c.kind, c.query); got != c.want {
				t.Fatalf("IsWriteStatement(%q, %q) = %v, want %v", c.kind, c.query, got, c.want)
			}
		})
	}
}

// EXPLAIN on a read-only connection: a plain EXPLAIN only plans (even of a
// write, which never runs); EXPLAIN ANALYZE executes the statement, so it is
// read-only only when the explained statement is.
func TestIsReadOnlySQL_Explain(t *testing.T) {
	for _, q := range []string{
		`EXPLAIN SELECT * FROM orders`,
		`explain analyze select * from orders where id = 1`,
		`EXPLAIN ANALYSE SELECT 1`,
		"EXPLAIN (FORMAT JSON, ANALYZE)\nSELECT *\nFROM \"orders\"\nWHERE status = 'paid'\n  AND placed_at >= NOW() - INTERVAL '7 days';",
		`EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT YAML) WITH x AS (SELECT 1) SELECT * FROM x`,
		`EXPLAIN ANALYZE VERBOSE SELECT 1`,
		`explain format=json select * from t`,                           // MySQL
		`EXPLAIN FORMAT = TREE SELECT * FROM t`,                         // MySQL
		`explain format='brief' select * from t`,                        // TiDB
		`EXPLAIN UPDATE orders SET status = 'x'`,                        // plan only — never runs
		`EXPLAIN DELETE FROM orders WHERE id = 1`,                       // plan only
		`EXPLAIN WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d`, // plan only
		`EXPLAIN SELECT 1; SELECT 2`,
		`/* plan */ EXPLAIN -- why
		 ANALYZE SELECT 1`,
	} {
		if !IsReadOnlySQL(q) {
			t.Errorf("should be read-only: %q", q)
		}
	}
	for _, q := range []string{
		`EXPLAIN ANALYZE UPDATE orders SET status = 'x'`,
		`EXPLAIN (ANALYZE) DELETE FROM orders`,
		`EXPLAIN (FORMAT JSON, ANALYZE TRUE) INSERT INTO t VALUES (1)`,
		`EXPLAIN ANALYZE WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d`,
		`EXPLAIN ANALYZE SELECT * INTO t2 FROM t1`,
		`EXPLAIN SELECT 1; DELETE FROM t`,
		`EXPLAIN`,
		`EXPLAIN EXECUTE stmt`,
		`ANALYZE orders`,                        // the stats command, not EXPLAIN
		`SELECT * INTO new_t FROM t`,            // creates a table
		`SELECT * FROM t INTO OUTFILE '/tmp/x'`, // MySQL writes a file
	} {
		if IsReadOnlySQL(q) {
			t.Errorf("should NOT be read-only: %q", q)
		}
	}
	// EXPLAIN ANALYZE of a write is a write: it goes through the read-write gate.
	if !IsWriteStatement("postgres", `EXPLAIN ANALYZE UPDATE t SET a = 1`) {
		t.Error("EXPLAIN ANALYZE UPDATE must classify as a write")
	}
	if IsWriteStatement("postgres", `EXPLAIN ANALYZE SELECT 1`) {
		t.Error("EXPLAIN ANALYZE SELECT must not classify as a write")
	}
	if err := ValidateReadOnly("mysql", "EXPLAIN ANALYZE SELECT * FROM t"); err != nil {
		t.Errorf("mysql EXPLAIN ANALYZE SELECT rejected: %v", err)
	}
}
