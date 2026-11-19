package dbquery

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Live Mongo tests. Run with a throwaway server:
//
//	docker run -d --name patchman-test-mongo -p 127.0.0.1:27118:27017 mongo:7
//	PATCHMAN_TEST_MONGO_URI=mongodb://127.0.0.1:27118 go test ./internal/dbquery -run Live
func liveMongo(t *testing.T) *mongoSession {
	uri := os.Getenv("PATCHMAN_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("PATCHMAN_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	sess, err := NewMongoDriver().Connect(ctx, ConnectionConfig{Kind: KindMongo, Host: u.Hostname(), Port: port, Database: "pmtest"})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*mongoSession)
	_ = s.client.Database("pmtest").Drop(ctx)
	t.Cleanup(func() { _ = s.client.Database("pmtest").Drop(context.Background()); _ = s.Close() })
	return s
}

func drain(t *testing.T, cur Cursor) ([]ColumnMeta, []Row) {
	t.Helper()
	var rows []Row
	for {
		r, err := cur.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			break
		}
		rows = append(rows, r)
	}
	return cur.Columns(), rows
}

// Columns are the union of every document's keys (fields only later
// documents carry used to vanish), and nested docs stay JSON objects.
func TestLiveMongoColumnUnion(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	c := s.client.Database("pmtest").Collection("people")
	_, _ = c.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}, {Key: "email", Value: "b@x"}, {Key: "addr", Value: bson.D{{Key: "city", Value: "Ankara"}}}},
	})
	cur, err := s.Execute(ctx, `db.people.find({}).sort({_id: 1})`, ExecOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := drain(t, cur)
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "_id,name,email,addr" {
		t.Fatalf("columns: %v", names)
	}
	if len(rows) != 2 || rows[1][2] != "b@x" || rows[0][2] != nil {
		t.Fatalf("rows: %v", rows)
	}
	if _, ok := rows[1][3].(bson.M); !ok {
		t.Fatalf("nested doc must be a map, got %T", rows[1][3])
	}
}

func TestLiveMongoExplain(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	_, _ = s.client.Database("pmtest").Collection("orders").InsertOne(ctx, bson.D{{Key: "status", Value: "new"}})
	for _, q := range []string{
		`db.orders.find({status: "new"}).sort({status: 1}).limit(5).explain()`,
		`db.orders.aggregate([{$match: {status: "new"}}]).explain("queryPlanner")`,
	} {
		cur, err := s.Execute(ctx, q, ExecOpts{Limit: 10})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, rows := drain(t, cur)
		if len(cols) != 1 || cols[0].Name != "explain" || len(rows) != 1 {
			t.Fatalf("%s: cols %v rows %d", q, cols, len(rows))
		}
		plan, ok := rows[0][0].(bson.M)
		if !ok || plan["queryPlanner"] == nil && plan["stages"] == nil {
			t.Fatalf("%s: no plan in %v", q, rows[0][0])
		}
	}
}

