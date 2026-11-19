package dbquery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeBigQuery answers the REST calls the driver makes. dry runs report
// `bytes` and the statement type guessed from the query's first word.
type fakeBigQuery struct {
	mu      sync.Mutex
	bytes   int64
	queries []map[string]any // bodies POSTed to /queries
	polls   int
	pending bool // first /queries answer says jobComplete=false
}

func (f *fakeBigQuery) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/projects/proj/datasets"):
			_, _ = w.Write([]byte(`{"datasets":[{"datasetReference":{"datasetId":"sales"}},{"datasetReference":{"datasetId":"empty"}}]}`))
		case r.Method == "GET" && strings.HasSuffix(p, "/datasets/sales/tables"):
			_, _ = w.Write([]byte(`{"tables":[{"tableReference":{"tableId":"orders"}}]}`))
		case r.Method == "GET" && strings.HasSuffix(p, "/datasets/empty/tables"):
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasSuffix(p, "/datasets/sales/tables/orders"):
			_, _ = w.Write([]byte(`{"schema":{"fields":[{"name":"id","type":"INTEGER"},{"name":"tags","type":"STRING","mode":"REPEATED"}]}}`))
		case r.Method == "POST" && strings.HasSuffix(p, "/projects/proj/jobs"):
			cfg, _ := in["configuration"].(map[string]any)
			q, _ := cfg["query"].(map[string]any)
			text, _ := q["query"].(string)
			stmt := "SELECT"
			if w0 := strings.ToUpper(strings.Fields(text)[0]); w0 == "INSERT" || w0 == "DELETE" || w0 == "CREATE" {
				stmt = map[string]string{"INSERT": "INSERT", "DELETE": "DELETE", "CREATE": "CREATE_TABLE"}[w0]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"statistics": map[string]any{
				"totalBytesProcessed": jsonInt(f.bytes), "query": map[string]any{"statementType": stmt},
			}})
		case r.Method == "POST" && strings.HasSuffix(p, "/projects/proj/queries"):
			f.queries = append(f.queries, in)
			text, _ := in["query"].(string)
			if strings.HasPrefix(strings.ToUpper(text), "INSERT") {
				_, _ = w.Write([]byte(`{"jobComplete":true,"jobReference":{"jobId":"j2"},"numDmlAffectedRows":"3"}`))
				return
			}
			if f.pending {
				_, _ = w.Write([]byte(`{"jobComplete":false,"jobReference":{"jobId":"j1","location":"EU"}}`))
				return
			}
			_, _ = w.Write([]byte(page1))
		case r.Method == "GET" && strings.HasSuffix(p, "/projects/proj/queries/j1"):
			f.polls++
			if r.URL.Query().Get("pageToken") == "p2" {
				_, _ = w.Write([]byte(page2))
				return
			}
			_, _ = w.Write([]byte(page1))
		default:
			t.Errorf("unexpected call %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	})
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

const schemaJSON = `"schema":{"fields":[
 {"name":"id","type":"INTEGER"},{"name":"big","type":"INTEGER"},{"name":"price","type":"FLOAT"},
 {"name":"ok","type":"BOOLEAN"},{"name":"at","type":"TIMESTAMP"},{"name":"tags","type":"STRING","mode":"REPEATED"},
 {"name":"addr","type":"RECORD","fields":[{"name":"city","type":"STRING"}]}]}`

const page1 = `{"jobComplete":true,"jobReference":{"jobId":"j1","location":"EU"},` + schemaJSON + `,"pageToken":"p2","rows":[
 {"f":[{"v":"1"},{"v":"9007199254740993"},{"v":"2.5"},{"v":"true"},{"v":"1700000000.5"},{"v":[{"v":"a"},{"v":"b"}]},{"v":{"f":[{"v":"Ankara"}]}}]}]}`

const page2 = `{"jobComplete":true,"jobReference":{"jobId":"j1"},` + schemaJSON + `,"rows":[
 {"f":[{"v":"2"},{"v":null},{"v":"0"},{"v":"false"},{"v":null},{"v":[]},{"v":null}]}]}`

func newFakeSession(t *testing.T, f *fakeBigQuery) Session {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	oldBase, oldClient := bigQueryBase, bigQueryHTTPClient
	bigQueryBase = srv.URL
	bigQueryHTTPClient = func(context.Context, ConnectionConfig) (*http.Client, error) { return srv.Client(), nil }
	t.Cleanup(func() { bigQueryBase, bigQueryHTTPClient = oldBase, oldClient })
	s, err := NewBigQueryDriver().Connect(context.Background(), ConnectionConfig{Kind: KindBigQuery, Database: "proj", Options: map[string]any{"location": "EU"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bqDrain(t *testing.T, c Cursor) []Row {
	t.Helper()
	var rows []Row
	for {
		r, err := c.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			return rows
		}
		rows = append(rows, r)
	}
}

func TestBigQuerySelectPagingAndTypes(t *testing.T) {
	f := &fakeBigQuery{bytes: 1 << 20}
	s := newFakeSession(t, f)
	cur, err := s.Execute(context.Background(), "SELECT * FROM sales.orders", ExecOpts{MaxBytes: 10 << 30, AskBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	rows := bqDrain(t, cur)
	if len(rows) != 2 {
		t.Fatalf("rows across pages: %d", len(rows))
	}
	r := rows[0]
	if r[0] != int64(1) || r[1] != "9007199254740993" || r[2] != 2.5 || r[3] != true {
		t.Errorf("scalars: %#v", r[:4])
	}
	if r[4] != "2023-11-14T22:13:20.5Z" {
		t.Errorf("timestamp: %v", r[4])
	}
	if tags, _ := r[5].([]any); len(tags) != 2 || tags[0] != "a" {
		t.Errorf("repeated: %#v", r[5])
	}
	if addr, _ := r[6].(map[string]any); addr["city"] != "Ankara" {
		t.Errorf("record: %#v", r[6])
	}
	if rows[1][1] != nil || rows[1][3] != false {
		t.Errorf("nulls / false: %#v", rows[1])
	}
	cols := cur.Columns()
	if cols[5].Type != "ARRAY<STRING>" {
		t.Errorf("repeated column type: %s", cols[5].Type)
	}
	if got := f.queries[0]["maximumBytesBilled"]; got != "10737418240" {
		t.Errorf("maximumBytesBilled = %v", got)
	}
	if f.queries[0]["location"] != "EU" {
		t.Errorf("location not sent")
	}
}

func TestBigQueryCostGuard(t *testing.T) {
	f := &fakeBigQuery{bytes: 5 << 30} // 5 GB
	s := newFakeSession(t, f)
	ctx := context.Background()

	_, err := s.Execute(ctx, "SELECT 1", ExecOpts{MaxBytes: 2 << 30})
	var over *CostLimitError
	if !errors.As(err, &over) || len(f.queries) != 0 {
		t.Fatalf("over the limit must be refused before running: %v (ran %d)", err, len(f.queries))
	}
	_, err = s.Execute(ctx, "SELECT 1", ExecOpts{MaxBytes: 10 << 30, AskBytes: 1 << 30})
	var ask *CostConfirmError
	if !errors.As(err, &ask) || ask.Bytes != 5<<30 || len(f.queries) != 0 {
		t.Fatalf("over the ask threshold must wait for confirmation: %v", err)
	}
	if !strings.Contains(err.Error(), "5.0 GB") {
		t.Errorf("message should show the size: %s", err)
	}
	if _, err = s.Execute(ctx, "SELECT 1", ExecOpts{MaxBytes: 10 << 30, AskBytes: 1 << 30, CostConfirmed: true}); err != nil || len(f.queries) != 1 {
		t.Fatalf("confirmed query must run: %v", err)
	}
}

func TestBigQueryReadOnlyAndDML(t *testing.T) {
	f := &fakeBigQuery{bytes: 100}
	s := newFakeSession(t, f)
	ctx := context.Background()
	for _, q := range []string{"INSERT INTO t VALUES (1)", "DELETE FROM t WHERE true", "CREATE TABLE x (a INT64)"} {
		if _, err := s.Execute(ctx, q, ExecOpts{}); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%q on a read-only call: %v", q, err)
		}
	}
	if len(f.queries) != 0 {
		t.Fatal("a write ran on a read-only call")
	}
	cur, err := s.Execute(ctx, "INSERT INTO t VALUES (1)", ExecOpts{AllowWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	rows := bqDrain(t, cur)
	if len(rows) != 1 || rows[0][0] != int64(3) || cur.Columns()[0].Name != "affected_rows" {
		t.Errorf("DML result: %#v %#v", rows, cur.Columns())
	}
}

func TestBigQueryPollsLongJobs(t *testing.T) {
	f := &fakeBigQuery{bytes: 10, pending: true}
	s := newFakeSession(t, f)
	cur, err := s.Execute(context.Background(), "SELECT 1", ExecOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if rows := bqDrain(t, cur); len(rows) != 2 || f.polls < 2 {
		t.Errorf("poll + page: rows=%d polls=%d", len(rows), f.polls)
	}
}

func TestBigQueryIntrospect(t *testing.T) {
	f := &fakeBigQuery{}
	s := newFakeSession(t, f)
	sc, err := NewBigQueryDriver().Introspect(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Tables) != 2 || sc.Tables[0].Schema != "sales" || sc.Tables[0].Name != "orders" || len(sc.Tables[0].Columns) != 2 || sc.Tables[0].Columns[1].Type != "ARRAY<STRING>" {
		t.Fatalf("schema: %+v", sc.Tables)
	}
	if sc.Tables[1].Schema != "empty" || sc.Tables[1].Name != "" {
		t.Errorf("empty dataset should still be listed: %+v", sc.Tables[1])
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 1536: "1.5 KB", 10 << 30: "10.0 GB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
