package dbquery

// Google BigQuery over its REST API (no gRPC SDK: keeps the sidecar small).
//
// Every query is dry-run first (free): the dry run tells how many bytes the
// query would scan and what kind of statement it is. Then —
//   - a write / DDL / script runs only with ExecOpts.AllowWrite;
//   - more than ExecOpts.MaxBytes is refused up front, and the real job also
//     carries maximumBytesBilled so Google itself refuses it;
//   - more than ExecOpts.AskBytes needs ExecOpts.CostConfirmed (the user said
//     yes to "this scans X GB").
//
// Auth (connection options.auth):
//   - "service_account" (default): the key JSON in the connection's encrypted
//     password field;
//   - "adc": Application Default Credentials — `gcloud auth
//     application-default login` on the desktop (never on a hosted server);
//   - "credential": a shared Google Cloud credential (gcp_credentials row,
//     options.gcpCredentialId) of the connection's OWNER — service account,
//     gcloud or a linked Google account. Like GKE clusters, everyone the
//     connection is shared with runs queries as that owner's Google identity.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const bigQueryScope = "https://www.googleapis.com/auth/bigquery"

// bigQueryBase is the API root; tests point it at a fake server.
var bigQueryBase = "https://bigquery.googleapis.com/bigquery/v2"

// GCPTokenSource resolves the owner's shared Google Cloud credential to a
// BigQuery token source (auth "credential"); set at startup
// (handlers.GCPHandler.BigQueryTokenSource) — same seam as
// k8sclient.GCPTokenSource, so dbquery never imports handlers. nil = the
// "credential" auth mode can't authenticate.
var GCPTokenSource func(ownerID, credID string) (oauth2.TokenSource, error)

// bigQueryCredentialSource resolves a "credential" connection's token source,
// refusing a missing credential id or a server without Google Cloud.
func bigQueryCredentialSource(c ConnectionConfig) (oauth2.TokenSource, error) {
	credID := BigQueryCredentialID(c)
	if credID == "" {
		return nil, errors.New("pick the Google Cloud credential this BigQuery connection signs in with")
	}
	if GCPTokenSource == nil {
		return nil, errors.New("Google Cloud credentials aren't available on this server")
	}
	return GCPTokenSource(c.OwnerID, credID)
}

// bigQueryHTTPClient builds the authenticated client; tests replace it.
var bigQueryHTTPClient = func(ctx context.Context, c ConnectionConfig) (*http.Client, error) {
	var creds *google.Credentials
	var err error
	if BigQueryAuth(c) == "credential" {
		ts, err := bigQueryCredentialSource(c)
		if err != nil {
			return nil, err
		}
		cl := oauth2.NewClient(context.Background(), oauth2.ReuseTokenSource(nil, ts))
		cl.Timeout = 5 * time.Minute
		return cl, nil
	}
	if BigQueryAuth(c) == "adc" {
		creds, err = google.FindDefaultCredentials(context.Background(), bigQueryScope)
		if err != nil {
			return nil, fmt.Errorf("no gcloud credentials on this computer — run `gcloud auth application-default login`: %w", err)
		}
	} else {
		if strings.TrimSpace(c.Password) == "" {
			return nil, errors.New("service account key is missing")
		}
		creds, err = google.CredentialsFromJSON(context.Background(), []byte(c.Password), bigQueryScope)
		if err != nil {
			return nil, fmt.Errorf("service account key: %w", err)
		}
	}
	cl := oauth2.NewClient(context.Background(), creds.TokenSource)
	cl.Timeout = 5 * time.Minute
	return cl, nil
}

// BigQueryAuth returns the connection's auth mode ("service_account" | "adc" | "credential").
func BigQueryAuth(c ConnectionConfig) string {
	switch v, _ := c.Options["auth"].(string); v {
	case "adc", "credential":
		return v
	}
	return "service_account"
}

// BigQueryCredentialID is the shared Google Cloud credential a "credential"
// connection signs in with.
func BigQueryCredentialID(c ConnectionConfig) string {
	v, _ := c.Options["gcpCredentialId"].(string)
	return strings.TrimSpace(v)
}

// CostConfirmError: the dry run scans more than the "ask first" threshold.
type CostConfirmError struct{ Bytes, AskBytes int64 }

func (e *CostConfirmError) Error() string {
	return fmt.Sprintf("this query will scan %s (above %s) — confirm to run it", FormatBytes(e.Bytes), FormatBytes(e.AskBytes))
}