func TestLiveMongoIndexes(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	c := s.client.Database("pmtest").Collection("events")
	_, _ = c.InsertOne(ctx, bson.D{{Key: "user", Value: "u1"}, {Key: "at", Value: time.Now()}, {Key: "status", Value: "ok"}})

	ttl := int64(3600)
	name, err := s.MongoCreateIndex(ctx, "", "events", MongoIndexSpec{
		Keys:               []MongoIndexKey{{Field: "at", Value: float64(1)}},
		ExpireAfterSeconds: &ttl,
	})
	if err != nil || name != "at_1" {
		t.Fatalf("ttl index: %q %v", name, err)
	}
	if _, err := s.MongoCreateIndex(ctx, "pmtest", "events", MongoIndexSpec{
		Keys:          []MongoIndexKey{{Field: "user", Value: float64(1)}, {Field: "status", Value: float64(-1)}},
		Name:          "user_status",
		Unique:        true,
		PartialFilter: `{ status: "ok" }`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MongoCreateIndex(ctx, "", "events", MongoIndexSpec{Keys: []MongoIndexKey{{Field: "x", Value: "bogus"}}}); err == nil {
		t.Fatal("bogus key type must be refused")
	}
	// Use one index so $indexStats has something to count.
	_ = c.FindOne(ctx, bson.D{{Key: "user", Value: "u1"}, {Key: "status", Value: "ok"}}).Err()

	list, err := s.MongoIndexes(ctx, "", "events")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]MongoIndex{}
	for _, ix := range list {
		by[ix.Name] = ix
	}
	if len(by) != 3 {
		t.Fatalf("want _id_, at_1, user_status: %+v", list)
	}
	if ix := by["at_1"]; ix.ExpireAfterSeconds == nil || *ix.ExpireAfterSeconds != 3600 || ix.Type != "regular" {
		t.Fatalf("at_1: %+v", ix)
	}
	us := by["user_status"]
	if !us.Unique || us.Type != "compound" || us.Partial == nil || len(us.Keys) != 2 || us.Keys[1].Field != "status" {
		t.Fatalf("user_status: %+v", us)
	}
	if us.Size <= 0 || us.Ops < 1 {
		t.Fatalf("stats missing: size %d ops %d", us.Size, us.Ops)
	}

	if err := s.MongoSetIndexHidden(ctx, "", "events", "user_status", true); err != nil {
		t.Fatal(err)
	}
	list, _ = s.MongoIndexes(ctx, "", "events")
	for _, ix := range list {
		if ix.Name == "user_status" && !ix.Hidden {
			t.Fatal("hide did not stick")
		}
	}
	if err := s.MongoDropIndex(ctx, "", "events", "_id_"); err == nil {
		t.Fatal("_id_ must be protected")
	}
	if err := s.MongoDropIndex(ctx, "", "events", "user_status"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.MongoIndexes(ctx, "", "events")
	if len(list) != 2 {
		t.Fatalf("after drop: %+v", list)
	}
}

// Absent columns are reported per row; .hint / .collation are accepted.
func TestLiveMongoAbsentHintCollation(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	c := s.client.Database("pmtest").Collection("names")
	_, _ = c.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "n", Value: "b"}, {Key: "x", Value: nil}},
		bson.D{{Key: "_id", Value: 2}, {Key: "n", Value: "A"}},
	})
	_, _ = s.MongoCreateIndex(ctx, "", "names", MongoIndexSpec{Keys: []MongoIndexKey{{Field: "n", Value: float64(1)}}})
	cur, err := s.Execute(ctx, `db.names.find({}).sort({n: 1}).collation({locale: "en", strength: 1}).hint("n_1")`, ExecOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var absent [][]int
	var names []any
	for {
		r, err := cur.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			break
		}
		names = append(names, r[1])
		absent = append(absent, cur.(AbsentReporter).LastAbsent())
	}
	// collation-aware sort puts "A" before "b"; doc 2 has no x (absent),
	// doc 1 has x explicitly null (present).
	if len(names) != 2 || names[0] != "A" || len(absent[0]) != 1 || len(absent[1]) != 0 {
		t.Fatalf("names %v absent %v", names, absent)
	}
	if _, err := s.Execute(ctx, `db.names.find({}).collation({strength: 1})`, ExecOpts{Limit: 1}); err == nil {
		t.Fatal("collation without locale must be refused")
	}
}

func TestLiveMongoAggregatePreview(t *testing.T) {
	s := liveMongo(t)
	ctx := context.Background()
	c := s.client.Database("pmtest").Collection("sales")
	for i := 1; i <= 30; i++ {
		_, _ = c.InsertOne(ctx, bson.D{{Key: "_id", Value: i}, {Key: "region", Value: []string{"eu", "us"}[i%2]}, {Key: "amount", Value: i * 10}})
	}
	stages := []string{
		`{ $match: { amount: { $gte: 100 } } }`,
		`{ $group: { _id: '$region', total: { $sum: '$amount' } } }`,
		`{ $sort: { total: -1 } }`,
		`{ $out: 'x' }`,
	}
	docs, err := s.MongoAggregatePreview(ctx, "", "sales", stages, 0, 5)
	if err != nil || len(docs) != 5 || !strings.HasPrefix(docs[0], `{"_id":`) {
		t.Fatalf("stage 1: %v %v", docs, err)
	}
	docs, err = s.MongoAggregatePreview(ctx, "", "sales", stages, 2, 20)
	if err != nil || len(docs) != 2 || !strings.Contains(docs[0], `"total"`) {
		t.Fatalf("stage 3: %v %v", docs, err)
	}
	if _, err := s.MongoAggregatePreview(ctx, "", "sales", stages, 3, 20); err == nil {
		t.Fatal("$out must not be previewed")
	}
	if _, err := s.MongoAggregatePreview(ctx, "", "sales", []string{`{ $match: {}, $limit: 1 }`}, 0, 20); err == nil {
		t.Fatal("two-operator stage must be refused")
	}
	if n, _ := s.client.Database("pmtest").ListCollectionNames(ctx, bson.D{{Key: "name", Value: "x"}}); len(n) != 0 {
		t.Fatal("preview must never write")
	}
}
