package dbquery

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql driver registration
)

// mysqlDriver shares the `database/sql` + sqlCursor adapter with the
// Postgres driver (see driver_postgres.go). Only the DSN format + the
// introspection query differ between the two.
type mysqlDriver struct{}

func NewMySQLDriver() Driver { return &mysqlDriver{} }

func (mysqlDriver) Kind() Kind { return KindMySQL }

func (mysqlDriver) Connect(ctx context.Context, c ConnectionConfig) (Session, error) {
	dsn := c.ConnectionString
	if dsn == "" {
		// go-sql-driver DSN: user:pass@tcp(host:port)/db?param=value
		params := url.Values{}
		params.Set("parseTime", "true")
		params.Set("multiStatements", "false") // hard-stop on `;` injections
		if v, ok := c.Options["tls"].(string); ok && v != "" {
			params.Set("tls", v)
		}
		if v, ok := c.Options["allowNativePasswords"].(bool); ok {
			if v {
				params.Set("allowNativePasswords", "true")
			} else {
				params.Set("allowNativePasswords", "false")
			}
		}
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
			c.Username, c.Password, c.Host, c.Port, c.Database, params.Encode())
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql ping: %w", err)
	}
	return &sqlSession{db: db, kind: KindMySQL}, nil
}

func (mysqlDriver) Introspect(ctx context.Context, sess Session) (Schema, error) {
	ss, ok := sess.(*sqlSession)
	if !ok {
		return Schema{}, fmt.Errorf("mysql introspect: session type mismatch")
	}
	// When the connection bound to a specific database (DSN has /db), scope
	// to that schema only. When it left the database blank ("browse all
	// databases"), list every user schema so the sidebar can group them by
	// DB; system schemas are excluded so a normal user isn't shown catalog
	// tables they can't read. This is how a single MySQL/StarRocks
	// connection browses all of its databases at once.
	//
	// NB: "no default database" is COALESCE(DATABASE(),'') = '' — StarRocks
	// returns an empty string (not NULL) from DATABASE() when none is
	// selected, so an `IS NULL` test silently matches nothing there.
	query := `
		SELECT table_schema, table_name, column_name, column_type
		FROM information_schema.columns
		WHERE (COALESCE(DATABASE(), '') <> '' AND table_schema = DATABASE())
		   OR (COALESCE(DATABASE(), '') =  '' AND table_schema NOT IN
		        ('information_schema', 'performance_schema', 'mysql', 'sys', '_statistics_'))
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
	// FK introspection — best-effort.
	fkCur, fkErr := ss.Execute(ctx, `
		SELECT
		  kcu.table_schema,
		  kcu.table_name,
		  kcu.column_name,
		  kcu.referenced_table_schema,
		  kcu.referenced_table_name,
		  kcu.referenced_column_name
		FROM information_schema.key_column_usage kcu
		WHERE kcu.table_schema = DATABASE()
		  AND kcu.referenced_table_name IS NOT NULL
	`, ExecOpts{Limit: 50000})
	if fkErr == nil {
		attachForeignKeys(ctx, &schema, fkCur)
		fkCur.Close()
	}
	return schema, nil
}

// `strings` is used by isTextual/isJSONType in driver_postgres.go (same
// package); referencing it here keeps the import linker happy if those
// helpers are inlined.
var _ = strings.HasPrefix