// CostLimitError: the dry run scans more than the limit; it never runs.
type CostLimitError struct{ Bytes, MaxBytes int64 }

func (e *CostLimitError) Error() string {
	return fmt.Sprintf("this query would scan %s, over the limit of %s — narrow it (filter on the partition column, select fewer columns) or raise the limit", FormatBytes(e.Bytes), FormatBytes(e.MaxBytes))
}

// FormatBytes renders a byte count as B / KB / MB / GB / TB (1024-based).
func FormatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

type bigqueryDriver struct{}

func NewBigQueryDriver() Driver { return &bigqueryDriver{} }

func (bigqueryDriver) Kind() Kind { return KindBigQuery }

type bqSession struct {
	client   *http.Client
	project  string
	location string
}

func (bigqueryDriver) Connect(ctx context.Context, c ConnectionConfig) (Session, error) {
	project := strings.TrimSpace(c.Database)
	if project == "" {
		return nil, errors.New("BigQuery project id is required")
	}
	// A shared credential is resolved (owner + scope checks) even when the
	// HTTP client is swapped for an emulator (BIGQUERY_API_BASE / tests).
	if BigQueryAuth(c) == "credential" {
		if _, err := bigQueryCredentialSource(c); err != nil {
			return nil, err
		}
	}
	client, err := bigQueryHTTPClient(ctx, c)
	if err != nil {
		return nil, err
	}
	loc, _ := c.Options["location"].(string)
	s := &bqSession{client: client, project: project, location: strings.TrimSpace(loc)}
	// Cheap reachability + permission check: list one dataset.
	if _, err := s.get(ctx, "/projects/"+url.PathEscape(project)+"/datasets", url.Values{"maxResults": {"1"}}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *bqSession) Close() error { return nil }

// ── REST helpers ─────────────────────────────────────────────────────────────

type bqAPIError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *bqSession) do(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	u := bigQueryBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bigquery: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode/100 != 2 {
		var e bqAPIError
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("bigquery: %s", e.Error.Message)
		}
		return nil, fmt.Errorf("bigquery: HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func (s *bqSession) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	return s.do(ctx, http.MethodGet, path, q, nil)
}

// ── Execute ──────────────────────────────────────────────────────────────────

type bqField struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Mode   string    `json:"mode"`
	Fields []bqField `json:"fields"`
}

type bqQueryResponse struct {
	JobComplete  bool `json:"jobComplete"`
	JobReference struct {
		JobID    string `json:"jobId"`
		Location string `json:"location"`
	} `json:"jobReference"`
	Schema struct {
		Fields []bqField `json:"fields"`
	} `json:"schema"`
	Rows               []map[string]any `json:"rows"`
	PageToken          string           `json:"pageToken"`
	NumDMLAffectedRows string           `json:"numDmlAffectedRows"`
}

// readStatements are the only statement types a read-only call may run.
var readStatements = map[string]bool{"SELECT": true}

// DryRun returns the bytes a query would scan and its statement type.
func (s *bqSession) DryRun(ctx context.Context, query string) (int64, string, error) {
	body := map[string]any{
		"configuration": map[string]any{
			"dryRun": true,
			"query":  map[string]any{"query": query, "useLegacySql": false},
		},
	}
	if s.location != "" {
		body["jobReference"] = map[string]any{"location": s.location}
	}
	data, err := s.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(s.project)+"/jobs", nil, body)
	if err != nil {
		return 0, "", err
	}
	var job struct {
		Statistics struct {
			TotalBytesProcessed string `json:"totalBytesProcessed"`
			Query               struct {
				StatementType string `json:"statementType"`
			} `json:"query"`
		} `json:"statistics"`
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return 0, "", fmt.Errorf("bigquery dry run: %w", err)
	}
	n, _ := strconv.ParseInt(job.Statistics.TotalBytesProcessed, 10, 64)
	return n, strings.ToUpper(job.Statistics.Query.StatementType), nil
}

