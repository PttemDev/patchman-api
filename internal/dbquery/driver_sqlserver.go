package dbquery

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"time"

	mssql "github.com/microsoft/go-mssqldb" // driver registration ("sqlserver") + UniqueIdentifier codec
)

// sqlserverDriver speaks Microsoft SQL Server / Azure SQL through the
// go-mssqldb database/sql driver, reusing the same sql.Rows → Cursor
// adapter as Postgres + MySQL (see driver_postgres.go). Only the DSN
// format and the T-SQL introspection queries differ.
type sqlserverDriver struct{}

func NewSQLServerDriver() Driver { return &sqlserverDriver{} }

func (sqlserverDriver) Kind() Kind { return KindSQLServer }

// buildSQLServerDSN assembles a go-mssqldb URL DSN. Split out from Connect
// so it's unit-testable without a live server — credentials containing
// @ / : ? are percent-encoded by url.URL.
//
// Encryption: we deliberately DON'T inject `encrypt` when the user hasn't
// asked for it. go-mssqldb v1.x then negotiates EncryptionOff *and* trusts
// the server certificate, so a connection to an internal SQL Server with a
// self-signed cert succeeds out of the box. A security-conscious user opts
// into validation with options {"encrypt":"true"} (or "strict"), adding
// {"trustServerCertificate":true} when the cert is self-signed.
func buildSQLServerDSN(c ConnectionConfig) string {
	if c.ConnectionString != "" {
		return c.ConnectionString
	}
	u := url.URL{
		Scheme: "sqlserver",
		User:   url.UserPassword(c.Username, c.Password),
		Host:   fmt.Sprintf("%s:%d", c.Host, c.Port),
	}
	q := u.Query()
	if c.Database != "" {
		q.Set("database", c.Database)
	}
	if v, ok := c.Options["encrypt"].(string); ok && v != "" {
		q.Set("encrypt", v)
	}
	if v, ok := c.Options["trustServerCertificate"]; ok {
		switch t := v.(type) {
		case bool:
			q.Set("trustservercertificate", strconv.FormatBool(t))
		case string:
			if t != "" {
				q.Set("trustservercertificate", t)
			}
		}
	}
	if v, ok := c.Options["app name"].(string); ok && v != "" {
		q.Set("app name", v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (sqlserverDriver) Connect(ctx context.Context, c ConnectionConfig) (Session, error) {
	dsn := buildSQLServerDSN(c)

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlserver open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlserver ping: %w", err)
	}
	return &sqlSession{db: db, kind: KindSQLServer}, nil
}

func (sqlserverDriver) Introspect(ctx context.Context, sess Session) (Schema, error) {
	ss, ok := sess.(*sqlSession)
	if !ok {
		return Schema{}, fmt.Errorf("sqlserver introspect: session type mismatch")
	}
	// INFORMATION_SCHEMA.COLUMNS is per-database in SQL Server, so this is
	// scoped to the connection's current DB (the one named in the DSN).
	// System schemas are excluded so a normal user isn't shown catalog
	// objects they can't read. Same 4-col shape as Postgres/MySQL, so
	// buildSQLSchema handles it unchanged.
	query := `
		SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, DATA_TYPE
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA NOT IN ('sys', 'INFORMATION_SCHEMA')
		ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION
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
	// FK introspection — best-effort. Unlike MySQL, SQL Server's
	// INFORMATION_SCHEMA.KEY_COLUMN_USAGE doesn't expose the *referenced*
	// column, so we go through the sys.* catalog views (parent + referenced
	// sides both joined to sys.schemas/tables/columns) to produce the 6-col
	// shape attachForeignKeys expects.
	fkCur, fkErr := ss.Execute(ctx, `
		SELECT
		  sch.name  AS table_schema,
		  tab.name  AS table_name,
		  col.name  AS column_name,
		  rsch.name AS referenced_schema,
		  rtab.name AS referenced_table,
		  rcol.name AS referenced_column
		FROM sys.foreign_key_columns AS fkc
		JOIN sys.tables  AS tab  ON tab.object_id  = fkc.parent_object_id
		JOIN sys.schemas AS sch  ON sch.schema_id  = tab.schema_id
		JOIN sys.columns AS col  ON col.object_id  = fkc.parent_object_id     AND col.column_id  = fkc.parent_column_id
		JOIN sys.tables  AS rtab ON rtab.object_id = fkc.referenced_object_id
		JOIN sys.schemas AS rsch ON rsch.schema_id = rtab.schema_id
		JOIN sys.columns AS rcol ON rcol.object_id = fkc.referenced_object_id AND rcol.column_id = fkc.referenced_column_id
	`, ExecOpts{Limit: 50000})
	if fkErr == nil {
		attachForeignKeys(ctx, &schema, fkCur)
		fkCur.Close()
	}
	return schema, nil
}

// mssqlGUID converts the 16 raw bytes go-mssqldb hands back for a
// UNIQUEIDENTIFIER column into its canonical hyphenated string. Scanning
// those bytes as text (string(b)) would emit binary garbage, so the cursor
// routes UNIQUEIDENTIFIER through here instead of isTextual. Returns false
// (leave the value as-is) if the shape isn't the expected 16 bytes.
func mssqlGUID(b []byte) (string, bool) {
	if len(b) != 16 {
		return "", false
	}
	var u mssql.UniqueIdentifier
	if err := u.Scan(b); err != nil {
		return "", false
	}
	return u.String(), true
}
