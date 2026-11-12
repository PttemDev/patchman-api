// Package dbquery is the BE-side adapter layer for the user-facing "DB
// Query" tool: a small interface that all three drivers (Postgres /
// MySQL / Mongo) implement so the handler doesn't have to switch on
// kind at every call site. See DB_QUERY_TOOL_RFC.md for the design
// rationale (intent parser, delegated execution, schema cache, ...).
package dbquery

import (
	"context"
)

// Kind is the enum used in DB rows + JSON payloads.
type Kind string

const (
	KindPostgres  Kind = "postgres"
	KindMySQL     Kind = "mysql"
	KindMongo     Kind = "mongo"
	// Wire-compatible variants — same driver, different default port + UI
	// label so users see "CockroachDB" rather than "Postgres" when they
	// connect to one.
	KindCockroach Kind = "cockroachdb"
	KindYugabyte  Kind = "yugabytedb"
	KindRedshift  Kind = "redshift"
	KindMariaDB   Kind = "mariadb"
	KindTiDB      Kind = "tidb"
	// Microsoft SQL Server / Azure SQL — its own driver (go-mssqldb, T-SQL
	// introspection), not wire-compatible with the Postgres/MySQL families.
	KindSQLServer Kind = "sqlserver"
)

// IsPostgresFamily / IsMySQLFamily lets the registry & drivers fan in
// the aliases onto the same underlying client without hard-coding the
// list at every call site.
func (k Kind) IsPostgresFamily() bool {
	return k == KindPostgres || k == KindCockroach || k == KindYugabyte || k == KindRedshift
}
func (k Kind) IsMySQLFamily() bool {
	return k == KindMySQL || k == KindMariaDB || k == KindTiDB
}

// ConnectionConfig is the resolved (decrypted) DSN-shaped struct passed
// into a Driver. Handlers build this from models.DBConnection + the
// TokenCipher decrypt — keeping it as a value type means a Driver
// never sees the encrypted ciphertext or the DB row directly.
type ConnectionConfig struct {
	Kind     Kind
	Host     string
	Port     int
	Database string
	Username string
	Password string
	// Options is the parsed `options_json` blob. Driver-specific keys:
	//   postgres: sslmode, application_name, search_path
	//   mysql:    tls, parseTime, allowNativePasswords
	//   mongo:    authSource, authMechanism, replicaSet, directConnection
	Options map[string]any
	// ConnectionString lets power users override the auto-built DSN with
	// their own (e.g. to add a parameter we don't surface as a form
	// field). When non-empty, drivers pass it straight to their underlying
	// client and ignore Host/Port/Database/Username/Password/Options.
	ConnectionString string
}

// ColumnMeta is a single column descriptor in a result. Type names are
// driver-specific (Postgres returns "int4", MySQL "BIGINT", Mongo "$$")
// so the FE renders them informationally only — sorting / filtering
// happens client-side on raw row values.
type ColumnMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Row is one result row. Values are heterogeneous (string / number /
// bool / nil / nested map/array for JSON-bearing columns) and we hand
// them to `json.Marshal` as-is.
type Row []any

// ExecOpts is the per-call knob set for Execute. `Limit` is the cap
// after which Cursor.Next returns ErrLimitReached so the caller can
// report rowsTruncated=true; `Timeout` ends the query when the wall
// clock exceeds it (default 60s applied by the handler).
type ExecOpts struct {
	Limit int
}

// Cursor walks the result set lazily so a SELECT * over a 10M row table
// can't OOM the BE — we only hold `Limit` rows of metadata in memory
// at a time. Drivers internally back this with database/sql.Rows /
// mongo.Cursor; Close releases whatever the driver allocated.
type Cursor interface {
	Columns() []ColumnMeta
	Next(ctx context.Context) (Row, error) // nil row + nil err == EOF
	Close() error
}

// Session is a single live connection (or driver client for Mongo).
// Cheap to keep around; the handler pools one per connection_id.
type Session interface {
	Execute(ctx context.Context, query string, opts ExecOpts) (Cursor, error)
	Close() error
}

// SchemaTable is one entry in the introspected schema — used by the
// FE Monaco autocomplete provider.
type SchemaTable struct {
	Schema      string         `json:"schema,omitempty"` // postgres / mysql; empty for mongo
	Name        string         `json:"name"`
	Columns     []ColumnMeta   `json:"columns"`
	ForeignKeys []ForeignKey   `json:"foreignKeys,omitempty"` // SQL only
	Sample      map[string]any `json:"sample,omitempty"`       // mongo only — one document key snapshot
}

// ForeignKey describes a single FK relationship a column participates
// in. Used by the FE for "jump to referenced row" affordances.
type ForeignKey struct {
	Column        string `json:"column"`
	TargetSchema  string `json:"targetSchema,omitempty"`
	TargetTable   string `json:"targetTable"`
	TargetColumn  string `json:"targetColumn"`
}

// Schema is the entire DB metadata snapshot for one connection.
type Schema struct {
	Tables    []SchemaTable `json:"tables"`
	UpdatedAt int64         `json:"updatedAt"` // unix seconds for cache freshness
}

// Driver is the interface every dialect implements. Kept narrow so a
// new driver (clickhouse, mssql, ...) is straightforward to add later.
type Driver interface {
	Kind() Kind
	Connect(ctx context.Context, conn ConnectionConfig) (Session, error)
	Introspect(ctx context.Context, sess Session) (Schema, error)
}