func (s *bqSession) Execute(ctx context.Context, query string, opts ExecOpts) (Cursor, error) {
	bytesScanned, stmt, err := s.DryRun(ctx, query)
	if err != nil {
		return nil, err
	}
	if !readStatements[stmt] && !opts.AllowWrite {
		kind := stmt
		if kind == "" {
			kind = "this statement"
		}
		return nil, fmt.Errorf("%s changes data or tables — this connection runs read-only queries (SELECT). Enable read-write on the connection to run it", kind)
	}
	if opts.MaxBytes > 0 && bytesScanned > opts.MaxBytes {
		return nil, &CostLimitError{Bytes: bytesScanned, MaxBytes: opts.MaxBytes}
	}
	if opts.AskBytes > 0 && bytesScanned > opts.AskBytes && !opts.CostConfirmed {
		return nil, &CostConfirmError{Bytes: bytesScanned, AskBytes: opts.AskBytes}
	}

	pageSize := 1000
	if opts.Limit > 0 && opts.Limit < pageSize {
		pageSize = opts.Limit
	}
	req := map[string]any{
		"query":        query,
		"useLegacySql": false,
		"maxResults":   pageSize,
		"timeoutMs":    20000,
	}
	if opts.MaxBytes > 0 {
		req["maximumBytesBilled"] = strconv.FormatInt(opts.MaxBytes, 10)
	}
	if s.location != "" {
		req["location"] = s.location
	}
	data, err := s.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(s.project)+"/queries", nil, req)
	if err != nil {
		return nil, err
	}
	var r bqQueryResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("bigquery: %w", err)
	}
	cur := &bqCursor{s: s, pageSize: pageSize, jobID: r.JobReference.JobID, location: r.JobReference.Location}
	// Long queries: poll until the job is done.
	for !r.JobComplete {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if err := cur.fetch(ctx, "", &r); err != nil {
			return nil, err
		}
	}
	cur.load(r)
	if len(r.Schema.Fields) == 0 && r.NumDMLAffectedRows != "" {
		// A DML statement: report affected rows like the SQL drivers.
		n, _ := strconv.ParseInt(r.NumDMLAffectedRows, 10, 64)
		cur.fields = []bqField{{Name: "affected_rows", Type: "INTEGER"}}
		cur.buf = []Row{{n}}
	}
	return cur, nil
}

type bqCursor struct {
	s         *bqSession
	pageSize  int
	jobID     string
	location  string
	fields    []bqField
	buf       []Row
	pageToken string
}

func (c *bqCursor) load(r bqQueryResponse) {
	if len(r.Schema.Fields) > 0 {
		c.fields = r.Schema.Fields
	}
	c.pageToken = r.PageToken
	c.buf = c.buf[:0]
	for _, row := range r.Rows {
		c.buf = append(c.buf, bqRow(c.fields, row))
	}
}

func (c *bqCursor) fetch(ctx context.Context, token string, into *bqQueryResponse) error {
	q := url.Values{"maxResults": {strconv.Itoa(c.pageSize)}, "timeoutMs": {"20000"}}
	if token != "" {
		q.Set("pageToken", token)
	}
	if c.location != "" {
		q.Set("location", c.location)
	}
	data, err := c.s.get(ctx, "/projects/"+url.PathEscape(c.s.project)+"/queries/"+url.PathEscape(c.jobID), q)
	if err != nil {
		return err
	}
	*into = bqQueryResponse{}
	return json.Unmarshal(data, into)
}

func (c *bqCursor) Columns() []ColumnMeta {
	cols := make([]ColumnMeta, len(c.fields))
	for i, f := range c.fields {
		t := f.Type
		if f.Mode == "REPEATED" {
			t = "ARRAY<" + t + ">"
		}
		cols[i] = ColumnMeta{Name: f.Name, Type: t}
	}
	return cols
}

func (c *bqCursor) Next(ctx context.Context) (Row, error) {
	for len(c.buf) == 0 {
		if c.pageToken == "" {
			return nil, nil
		}
		var r bqQueryResponse
		if err := c.fetch(ctx, c.pageToken, &r); err != nil {
			return nil, err
		}
		c.load(r)
	}
	row := c.buf[0]
	c.buf = c.buf[1:]
	return row, nil
}

func (c *bqCursor) Close() error { return nil }

// ── value conversion ─────────────────────────────────────────────────────────

func bqRow(fields []bqField, row map[string]any) Row {
	cells, _ := row["f"].([]any)
	out := make(Row, len(fields))
	for i, f := range fields {
		if i < len(cells) {
			cell, _ := cells[i].(map[string]any)
			out[i] = bqValue(f, cell["v"])
		}
	}
	return out
}

