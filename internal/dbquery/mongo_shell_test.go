package dbquery

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestIsMongoJSScript(t *testing.T) {
	cases := map[string]bool{
		`db.users.find({ a: 1 })`:                       false, // one plain call: simple path
		`db.users.find({ a: 1 }).limit(5);`:             false,
		`please find users`:                             false, // not JavaScript: keep the syntax help
		"use merchant\ndb.merchants.countDocuments({})": true,
		`const f = { a: 1 }; db.u.find(f)`:              true,
		`db.u.find({}).forEach(d => print(d.Name))`:     true,
		"show collections":                              true,
		`db.x.find(); db.x.drop()`:                      true,
	}
	for q, want := range cases {
		if got := IsMongoJSScript(q); got != want {
			t.Errorf("IsMongoJSScript(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestPrepareScriptShowsTopLevelExpressions(t *testing.T) {
	code, labels, err := prepareScript("use shop\nconst a = 1;\nprint(a)\ndb.x.find({ a })\nif (a) { db.y.find() }")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, `{ db = db.getSiblingDB("shop"); }`) {
		t.Fatalf("use not rewritten: %s", code)
	}
	if len(labels) != 2 || labels[1] != "db.x.find({ a })" {
		t.Fatalf("labels: %q", labels)
	}
	if strings.Contains(code, "__pmShow((db.y") {
		t.Fatal("only top-level expressions are shown")
	}
	if _, _, err := prepareScript("const = ;"); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("syntax error expected, got %v", err)
	}
}

// The two scripts from the reports, against a live server.
const reportAggScript = `use pmtest

const V1 = "af786dbb-634a-4fce-b63e-209bcadf53e6";
const V2 = "d2ffad0a-4d7c-4d78-99c5-94af6d2f0d5f";
const filter = { "AcceptedContracts.ContractId": { $all: [V1, V2] } };

// 1) Dry run + backup
db.merchants.aggregate([
  { $match: filter },
  { $project: { _id: 0, OldMerchantId: 1,
    v1Acceptances: { $filter: { input: "$AcceptedContracts", as: "a", cond: { $eq: ["$$a.ContractId", V1] } } } } }
]).toArray();

db.merchants.countDocuments(filter);`

const reportPrintScript = `// MER-1063 - Step 1 (READ-ONLY)
const DB_NAME = "pmtest";
const SAMPLE_LIMIT = 20;

const merchants = db.getSiblingDB(DB_NAME).getCollection("merchants");

const untrimmedFilter = {
  Name: { $type: "string" },
  $expr: { $ne: ["$Name", { $trim: { input: "$Name" } }] }
};

const statusLabels = { 0: "Pending", 1: "Active", 2: "Suspend", 3: "Rejected" };

const total = merchants.countDocuments(untrimmedFilter);
print(` + "`Database                  : ${DB_NAME}`" + `);
print(` + "`Records to be updated     : ${total}`" + `);

print("\nBy status:");
merchants.aggregate([
  { $match: untrimmedFilter },
  { $group: { _id: "$Status", count: { $sum: 1 } } },
  { $sort: { _id: 1 } }
]).forEach(r => print(` + "`  ${statusLabels[r._id] ?? r._id} (${r._id}) : ${r.count}`" + `));

const checks = merchants.aggregate([
  { $match: untrimmedFilter },
  { $project: { trimmed: { $trim: { input: "$Name" } }, OldMerchantId: 1 } },
  { $group: { _id: null,
      emptyAfterTrim: { $sum: { $cond: [{ $eq: ["$trimmed", ""] }, 1, 0] } },
      missingOldMerchantId: { $sum: { $cond: [{ $gt: ["$OldMerchantId", 0] }, 0, 1] } } } }
]).toArray()[0] ?? { emptyAfterTrim: 0, missingOldMerchantId: 0 };

const collisions = merchants.aggregate([
  { $match: { Name: { $type: "string" } } },
  { $project: { key: { $toLower: { $trim: { input: "$Name" } } }, dirty: { $ne: ["$Name", { $trim: { input: "$Name" } }] } } },
  { $group: { _id: "$key", count: { $sum: 1 }, dirty: { $sum: { $cond: ["$dirty", 1, 0] } } } },
  { $match: { count: { $gt: 1 }, dirty: { $gt: 0 } } },
  { $count: "groups" }
]).toArray()[0]?.groups ?? 0;

print(` + "`  Empty name after trim : ${checks.emptyAfterTrim}`" + `);
print(` + "`  Collides              : ${collisions}`" + `);
print(` + "`  Missing OldMerchantId : ${checks.missingOldMerchantId}`" + `);

merchants.find(untrimmedFilter, { _id: 0, OldMerchantId: 1, Name: 1 })
  .sort({ OldMerchantId: 1 })
  .limit(SAMPLE_LIMIT)
  .forEach(m => print(` + "`  ${m.OldMerchantId} : ${JSON.stringify(m.Name)} -> ${JSON.stringify(m.Name.trim())}`" + `));`

func seedMerchants(t *testing.T, s *mongoSession) {
	t.Helper()
	_, err := s.client.Database("pmtest").Collection("merchants").InsertMany(context.Background(), []any{
		bson.D{{Key: "OldMerchantId", Value: 1}, {Key: "Name", Value: " Acme "}, {Key: "Status", Value: 1},
			{Key: "AcceptedContracts", Value: bson.A{bson.D{{Key: "ContractId", Value: "af786dbb-634a-4fce-b63e-209bcadf53e6"}}, bson.D{{Key: "ContractId", Value: "d2ffad0a-4d7c-4d78-99c5-94af6d2f0d5f"}}}}},
		bson.D{{Key: "OldMerchantId", Value: 2}, {Key: "Name", Value: "acme"}, {Key: "Status", Value: 0},
			{Key: "AcceptedContracts", Value: bson.A{bson.D{{Key: "ContractId", Value: "af786dbb-634a-4fce-b63e-209bcadf53e6"}}}}},
		bson.D{{Key: "OldMerchantId", Value: 3}, {Key: "Name", Value: "Beta Şirket "}, {Key: "Status", Value: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runScript(t *testing.T, s *mongoSession, src string, opts ExecOpts) (*ScriptOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	return s.ExecuteScript(ctx, src, opts)
}

func TestLiveMongoScriptAggregateReport(t *testing.T) {
	s := liveMongo(t)
	seedMerchants(t, s)
	out, err := runScript(t, s, reportAggScript, ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(out.Results))
	}
	_, rows := drain(t, out.Results[0].Cursor)
	if len(rows) != 1 || !strings.HasPrefix(out.Results[0].Label, "db.merchants.aggregate([") {
		t.Fatalf("aggregate: %d rows, label %q", len(rows), out.Results[0].Label)
	}
	_, rows = drain(t, out.Results[1].Cursor)
	if len(rows) != 1 || fmt.Sprint(rows[0][0]) != "1" {
		t.Fatalf("count: %v", rows)
	}
}

func TestLiveMongoScriptPrintReport(t *testing.T) {
	s := liveMongo(t)
	seedMerchants(t, s)
	out, err := runScript(t, s, reportPrintScript, ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(out.Output, "\n")
	for _, want := range []string{
		"Database                  : pmtest",
		"Records to be updated     : 2",
		"  Active (1) : 2",
		"  Empty name after trim : 0",
		"  Collides              : 1",
		`  1 : " Acme " -> "Acme"`,
		`  3 : "Beta Şirket " -> "Beta Şirket"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if len(out.Results) != 0 {
		t.Fatalf("print/forEach statements show nothing, got %d results", len(out.Results))
	}
}

// A read-only caller can't write from a script, whatever the call.
func TestLiveMongoScriptWriteGuard(t *testing.T) {
	s := liveMongo(t)
	seedMerchants(t, s)
	if _, err := runScript(t, s, "print(1)\nfoo.bar()", ExecOpts{}); err == nil || err.Error() != "ReferenceError: foo is not defined (line 2)" {
		t.Fatalf("error message: %v", err)
	}
	for _, src := range []string{
		`db.merchants.deleteMany({ Status: 0 })`,
		`const m = db.merchants; m.updateOne({ OldMerchantId: 1 }, { $set: { Name: "x" } })`,
		`db.merchants.aggregate([{ $match: {} }, { $out: "copy" }]).toArray()`,
		`db.runCommand({ drop: "merchants" })`,
		`db.merchants.drop()`,
	} {
		_, err := runScript(t, s, "print(1);\n"+src, ExecOpts{WriteDenied: "this connection is read-only"})
		if err == nil || !strings.Contains(err.Error(), "blocked: this connection is read-only") || strings.Contains(err.Error(), "native") {
			t.Errorf("%s: want write blocked, got %v", src, err)
		}
	}
	n, _ := s.client.Database("pmtest").Collection("merchants").CountDocuments(context.Background(), bson.D{})
	if n != 3 {
		t.Fatalf("documents changed: %d", n)
	}
	// With write allowed it works, and the empty-filter guard still holds.
	out, err := runScript(t, s, `db.merchants.updateMany({ Status: 0 }, { $set: { Status: 1 } })`, ExecOpts{AllowWrite: true})
	if err != nil || len(out.Results) != 1 {
		t.Fatalf("write: %v", err)
	}
	if _, err := runScript(t, s, `db.merchants.deleteMany({})`, ExecOpts{AllowWrite: true}); err == nil || !strings.Contains(err.Error(), "empty filter") {
		t.Fatalf("empty filter guard: %v", err)
	}
}

// Cursor helpers, BSON types and limits.
func TestLiveMongoScriptCursorAndTypes(t *testing.T) {
	s := liveMongo(t)
	seedMerchants(t, s)
	src := `
const c = db.merchants.find({}, { _id: 0 }).sort({ OldMerchantId: -1 });
let firstId = c.hasNext() ? c.next().OldMerchantId : null;
const names = db.merchants.find().map(m => m.Name.trim()).toArray();
const oid = ObjectId("65f000000000000000000001");
print(firstId, names.length, oid.toString(), typeof oid.getTimestamp().getTime());
db.merchants.findOne({ OldMerchantId: 2 }, { _id: 0, Name: 1 });
db.merchants.distinct("Status");
db.merchants.find({}, { _id: 0, OldMerchantId: 1 }).sort({ OldMerchantId: 1 });
`
	out, err := runScript(t, s, src, ExecOpts{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.Output, "|"); got != "3 3 65f000000000000000000001 number" {
		t.Fatalf("output: %q", got)
	}
	if len(out.Results) != 3 {
		t.Fatalf("results: %d", len(out.Results))
	}
	if _, rows := drain(t, out.Results[2].Cursor); len(rows) != 2 {
		t.Fatalf("a shown cursor is cut at the row limit: %d", len(rows))
	}
	// A runaway loop is stopped by the context.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := s.ExecuteScript(ctx, "while (true) {}", ExecOpts{}); err == nil {
		t.Fatal("infinite loop must be interrupted")
	}
}
