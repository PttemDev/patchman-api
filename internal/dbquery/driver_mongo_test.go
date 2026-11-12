package dbquery

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

func TestMongoURI(t *testing.T) {
	cases := []struct {
		name       string
		cfg        ConnectionConfig
		uri        string
		hosts      []string
		user, pass string
		authSource string
		replicaSet string
	}{
		{
			name:  "single host, default auth source is the database",
			cfg:   ConnectionConfig{Host: "10.0.0.5", Port: 27017, Database: "orders", Username: "app", Password: "secret"},
			uri:   "mongodb://app:secret@10.0.0.5:27017/orders",
			hosts: []string{"10.0.0.5:27017"}, user: "app", pass: "secret", authSource: "orders",
		},
		{
			name: "replica set hosts, own ports kept, options passed",
			cfg: ConnectionConfig{
				Host: "db1, db2:27018,db3", Port: 27017, Database: "orders", Username: "app", Password: "p@ss:w/rd%1 +x",
				Options: map[string]any{"authSource": "admin", "replicaSet": "rs0", "directConnection": false, "ignored": "x"},
			},
			uri:   "mongodb://app:p%40ss%3Aw%2Frd%251%20%2Bx@db1:27017,db2:27018,db3:27017/orders?authSource=admin&replicaSet=rs0&directConnection=false",
			hosts: []string{"db1:27017", "db2:27018", "db3:27017"}, user: "app", pass: "p@ss:w/rd%1 +x", authSource: "admin", replicaSet: "rs0",
		},
		{
			name:  "no credentials, no database",
			cfg:   ConnectionConfig{Host: "[::1]", Port: 27017},
			uri:   "mongodb://[::1]:27017/",
			hosts: []string{"[::1]:27017"},
		},
	}
	for _, c := range []struct {
		name      string
		cfg       ConnectionConfig
		mechanism string
		source    string
		user      string
	}{
		{"ldap", ConnectionConfig{Host: "db", Port: 27017, Database: "orders", Username: "jdoe", Password: "pw",
			Options: map[string]any{"authMechanism": "PLAIN", "authSource": "$external"}}, "PLAIN", "$external", "jdoe"},
		{"aws role, no keys", ConnectionConfig{Host: "db", Port: 27017,
			Options: map[string]any{"authMechanism": "MONGODB-AWS", "authSource": "$external"}}, "MONGODB-AWS", "$external", ""},
	} {
		cs, err := connstring.ParseAndValidate(mongoURI(c.cfg))
		if err != nil {
			t.Errorf("%s: driver rejects %s: %v", c.name, mongoURI(c.cfg), err)
			continue
		}
		if cs.AuthMechanism != c.mechanism || cs.AuthSource != c.source || cs.Username != c.user {
			t.Errorf("%s: mechanism=%q source=%q user=%q", c.name, cs.AuthMechanism, cs.AuthSource, cs.Username)
		}
	}
	for _, c := range cases {
		got := mongoURI(c.cfg)
		if got != c.uri {
			t.Errorf("%s: uri\n got %s\nwant %s", c.name, got, c.uri)
			continue
		}
		cs, err := connstring.ParseAndValidate(got)
		if err != nil {
			t.Errorf("%s: driver rejects %s: %v", c.name, got, err)
			continue
		}
		if !reflect.DeepEqual(cs.Hosts, c.hosts) || cs.Username != c.user || cs.Password != c.pass {
			t.Errorf("%s: parsed hosts=%v user=%q pass=%q", c.name, cs.Hosts, cs.Username, cs.Password)
		}
		if c.user != "" && cs.AuthSource != c.authSource {
			t.Errorf("%s: authSource = %q, want %q", c.name, cs.AuthSource, c.authSource)
		}
		if cs.ReplicaSet != c.replicaSet {
			t.Errorf("%s: replicaSet = %q, want %q", c.name, cs.ReplicaSet, c.replicaSet)
		}
	}
}

func TestMongoWithOptions(t *testing.T) {
	cases := []struct{ uri, want string }{
		{"mongodb://u:p@h1:27017,h2:27017/orders", "mongodb://u:p@h1:27017,h2:27017/orders?authSource=admin&replicaSet=rs0"},
		{"mongodb://u:p@h1:27017", "mongodb://u:p@h1:27017/?authSource=admin&replicaSet=rs0"},
		{"mongodb://u:p@h1/orders?AUTHSOURCE=orders&w=majority", "mongodb://u:p@h1/orders?w=majority&authSource=admin&replicaSet=rs0"},
		{"mongodb+srv://u:p@cluster.example.net/orders?retryWrites=true", "mongodb+srv://u:p@cluster.example.net/orders?retryWrites=true&authSource=admin&replicaSet=rs0"},
	}
	opts := map[string]any{"authSource": "admin", "replicaSet": "rs0", "ignored": "x"}
	for _, c := range cases {
		if got := mongoWithOptions(c.uri, opts); got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.uri, got, c.want)
		}
	}
	if got := mongoWithOptions("mongodb://h/db?authSource=x", nil); got != "mongodb://h/db?authSource=x" {
		t.Errorf("no options changed the uri: %s", got)
	}
}

