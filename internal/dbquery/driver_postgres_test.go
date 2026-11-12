package dbquery

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every pasted form must reach pgx with the right user, password, database
// and settings — checked with pgx's own parser (no network).
func TestNormalizePostgresDSN(t *testing.T) {
	form := ConnectionConfig{Username: "app", Password: "p@ss:w/rd%1"}
	cases := []struct {
		name, dsn            string
		cfg                  ConnectionConfig
		user, pass, db, path string
		host                 string
		sslRequired          bool
	}{
		{name: "encoded url", dsn: "postgres://app:p%40ss%3Aw%2Frd%251@db1:5432/wms", user: "app", pass: "p@ss:w/rd%1", db: "wms", host: "db1"},
		{name: "raw password in url", dsn: "postgres://app:p@ss:w/rd%1@db1:5432/wms", user: "app", pass: "p@ss:w/rd%1", db: "wms", host: "db1"},
		{name: "jdbc with properties", dsn: "jdbc:postgresql://db1:5432/wms?currentSchema=inventory&user=app&password=p%40ss&ssl=true&ApplicationName=svc&prepareThreshold=0",
			user: "app", pass: "p@ss", db: "wms", path: "inventory", host: "db1", sslRequired: true},
		{name: "jdbc, credentials in the form", dsn: "jdbc:postgresql://db1:5432/wms", cfg: form, user: "app", pass: "p@ss:w/rd%1", db: "wms", host: "db1"},
		{name: "url without credentials, form has them", dsn: "postgresql://db1/wms?sslmode=disable", cfg: form, user: "app", pass: "p@ss:w/rd%1", db: "wms", host: "db1"},
		{name: "url user only, form password", dsn: "postgres://app@db1/wms", cfg: ConnectionConfig{Password: "secret"}, user: "app", pass: "secret", db: "wms", host: "db1"},
		{name: "string credentials win over the form", dsn: "postgres://other:pw@db1/wms", cfg: form, user: "other", pass: "pw", db: "wms", host: "db1"},
		{name: "keyword/value, form credentials", dsn: "host=db1 port=5432 dbname=wms", cfg: ConnectionConfig{Username: "app", Password: `it's\here`}, user: "app", pass: `it's\here`, db: "wms", host: "db1"},
		{name: "keyword/value with its own user", dsn: "host=db1 dbname=wms user=x password=y", cfg: form, user: "x", pass: "y", db: "wms", host: "db1"},
	}
	for _, c := range cases {
		got := normalizePostgresDSN(c.dsn, c.cfg)
		pc, err := pgconn.ParseConfig(got)
		if err != nil {
			t.Errorf("%s: pgx rejects %q: %v", c.name, got, err)
			continue
		}
		if pc.User != c.user || pc.Password != c.pass || pc.Database != c.db || pc.Host != c.host {
			t.Errorf("%s: user=%q pass=%q db=%q host=%q (from %q)", c.name, pc.User, pc.Password, pc.Database, pc.Host, got)
		}
		if c.path != "" && pc.RuntimeParams["search_path"] != c.path {
			t.Errorf("%s: search_path=%q", c.name, pc.RuntimeParams["search_path"])
		}
		if _, leaked := pc.RuntimeParams["prepareThreshold"]; leaked {
			t.Errorf("%s: a JDBC-only property reached the server settings", c.name)
		}
		if c.sslRequired && pc.TLSConfig == nil {
			t.Errorf("%s: ssl=true did not require TLS", c.name)
		}
	}
	if got := normalizePostgresDSN("", form); got != "" {
		t.Errorf("empty dsn = %q", got)
	}
}