func bqValue(f bqField, v any) any {
	if v == nil {
		return nil
	}
	if f.Mode == "REPEATED" {
		items, _ := v.([]any)
		out := make([]any, 0, len(items))
		one := f
		one.Mode = ""
		for _, it := range items {
			m, _ := it.(map[string]any)
			out = append(out, bqValue(one, m["v"]))
		}
		return out
	}
	switch f.Type {
	case "RECORD", "STRUCT":
		rec, _ := v.(map[string]any)
		cells, _ := rec["f"].([]any)
		m := make(map[string]any, len(f.Fields))
		for i, sub := range f.Fields {
			if i < len(cells) {
				cell, _ := cells[i].(map[string]any)
				m[sub.Name] = bqValue(sub, cell["v"])
			}
		}
		return m
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch f.Type {
	case "INTEGER", "INT64":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > -(1<<53) && n < 1<<53 {
			return n
		}
		return s // keep big ints exact
	case "FLOAT", "FLOAT64":
		if x, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(x, 0) && !math.IsNaN(x) {
			return x
		}
		return s
	case "BOOLEAN", "BOOL":
		return s == "true"
	case "TIMESTAMP":
		// Seconds since the epoch, as a decimal string.
		if x, err := strconv.ParseFloat(s, 64); err == nil {
			sec := math.Floor(x)
			return time.Unix(int64(sec), int64((x-sec)*1e9)).UTC().Format(time.RFC3339Nano)
		}
		return s
	}
	return s
}

// ── Introspect ───────────────────────────────────────────────────────────────

// introspectTableCap bounds how many tables the schema tree loads; very
// large projects still list every dataset.
const introspectTableCap = 300

func (bigqueryDriver) Introspect(ctx context.Context, sess Session) (Schema, error) {
	s, ok := sess.(*bqSession)
	if !ok {
		return Schema{}, errors.New("bigquery introspect: session type mismatch")
	}
	base := "/projects/" + url.PathEscape(s.project)
	data, err := s.get(ctx, base+"/datasets", url.Values{"maxResults": {"1000"}})
	if err != nil {
		return Schema{}, err
	}
	var ds struct {
		Datasets []struct {
			DatasetReference struct {
				DatasetID string `json:"datasetId"`
			} `json:"datasetReference"`
		} `json:"datasets"`
	}
	_ = json.Unmarshal(data, &ds)
	type ref struct{ dataset, table string }
	var refs []ref
	for _, d := range ds.Datasets {
		if len(refs) >= introspectTableCap {
			break
		}
		id := d.DatasetReference.DatasetID
		td, err := s.get(ctx, base+"/datasets/"+url.PathEscape(id)+"/tables", url.Values{"maxResults": {"1000"}})
		if err != nil {
			continue
		}
		var tl struct {
			Tables []struct {
				TableReference struct {
					TableID string `json:"tableId"`
				} `json:"tableReference"`
			} `json:"tables"`
		}
		_ = json.Unmarshal(td, &tl)
		for _, t := range tl.Tables {
			if len(refs) >= introspectTableCap {
				break
			}
			refs = append(refs, ref{id, t.TableReference.TableID})
		}
	}
	out := make([]SchemaTable, len(refs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, r := range refs {
		wg.Add(1)
		go func(i int, r ref) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = SchemaTable{Schema: r.dataset, Name: r.table}
			data, err := s.get(ctx, base+"/datasets/"+url.PathEscape(r.dataset)+"/tables/"+url.PathEscape(r.table), url.Values{"fields": {"schema"}})
			if err != nil {
				return
			}
			var t struct {
				Schema struct {
					Fields []bqField `json:"fields"`
				} `json:"schema"`
			}
			if json.Unmarshal(data, &t) == nil {
				for _, f := range t.Schema.Fields {
					typ := f.Type
					if f.Mode == "REPEATED" {
						typ = "ARRAY<" + typ + ">"
					}
					out[i].Columns = append(out[i].Columns, ColumnMeta{Name: f.Name, Type: typ})
				}
			}
		}(i, r)
	}
	wg.Wait()
	// Datasets without (loaded) tables still show up.
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.dataset] = true
	}
	for _, d := range ds.Datasets {
		if id := d.DatasetReference.DatasetID; !seen[id] {
			out = append(out, SchemaTable{Schema: id})
		}
	}
	return Schema{Tables: out, UpdatedAt: time.Now().Unix()}, nil
}

// UseBigQueryEndpoint points the driver at another API root with a plain
// HTTP client (tests in other packages, a local emulator). It returns a
// function that restores the real endpoint.
func UseBigQueryEndpoint(base string, client *http.Client) func() {
	oldBase, oldClient := bigQueryBase, bigQueryHTTPClient
	bigQueryBase = base
	bigQueryHTTPClient = func(context.Context, ConnectionConfig) (*http.Client, error) { return client, nil }
	return func() { bigQueryBase, bigQueryHTTPClient = oldBase, oldClient }
}
