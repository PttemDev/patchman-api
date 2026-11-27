package dbquery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// UUIDs show as UUID('…') (like mongosh / Compass) everywhere a row goes:
// the table cell, JSON, the column type.
func TestBinaryDisplay(t *testing.T) {
	id := bson.Binary{Subtype: 4, Data: []byte{0xda, 0xa3, 0x3d, 0x29, 0xb1, 0x8f, 0x49, 0x1e, 0x85, 0x87, 0x87, 0x21, 0xeb, 0x63, 0x1a, 0xf7}}
	doc := bson.D{{Key: "_id", Value: id}, {Key: "Legacy", Value: bson.Binary{Subtype: 3, Data: id.Data}},
		{Key: "Nested", Value: bson.D{{Key: "ids", Value: bson.A{id}}}}}
	cols := unionMongoCols([]bson.D{doc})
	row, _ := rowFromOrderedDoc(doc, cols)
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	want := `["UUID('daa33d29-b18f-491e-8587-8721eb631af7')","Binary.createFromBase64('2qM9KbGPSR6Fh4ch62Ma9w==', 3)",{"ids":["UUID('daa33d29-b18f-491e-8587-8721eb631af7')"]}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if cols[0].Type != "uuid" || cols[1].Type != "binData" {
		t.Fatalf("column types: %+v", cols)
	}
}

// What the table shows pastes back into a query on the single-call path.
func TestShellHelpersInQueries(t *testing.T) {
	op, err := parseMongoExpr(`db.merchants.find({ _id: UUID('daa33d29-b18f-491e-8587-8721eb631af7'), o: ObjectId("65f000000000000000000001"), at: { $gt: ISODate('2026-04-08T09:56:04.380Z') }, n: NumberLong(5), b: Binary.createFromBase64('2qM9KbGPSR6Fh4ch62Ma9w==', 3) })`)
	if err != nil {
		t.Fatal(err)
	}
	if !simpleChain(op) || IsMongoJSScript(`db.merchants.find({ _id: UUID('daa33d29-b18f-491e-8587-8721eb631af7') })`) {
		t.Fatal("a find with shell helpers stays on the single-call path")
	}
	f, _ := json.Marshal(op.Filter)
	for _, want := range []string{`"Subtype":4`, `"Subtype":3`} {
		if !strings.Contains(string(f), want) {
			t.Fatalf("filter %s lacks %s", f, want)
		}
	}
	var doc bson.D
	raw, _ := bson.Marshal(op.Filter)
	_ = bson.Unmarshal(raw, &doc)
	if b, ok := doc[0].Value.(bson.Binary); !ok || formatUUID(b.Data) != "daa33d29-b18f-491e-8587-8721eb631af7" {
		t.Fatalf("_id: %#v", doc[0].Value)
	}
	if _, ok := doc[1].Value.(bson.ObjectID); !ok {
		t.Fatalf("o: %#v", doc[1].Value)
	}
}

// Round trip on a live server: a UUID _id shows as UUID('…') and that text,
// pasted into a find or a script, finds the document.
func TestLiveMongoUUIDRoundTrip(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	id := bson.Binary{Subtype: 4, Data: []byte{0xda, 0xa3, 0x3d, 0x29, 0xb1, 0x8f, 0x49, 0x1e, 0x85, 0x87, 0x87, 0x21, 0xeb, 0x63, 0x1a, 0xf7}}
	if _, err := s.client.Database("pmtest").Collection("merchants").InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: id}, {Key: "Url", Value: "eskituruncushopeski"}},
		bson.D{{Key: "Url", Value: "other"}},
	}); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Execute(ctx, `db.merchants.find({ _id: UUID('daa33d29-b18f-491e-8587-8721eb631af7') })`, ExecOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := drain(t, cur)
	if len(rows) != 1 || cols[0].Type != "uuid" {
		t.Fatalf("find by UUID: %d rows, cols %+v", len(rows), cols)
	}
	if b, _ := json.Marshal(rows[0][0]); string(b) != `"UUID('daa33d29-b18f-491e-8587-8721eb631af7')"` {
		t.Fatalf("cell: %s", b)
	}
	out, err := s.ExecuteScript(ctx, `const m = db.merchants.findOne({ _id: UUID("daa33d29-b18f-491e-8587-8721eb631af7") }); print(m._id); print(`+"`${m._id}`"+`); print(m)`, ExecOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"UUID('daa33d29-b18f-491e-8587-8721eb631af7')",
		"daa33d29-b18f-491e-8587-8721eb631af7",
		`{"Url":"eskituruncushopeski","_id":"UUID('daa33d29-b18f-491e-8587-8721eb631af7')"}`,
	}
	if strings.Join(out.Output, "\n") != strings.Join(want, "\n") {
		t.Fatalf("print:\n%s", strings.Join(out.Output, "\n"))
	}
}