func TestMongoAdminFallback(t *testing.T) {
	cases := []struct {
		uri, want string
		ok        bool
	}{
		{"mongodb://u:p@h1:27017,h2:27017/wms?replicaSet=rs0", "mongodb://u:p@h1:27017,h2:27017/wms?replicaSet=rs0&authSource=admin", true},
		{"mongodb://u:p@h1:27017/wms", "mongodb://u:p@h1:27017/wms?authSource=admin", true},
		{"mongodb://u:p@h1:27017/wms?authMechanism=SCRAM-SHA-256", "mongodb://u:p@h1:27017/wms?authMechanism=SCRAM-SHA-256&authSource=admin", true},
		{"mongodb://u:p@h1:27017/wms?authSource=wms", "", false},
		{"mongodb://u:p@h1:27017/admin", "", false},
		{"mongodb://u:p@h1:27017/", "", false},
		{"mongodb://h1:27017/wms", "", false},
		{"mongodb://u:p@h1/wms?authMechanism=PLAIN", "", false},
	}
	for _, c := range cases {
		got, ok := mongoAdminFallback(c.uri)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.uri, got, ok, c.want, c.ok)
		}
	}
}

func TestMongoSeedsAndHosts(t *testing.T) {
	uri := "mongodb://u:p%40x@10.0.0.1:27017,10.0.0.2,DB3:27018/wms?replicaSet=rs0"
	if got, want := mongoSeeds(uri), []string{"10.0.0.1:27017", "10.0.0.2:27017", "db3:27018"}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds = %v, want %v", got, want)
	}
	if got := mongoSeeds("mongodb+srv://u:p@cluster.example.net/wms"); got != nil {
		t.Errorf("srv seeds = %v, want none", got)
	}
	if got, want := mongoWithHosts(uri, "10.0.0.2:27017"), "mongodb://u:p%40x@10.0.0.2:27017/wms?replicaSet=rs0"; got != want {
		t.Errorf("with hosts = %s, want %s", got, want)
	}
	if got, want := mongoWithHosts("mongodb://h1,h2", "h2:27017"), "mongodb://h2:27017"; got != want {
		t.Errorf("with hosts = %s, want %s", got, want)
	}
}

func TestParseMongoProjectionAndWrites(t *testing.T) {
	op, err := parseMongoExpr(`db.integrators.find({"IntegratorName": "x"}, { "_id": 1, "IntegratorName": 1 })`)
	if err != nil {
		t.Fatal(err)
	}
	if proj, ok := op.Projection.(bson.D); !ok || len(proj) != 2 || proj[1].Key != "IntegratorName" {
		t.Errorf("find projection = %#v", op.Projection)
	}
	op, err = parseMongoExpr(`db.c.findOne({a: 1}, {secret: 0})`)
	if err != nil || op.Projection == nil {
		t.Errorf("findOne projection = %#v, %v", op.Projection, err)
	}
	op, err = parseMongoExpr(`db.c.updateMany({status: "old"}, {$set: {status: "new"}}, {upsert: true})`)
	if err != nil || len(op.Args) != 3 {
		t.Fatalf("updateMany args = %#v, %v", op.Args, err)
	}
	op, err = parseMongoExpr(`db.c.insertMany([{a: 1}, {a: 2}])`)
	if err != nil {
		t.Fatal(err)
	}
	if docs, ok := op.Args[0].(bson.A); !ok || len(docs) != 2 {
		t.Errorf("insertMany docs = %#v", op.Args[0])
	}
}

func TestMongoRegexLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{ IntegratorName: /claude-test/ }`, `{ IntegratorName: {"$regex": "claude-test"} }`},
		{`{name: /^a,b$/i}`, `{name: {"$regex": "^a,b$", "$options": "i"}}`},
		{`{p: /a\/b[/]c/}`, `{p: {"$regex": "a\\/b[/]c"}}`},
		{`{url: "http://x/y", n: 'a/b/'}`, `{url: "http://x/y", n: 'a/b/'}`},
		{`[{$match: {t: /x/m}}]`, `[{$match: {t: {"$regex": "x", "$options": "m"}}}]`},
	}
	for _, c := range cases {
		if got := mongoRegexLiterals(c.in); got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// The shell query from the report now parses, and the comma inside the
	// pattern doesn't split the arguments.
	op, err := parseMongoExpr(`db.integrators.find({ IntegratorName: /claude-test/ })`)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := op.Filter.(bson.D); !ok || len(d) != 1 {
		t.Errorf("filter = %#v", op.Filter)
	}
	if op, err = parseMongoExpr(`db.c.find({n: /a,b/}, {n: 1})`); err != nil || op.Projection == nil {
		t.Errorf("regex with comma split the args: %#v, %v", op, err)
	}
}

func TestMongoWriteRefusesEmptyFilter(t *testing.T) {
	for _, q := range []string{`db.c.deleteMany({})`, `db.c.updateMany({}, {$set: {a: 1}})`} {
		op, err := parseMongoExpr(q)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runMongoWrite(context.Background(), nil, op); err == nil || !strings.Contains(err.Error(), "empty filter") {
			t.Errorf("%s: err = %v", q, err)
		}
	}
}
