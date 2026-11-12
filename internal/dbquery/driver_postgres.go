package dbquery

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver registration
)

// pgDriver speaks Postgres via pgx-as-database/sql so we can share the
// generic sql.Rows → Cursor adapter with the MySQL driver. pgx's
// native pool is bypassed in favour of database/sql's connection
// management (max 5 open conns per Session) — keeps the resource
// budget predictable and uniform across both SQL flavours.
type pgDriver struct{}

var _ = stdlib.GetDefaultDriver // keep import referenced

func NewPostgresDriver() Driver { return &pgDriver{} }

func (pgDriver) Kind() Kind { return KindPostgres }

func (pgDriver) Connect(ctx context.Context, c ConnectionConfig) (Session, error) {
	// Power-user override: paste a full DSN and we use it as-is. This
	// lets the user add params we don't surface as form fields (TLS
	// certs, application_name templating, channel_binding, etc.) without
	// us shipping a knob for every one.
	dsn := normalizePostgresDSN(c.ConnectionString, c)
	if dsn == "" {
		// Build a libpq-style URL. pgx's stdlib driver accepts both
		// keyword/value DSN and URL form; URL is friendlier for tunnels +
		// options later.
		u := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(c.Username, c.Password),
			Host:   fmt.Sprintf("%s:%d", c.Host, c.Port),
			Path:   "/" + c.Database,
		}
		q := u.Query()
		if v, ok := c.Options["sslmode"].(string); ok && v != "" {
			q.Set("sslmode", v)
		} else {
			// Local dev defaults to no TLS; production setups should set
			// sslmode=require explicitly via the connection options panel.
			q.Set("sslmode", "prefer")
		}
		if v, ok := c.Options["application_name"].(string); ok && v != "" {
			q.Set("application_name", v)
		}
		u.RawQuery = q.Encode()
		dsn = u.String()
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &sqlSession{db: db, kind: KindPostgres}, nil
}

func (pgDriver) Introspect(ctx context.Context, sess Session) (Schema, error) {
	ss, ok := sess.(*sqlSession)
	if !ok {
		return Schema{}, fmt.Errorf("postgres introspect: session type mismatch")
	}
	query := `
		SELECT table_schema, table_name, column_name, data_type
		FROM information_schema.columns
		WHERE table_schema NOT IN ('pg_catalog','information_schema')
		ORDER BY table_schema, table_name, ordinal_position
	`
	cur, err := ss.Execute(ctx, query, ExecOpts{Limit: 50000})
	if err != nil {
		return Schema{}, err
	}
	schema, err := buildSQLSchema(ctx, cur)
	cur.Close()
	if err != nil {
		return schema, err
	}
	// FK introspection — best-effort, swallow errors so a permission gap
	// on pg_constraint doesn't break the whole schema response.
	fkCur, fkErr := ss.Execute(ctx, `
		SELECT
		  tc.table_schema,
		  tc.table_name,
		  kcu.column_name,
		  ccu.table_schema AS foreign_table_schema,
		  ccu.table_name   AS foreign_table_name,
		  ccu.column_name  AS foreign_column_name
		FROM information_schema.table_constraints AS tc
		JOIN information_schema.key_column_usage AS kcu
		  ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		JOIN information_schema.constraint_column_usage AS ccu
		  ON ccu.constraint_name = tc.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_schema NOT IN ('pg_catalog','information_schema')
	`, ExecOpts{Limit: 50000})
	if fkErr == nil {
		attachForeignKeys(ctx, &schema, fkCur)
		fkCur.Close()
	}
	return schema, nil
}

// buildSQLSchema groups column rows (table_schema, table_name, column,
// type) into one SchemaTable per (schema, table). Shared between
// Postgres + MySQL because they both return the same shape from
// information_schema.columns.
func buildSQLSchema(ctx context.Context, cur Cursor) (Schema, error) {
	out := Schema{Tables: []SchemaTable{}, UpdatedAt: time.Now().Unix()}
	var current *SchemaTable
	for {
		row, err := cur.Next(ctx)
		if err != nil {
			return out, err
		}
		if row == nil {
			break
		}
		if len(row) < 4 {
			continue
		}
		schemaName := stringify(row[0])
		tableName := stringify(row[1])
		colName := stringify(row[2])
		colType := stringify(row[3])
		if current == nil || current.Schema != schemaName || current.Name != tableName {
			out.Tables = append(out.Tables, SchemaTable{
				Schema:  schemaName,
				Name:    tableName,
				Columns: []ColumnMeta{},
			})
			current = &out.Tables[len(out.Tables)-1]
		}
		current.Columns = append(current.Columns, ColumnMeta{Name: colName, Type: colType})
	}
	return out, nil
}

