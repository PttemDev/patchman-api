package dbquery

import (
	"database/sql"
	"net/url"
	"testing"
)

// TestBuildSQLServerDSNEncodesCredentials proves the two things a compile
// can't: the DSN round-trips through url.Parse with special characters in
// the password intact, and go-mssqldb's "sqlserver" driver name is actually
// registered (sql.Open validates the driver but makes no connection).
func TestBuildSQLServerDSNEncodesCredentials(t *testing.T) {
	dsn := buildSQLServerDSN(ConnectionConfig{
		Kind:     KindSQLServer,
		Host:     "db.internal",
		Port:     1433,
		Database: "Sales",
		Username: "sa",
		Password: "p@ss/w:rd?x",
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("dsn does not parse: %v (dsn=%q)", err, dsn)
	}
	if u.Scheme != "sqlserver" {
		t.Fatalf("scheme = %q, want sqlserver", u.Scheme)
	}
	if got, _ := u.User.Password(); got != "p@ss/w:rd?x" {
		t.Fatalf("password round-trip = %q", got)
	}
	if got := u.Query().Get("database"); got != "Sales" {
		t.Fatalf("database = %q, want Sales", got)
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("sql.Open(sqlserver): %v", err)
	}
	_ = db.Close()
}

func TestBuildSQLServerDSNPassthrough(t *testing.T) {
	raw := "sqlserver://u:p@h:1433?database=d&encrypt=strict"
	if got := buildSQLServerDSN(ConnectionConfig{ConnectionString: raw}); got != raw {
		t.Fatalf("passthrough = %q, want %q", got, raw)
	}
}

func TestBuildSQLServerDSNOptions(t *testing.T) {
	dsn := buildSQLServerDSN(ConnectionConfig{
		Host: "h", Port: 1433, Username: "u", Password: "p",
		Options: map[string]any{"encrypt": "true", "trustServerCertificate": true},
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("dsn does not parse: %v", err)
	}
	if got := u.Query().Get("encrypt"); got != "true" {
		t.Fatalf("encrypt = %q, want true", got)
	}
	if got := u.Query().Get("trustservercertificate"); got != "true" {
		t.Fatalf("trustservercertificate = %q, want true", got)
	}
}