// attachForeignKeys walks an FK-info cursor and attaches one
// ForeignKey entry to the matching SchemaTable column.
func attachForeignKeys(ctx context.Context, schema *Schema, cur Cursor) {
	for {
		row, err := cur.Next(ctx)
		if err != nil || row == nil {
			return
		}
		if len(row) < 6 {
			continue
		}
		tSchema := stringify(row[0])
		tName := stringify(row[1])
		tCol := stringify(row[2])
		fSchema := stringify(row[3])
		fName := stringify(row[4])
		fCol := stringify(row[5])
		for i := range schema.Tables {
			tbl := &schema.Tables[i]
			if tbl.Schema == tSchema && tbl.Name == tName {
				tbl.ForeignKeys = append(tbl.ForeignKeys, ForeignKey{
					Column:       tCol,
					TargetSchema: fSchema,
					TargetTable:  fName,
					TargetColumn: fCol,
				})
				break
			}
		}
	}
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Shared sql.DB-backed session/cursor used by both Postgres + MySQL.
// ──────────────────────────────────────────────────────────────────────

type sqlSession struct {
	db   *sql.DB
	kind Kind
}

func (s *sqlSession) Execute(ctx context.Context, query string, opts ExecOpts) (Cursor, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	colNames, _ := rows.Columns()
	colTypes, _ := rows.ColumnTypes()
	cols := make([]ColumnMeta, len(colNames))
	for i, n := range colNames {
		t := ""
		if i < len(colTypes) && colTypes[i] != nil {
			t = colTypes[i].DatabaseTypeName()
		}
		cols[i] = ColumnMeta{Name: n, Type: t}
	}
	return &sqlCursor{rows: rows, cols: cols, limit: opts.Limit}, nil
}

func (s *sqlSession) Close() error { return s.db.Close() }

type sqlCursor struct {
	rows  *sql.Rows
	cols  []ColumnMeta
	read  int
	limit int
}

func (c *sqlCursor) Columns() []ColumnMeta { return c.cols }

func (c *sqlCursor) Next(ctx context.Context) (Row, error) {
	if c.limit > 0 && c.read >= c.limit {
		return nil, nil
	}
	if !c.rows.Next() {
		if err := c.rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	dst := make([]any, len(c.cols))
	ptrs := make([]any, len(c.cols))
	for i := range dst {
		ptrs[i] = &dst[i]
	}
	if err := c.rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	// Normalize: []byte → string when it's a human-readable column so JSON
	// encoding doesn't dump base64 for text, JSON, or numeric-as-text types.
	// UNIQUEIDENTIFIER is 16 raw bytes (SQL Server) and needs the GUID codec,
	// not a raw string cast.
	for i, v := range dst {
		if b, ok := v.([]byte); ok {
			tname := strings.ToUpper(c.cols[i].Type)
			switch {
			case tname == "UNIQUEIDENTIFIER":
				if g, ok := mssqlGUID(b); ok {
					dst[i] = g
				}
			case isTextual(tname), isJSONType(tname), isDecimalText(tname):
				dst[i] = string(b)
			}
		}
	}
	c.read++
	return dst, nil
}

func (c *sqlCursor) Close() error { return c.rows.Close() }

func isTextual(t string) bool {
	switch t {
	case "TEXT", "VARCHAR", "CHAR", "CHARACTER VARYING", "CITEXT", "NAME", "UUID",
		// SQL Server unicode text types (go-mssqldb hands these back as []byte
		// otherwise, which would render as base64). UNIQUEIDENTIFIER is NOT
		// here — it's 16 raw bytes and goes through mssqlGUID in the cursor.
		"NVARCHAR", "NCHAR", "NTEXT":
		return true
	}
	return strings.HasPrefix(t, "VARCHAR") || strings.HasPrefix(t, "CHAR") ||
		strings.HasPrefix(t, "NVARCHAR") || strings.HasPrefix(t, "NCHAR")
}

func isJSONType(t string) bool {
	return t == "JSON" || t == "JSONB"
}

// isDecimalText covers exact-numeric types that go-mssqldb (and MySQL) return
// as a []byte holding the ASCII number ("123.45") rather than a float — so
// without this they'd JSON-encode as base64. Kept as strings to preserve
// full precision (a float64 round-trip would corrupt large DECIMAL/MONEY).
func isDecimalText(t string) bool {
	switch t {
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		return true
	}
	return false
}

// pgJDBCParams maps PgJDBC connection properties (lower-cased) to their
// libpq / pgx names. Anything else in a jdbc: string is a Java-driver
// property; pgx would send it to the server as a setting, which rejects it,
// so it is dropped.
var pgJDBCParams = map[string]string{
	"user": "user", "password": "password", "currentschema": "search_path",
	"applicationname": "application_name", "connecttimeout": "connect_timeout",
	"sslmode": "sslmode", "sslrootcert": "sslrootcert", "sslcert": "sslcert", "sslkey": "sslkey",
	"targetservertype": "target_session_attrs", "options": "options",
}

// pgJDBCTargets maps PgJDBC targetServerType values to target_session_attrs.
var pgJDBCTargets = map[string]string{
	"any": "any", "primary": "primary", "master": "primary",
	"secondary": "standby", "slave": "standby", "prefersecondary": "prefer-standby", "preferslave": "prefer-standby",
}

// normalizePostgresDSN makes a pasted connection string dialable by pgx:
//   - jdbc:postgresql://… (Spring / Java configs) loses its prefix and has its
//     properties renamed (currentSchema → search_path, ssl=true →
//     sslmode=require, …);
//   - a URL whose password holds unescaped @ : / % gets its user info
//     percent-encoded;
//   - with no user / password in the string, the dialog's Username and
//     Password fields are used instead of silently falling back to the OS
//     user.
//
// Anything it cannot read is returned unchanged for pgx to report.
func normalizePostgresDSN(dsn string, c ConnectionConfig) string {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return ""
	}
	jdbc := false
	if len(s) > 5 && strings.EqualFold(s[:5], "jdbc:") {
		s, jdbc = s[5:], true
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
		if jdbc {
			return dsn
		}
		return pgKeywordCreds(s, c)
	}
	u, err := url.Parse(s)
	if err != nil {
		fixed, ok := pgEscapeUserinfo(s)
		if !ok {
			return dsn
		}
		if u, err = url.Parse(fixed); err != nil {
			return dsn
		}
	}
	q := u.Query()
	if jdbc {
		out := url.Values{}
		for k, vs := range q {
			key := strings.ToLower(k)
			if len(vs) == 0 {
				continue
			}
			switch {
			case key == "ssl":
				if strings.EqualFold(vs[0], "true") && q.Get("sslmode") == "" {
					out.Set("sslmode", "require")
				}
			case key == "targetservertype":
				if v, ok := pgJDBCTargets[strings.ToLower(vs[0])]; ok {
					out.Set("target_session_attrs", v)
				}
			case pgJDBCParams[key] != "":
				out.Set(pgJDBCParams[key], vs[0])
			}
		}
		q = out
	}
	if q.Get("user") == "" && c.Username != "" && u.User == nil {
		u.User = url.UserPassword(c.Username, c.Password)
	} else if u.User != nil && q.Get("password") == "" && c.Password != "" {
		if _, has := u.User.Password(); !has {
			u.User = url.UserPassword(u.User.Username(), c.Password)
		}
	} else if u.User == nil && q.Get("user") != "" && q.Get("password") == "" && c.Password != "" {
		q.Set("password", c.Password)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// pgEscapeUserinfo percent-encodes the user info of a postgres URL whose
// password was pasted raw: everything before the LAST @ is user[:password].
func pgEscapeUserinfo(s string) (string, bool) {
	scheme, rest, ok := strings.Cut(s, "://")
	at := strings.LastIndex(rest, "@")
	if !ok || at < 0 {
		return "", false
	}
	unescape := func(v string) string {
		if d, err := url.PathUnescape(v); err == nil {
			return d
		}
		return v
	}
	user, pass, hasPass := strings.Cut(rest[:at], ":")
	ui := url.User(unescape(user))
	if hasPass {
		ui = url.UserPassword(unescape(user), unescape(pass))
	}
	return scheme + "://" + ui.String() + "@" + rest[at+1:], true
}

// pgKeywordCreds adds the dialog's Username / Password to a keyword/value
// string ("host=… dbname=…") that names none.
func pgKeywordCreds(s string, c ConnectionConfig) string {
	has := func(key string) bool {
		return regexp.MustCompile(`(^|\s)` + key + `\s*=`).MatchString(s)
	}
	quote := func(v string) string {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
	}
	if c.Username != "" && !has("user") {
		s += " user=" + quote(c.Username)
	}
	if c.Password != "" && !has("password") {
		s += " password=" + quote(c.Password)
	}
	return s
}
