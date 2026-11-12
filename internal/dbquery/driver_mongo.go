package dbquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoDriver wraps the official mongo-driver v2 client. Unlike SQL,
// Mongo queries are JS-shaped — we parse a tiny subset of the shell
// syntax (db.<coll>.<op>({...})) into a driver call, intentionally
// limited to the read-only operations the intent parser allows.
type mongoDriver struct{}

func NewMongoDriver() Driver { return &mongoDriver{} }

func (mongoDriver) Kind() Kind { return KindMongo }

func (mongoDriver) Connect(ctx context.Context, c ConnectionConfig) (Session, error) {
	uri := c.ConnectionString
	if uri == "" {
		uri = mongoURI(c)
	} else {
		uri = mongoWithOptions(uri, c.Options)
	}

	client, info, err := mongoDialDiscover(ctx, uri)
	// The auth source defaults to the database in the URI, but users usually
	// live in admin: when only that implicit default failed, try admin once.
	if err != nil && strings.Contains(err.Error(), "AuthenticationFailed") {
		if alt, ok := mongoAdminFallback(uri); ok {
			if c2, info2, err2 := mongoDialDiscover(ctx, alt); err2 == nil {
				client, info, err = c2, info2, nil
				info.AuthSource = "admin"
			}
		}
	}
	if err != nil {
		return nil, err
	}
	// dbName is the connection's "default" database. Empty = no default,
	// every query must come in qualified (db.getSiblingDB(...) or
	// connection-config-bound).
	var defaultDB *mongo.Database
	if c.Database != "" {
		defaultDB = client.Database(c.Database)
	}
	return &mongoSession{client: client, db: defaultDB, defaultDBName: c.Database, info: info}, nil
}

// errUnresolvableMembers: the seeds answered, but the replica set announces
// its members under names this machine cannot resolve (internal host names),
// so the driver can never reach them.
type errUnresolvableMembers struct{ members []string }

func (e *errUnresolvableMembers) Error() string {
	return "the replica set announces members this backend cannot resolve: " + strings.Join(e.members, ", ")
}

// mongoDial connects and pings. It gives up early, with
// errUnresolvableMembers, once every server the driver switched to is a name
// outside the seed list that does not resolve — instead of waiting out the
// server selection timeout.
func mongoDial(ctx context.Context, uri string) (*mongo.Client, error) {
	seeds := map[string]bool{}
	for _, h := range mongoSeeds(uri) {
		seeds[h] = true
	}
	unresolved := make(chan []string, 1)
	var once sync.Once
	mon := &event.ServerMonitor{
		TopologyDescriptionChanged: func(e *event.TopologyDescriptionChangedEvent) {
			servers := e.NewDescription.Servers
			if len(seeds) == 0 || len(servers) == 0 {
				return
			}
			var foreign []string
			for _, sd := range servers {
				if a := strings.ToLower(sd.Addr.String()); !seeds[a] {
					foreign = append(foreign, a)
				}
			}
			if len(foreign) != len(servers) {
				return
			}
			once.Do(func() {
				go func() {
					if noneResolve(foreign) {
						unresolved <- foreign
					}
				}()
			})
		},
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerMonitor(mon))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Ping(pingCtx, nil) }()
	select {
	case err = <-done:
		if err != nil {
			err = fmt.Errorf("mongo ping: %w", err)
		}
	case members := <-unresolved:
		cancel()
		<-done
		err = &errUnresolvableMembers{members: members}
	}
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return client, nil
}

// noneResolve reports whether none of the host:port addresses resolves.
func noneResolve(addrs []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a
		}
		if net.ParseIP(host) != nil {
			return false
		}
		if ips, err := net.DefaultResolver.LookupHost(ctx, host); err == nil && len(ips) > 0 {
			return false
		}
	}
	return true
}

// mongoDialDiscover dials the URI; when the replica set's member names do
// not resolve here it connects to one seed directly instead (what Compass
// calls "Direct connection"), preferring the primary.
func mongoDialDiscover(ctx context.Context, uri string) (*mongo.Client, TestInfo, error) {
	client, err := mongoDial(ctx, uri)
	var un *errUnresolvableMembers
	if !errors.As(err, &un) {
		return client, TestInfo{}, err
	}
	c, host, primary, err := mongoDialDirect(ctx, uri)
	if err != nil {
		return nil, TestInfo{}, fmt.Errorf("%s; connecting to a seed directly failed too: %w", un.Error(), err)
	}
	return c, TestInfo{DirectHost: host, DirectPrimary: primary, Members: un.members}, nil
}

// mongoDialDirect connects to each seed of the URI on its own
// (directConnection=true) and keeps the writable primary, or else the
// first seed that answers.
func mongoDialDirect(ctx context.Context, uri string) (*mongo.Client, string, bool, error) {
	var firstErr error
	var spare *mongo.Client
	spareHost := ""
	for _, h := range mongoSeeds(uri) {
		c, err := mongoDial(ctx, mongoWithOptions(mongoWithHosts(uri, h), map[string]any{"directConnection": true}))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var hello struct {
			IsWritablePrimary bool `bson:"isWritablePrimary"`
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = c.Database("admin").RunCommand(hctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
		cancel()
		if hello.IsWritablePrimary {
			if spare != nil {
				_ = spare.Disconnect(context.Background())
			}
			return c, h, true, nil
		}
		if spare == nil {
			spare, spareHost = c, h
		} else {
			_ = c.Disconnect(context.Background())
		}
	}
	if spare != nil {
		return spare, spareHost, false, nil
	}
	if firstErr == nil {
		firstErr = errors.New("no seed hosts")
	}
	return nil, "", false, firstErr
}

// mongoSeeds lists a mongodb:// URI's hosts as lower-case host:port (27017
// when unset). Empty for mongodb+srv, whose hosts come from DNS.
func mongoSeeds(uri string) []string {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || !strings.EqualFold(scheme, "mongodb") {
		return nil
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority = rest[:i]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	var out []string
	for _, h := range strings.Split(authority, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if !hostHasPort(h) {
			h = net.JoinHostPort(strings.Trim(h, "[]"), "27017")
		}
		out = append(out, h)
	}
	return out
}

// mongoWithHosts swaps a URI's host list for host, keeping the user info,
// database and options.
func mongoWithHosts(uri, host string) string {
	scheme, rest, _ := strings.Cut(uri, "://")
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo = authority[:at+1]
	}
	return scheme + "://" + userinfo + host + tail
}

// mongoAdminFallback returns the URI with authSource=admin when its auth
// source is only the implicit default: credentials, a database other than
// admin, no authSource and no non-SCRAM mechanism. Parsed by hand —
// connstring would resolve mongodb+srv over DNS.
func mongoAdminFallback(uri string) (string, bool) {
	_, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return "", false
	}
	rest, query, _ := strings.Cut(rest, "?")
	authority, path, _ := strings.Cut(rest, "/")
	if !strings.Contains(authority, "@") {
		return "", false
	}
	db, err := url.QueryUnescape(path)
	if err != nil || db == "" || db == "admin" {
		return "", false
	}
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "authsource":
			return "", false
		case "authmechanism":
			if !strings.HasPrefix(strings.ToUpper(v), "SCRAM") {
				return "", false
			}
		}
	}
	return mongoWithOptions(uri, map[string]any{"authSource": "admin"}), true
}

// mongoURIOptions are the connection options a form-mode connection may
// carry in Options and that go into the URI's query, in this order.
var mongoURIOptions = []string{
	"authSource", "authMechanism", "authMechanismProperties", "replicaSet",
	"directConnection", "loadBalanced", "tls", "ssl", "tlsInsecure",
	"tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames", "readPreference",
	"retryWrites", "retryReads", "appName", "compressors", "maxPoolSize",
	"connectTimeoutMS", "serverSelectionTimeoutMS", "socketTimeoutMS",
}

// mongoURI builds the URI for a connection entered as form fields. Host may
// list several hosts (a replica set), comma-separated, each with its own
// port or falling back to Port. Credentials and option values are
// percent-encoded, so passwords with @ : / % or spaces survive.
func mongoURI(c ConnectionConfig) string {
	var hosts []string
	for _, h := range strings.Split(c.Host, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !hostHasPort(h) && c.Port > 0 {
			h = net.JoinHostPort(strings.Trim(h, "[]"), strconv.Itoa(c.Port))
		}
		hosts = append(hosts, h)
	}
	auth := ""
	if c.Username != "" {
		auth = mongoEscape(c.Username)
		if c.Password != "" {
			auth += ":" + mongoEscape(c.Password)
		}
		auth += "@"
	}
	uri := "mongodb://" + auth + strings.Join(hosts, ",") + "/" + url.QueryEscape(c.Database)
	if q := mongoOptionParams(c.Options); len(q) > 0 {
		uri += "?" + strings.Join(q, "&")
	}
	return uri
}

// mongoOptionParams renders the known URI options set in Options as
// escaped key=value pairs, in mongoURIOptions order.
func mongoOptionParams(opts map[string]any) []string {
	var q []string
	for _, k := range mongoURIOptions {
		var v string
		switch x := opts[k].(type) {
		case string:
			v = strings.TrimSpace(x)
		case bool:
			v = strconv.FormatBool(x)
		case float64:
			v = strconv.FormatFloat(x, 'f', -1, 64)
		}
		if v != "" {
			q = append(q, k+"="+url.QueryEscape(v))
		}
	}
	return q
}

// mongoWithOptions puts the known URI options set in Options onto a pasted
// connection string, replacing the same keys there. The dialog shows them
// as fields (Auth source, Replica set, Authentication) next to the string,
// and a change made there must take effect, also on a saved string.
func mongoWithOptions(uri string, opts map[string]any) string {
	set := mongoOptionParams(opts)
	if len(set) == 0 {
		return uri
	}
	setKeys := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		setKeys[strings.ToLower(k)] = true
	}
	base, query, _ := strings.Cut(uri, "?")
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if key, err := url.QueryUnescape(k); err == nil && setKeys[strings.ToLower(key)] {
			continue
		}
		kept = append(kept, kv)
	}
	// Options need a "/" after the host list: mongodb://h:27017/?authSource=admin
	if _, rest, ok := strings.Cut(base, "://"); ok && !strings.Contains(rest, "/") {
		base += "/"
	}
	return base + "?" + strings.Join(append(kept, set...), "&")
}

// hostHasPort reports whether a host entry already names its port
// ("db1:27018", "[::1]:27017"); a bare IPv6 address does not.
func hostHasPort(h string) bool {
	if strings.HasPrefix(h, "[") {
		return strings.Contains(h, "]:")
	}
	return strings.Count(h, ":") == 1
}

// mongoEscape percent-encodes a user name or password for the URI's user
// info (the driver decodes it with url.PathUnescape, so a space must be
// %20, not +).
func mongoEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func (mongoDriver) Introspect(ctx context.Context, sess Session) (Schema, error) {
	ms, ok := sess.(*mongoSession)
	if !ok {
		return Schema{}, fmt.Errorf("mongo introspect: session type mismatch")
	}

	// Pick which databases to inspect: a connection-bound DB stays
	// single-DB for compatibility; otherwise we walk every non-system
	// DB so the sidebar can group collections under each one.
	var dbNames []string
	if ms.db != nil {
		dbNames = []string{ms.defaultDBName}
	} else {
		all, err := ms.client.ListDatabaseNames(ctx, bson.M{})
		if err != nil {
			return Schema{}, fmt.Errorf("listDatabases: %w", err)
		}
		for _, n := range all {
			if n == "admin" || n == "local" || n == "config" {
				continue
			}
			dbNames = append(dbNames, n)
		}
	}

	out := Schema{Tables: []SchemaTable{}, UpdatedAt: time.Now().Unix()}
	for _, dbName := range dbNames {
		db := ms.client.Database(dbName)
		names, err := db.ListCollectionNames(ctx, bson.M{})
		if err != nil {
			continue
		}
		for _, name := range names {
			coll := db.Collection(name)
			cur, err := coll.Find(ctx, bson.M{}, options.Find().SetLimit(50))
			if err != nil {
				// Find failure (insufficient privileges, view that
				// refuses sampling, etc.) — still surface the collection
				// so the user sees it in the sidebar; just no columns.
				// Empty slice (not nil) so the JSON serializes to []
				// and the FE doesn't crash on `.columns.length`.
				out.Tables = append(out.Tables, SchemaTable{
					Schema:  dbName,
					Name:    name,
					Columns: []ColumnMeta{},
				})
				continue
			}
			seen := map[string]string{}
			var firstDoc bson.M
			for cur.Next(ctx) {
				var doc bson.M
				if err := cur.Decode(&doc); err != nil {
					continue
				}
				if firstDoc == nil {
					firstDoc = doc
				}
				for k, v := range doc {
					if _, ok := seen[k]; !ok {
						seen[k] = bsonTypeName(v)
					}
				}
			}
			_ = cur.Close(ctx)
			cols := make([]ColumnMeta, 0, len(seen))
			for k, t := range seen {
				cols = append(cols, ColumnMeta{Name: k, Type: t})
			}
			out.Tables = append(out.Tables, SchemaTable{
				Schema:  dbName,
				Name:    name,
				Columns: cols,
				Sample:  firstDoc,
			})
		}
	}
	return out, nil
}

// bsonTypeName provides a human-readable name for the runtime type of
// a sampled BSON value — used in the schema's ColumnMeta.Type for the
// FE's autocomplete + type hints.
func bsonTypeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int, int32, int64:
		return "int"
	case float32, float64:
		return "double"
	case bool:
		return "bool"
	case bson.M, map[string]any:
		return "object"
	case bson.A, []any:
		return "array"
	case bson.Binary:
		return "binData"
	case bson.ObjectID:
		return "objectId"
	case time.Time:
		return "date"
	case nil:
		return "null"
	}
	return "mixed"
}

// ──────────────────────────────────────────────────────────────────────
// Mongo Session/Cursor implementation.
// ──────────────────────────────────────────────────────────────────────

type mongoSession struct {
	client        *mongo.Client
	db            *mongo.Database // nil = no default DB (must qualify via getSiblingDB)
	defaultDBName string
	// info records the fallbacks the connect needed (auth source, direct host).
	info TestInfo
}

func (s *mongoSession) testInfo() TestInfo { return s.info }

func (s *mongoSession) Execute(ctx context.Context, q string, opts ExecOpts) (Cursor, error) {
	op, err := parseMongoExpr(q)
	if err != nil {
		return nil, err
	}
	// Resolve which DB this op runs against. `db.getSiblingDB('x').coll.op(`
	// → x; otherwise the connection's default DB; otherwise reject.
	var targetDB *mongo.Database
	if op.Database != "" {
		targetDB = s.client.Database(op.Database)
	} else if s.db != nil {
		targetDB = s.db
	} else {
		return nil, fmt.Errorf("this connection has no default database — use db.getSiblingDB('name').%s.%s(…)", op.Collection, op.Op)
	}
	coll := targetDB.Collection(op.Collection)
	switch op.Op {
	case "find":
		findOpts := options.Find()
		if op.Projection != nil {
			findOpts.SetProjection(op.Projection)
		}
		if opts.Limit > 0 {
			// Apply a server-side cap one above our limit so we can
			// detect truncation without an extra round trip.
			findOpts.SetLimit(int64(opts.Limit + 1))
		}
		// Apply chained shell modifiers: .limit / .skip / .sort / .project
		// fold straight into the find options the driver already exposes.
		// `.count()` short-circuits to countDocuments instead.
		runCount := false
		for _, c := range op.Chain {
			switch c.Method {
			case "limit":
				if n, ok := parseIntArg(c.Arg); ok && n > 0 {
					// User-supplied limit wins over default cap when smaller.
					findOpts.SetLimit(int64(n))
				}
			case "skip":
				if n, ok := parseIntArg(c.Arg); ok && n >= 0 {
					findOpts.SetSkip(int64(n))
				}
			case "sort":
				spec, err := parseJSONish(c.Arg)
				if err != nil {
					return nil, fmt.Errorf(".sort arg: %w", err)
				}
				findOpts.SetSort(spec)
			case "project", "projection":
				spec, err := parseJSONish(c.Arg)
				if err != nil {
					return nil, fmt.Errorf(".%s arg: %w", c.Method, err)
				}
				findOpts.SetProjection(spec)
			case "count":
				runCount = true
			case "toarray", "pretty":
				// no-op — we already return rows
			default:
				return nil, fmt.Errorf("unsupported chained call .%s(...)", c.Method)
			}
		}
		if runCount {
			n, err := coll.CountDocuments(ctx, op.Filter)
			if err != nil {
				return nil, err
			}
			return newMongoStaticCursor([]bson.M{{"count": n}}), nil
		}
		cur, err := coll.Find(ctx, op.Filter, findOpts)
		if err != nil {
			return nil, err
		}
		return newMongoCursor(cur, opts.Limit), nil
	case "findone":
		var doc bson.M
		oneOpts := options.FindOne()
		if op.Projection != nil {
			oneOpts.SetProjection(op.Projection)
		}
		err := coll.FindOne(ctx, op.Filter, oneOpts).Decode(&doc)
		if err != nil && err != mongo.ErrNoDocuments {
			return nil, err
		}
		return newMongoStaticCursor(docToRows(doc)), nil
	case "aggregate":
		pipeline, ok := op.Filter.(bson.A)
		if !ok {
			return nil, fmt.Errorf("aggregate expects an array pipeline")
		}
		cur, err := coll.Aggregate(ctx, pipeline)
		if err != nil {
			return nil, err
		}
		return newMongoCursor(cur, opts.Limit), nil
	case "count", "countdocuments":
		n, err := coll.CountDocuments(ctx, op.Filter)
		if err != nil {
			return nil, err
		}
		return newMongoStaticCursor([]bson.M{{"count": n}}), nil
	case "estimateddocumentcount":
		n, err := coll.EstimatedDocumentCount(ctx)
		if err != nil {
			return nil, err
		}
		return newMongoStaticCursor([]bson.M{{"count": n}}), nil
	case "distinct":
		// op.Distinct holds the field name parsed off the second arg.
		res := coll.Distinct(ctx, op.Distinct, op.Filter)
		if res.Err() != nil {
			return nil, res.Err()
		}
		var vals []any
		if err := res.Decode(&vals); err != nil {
			return nil, err
		}
		rows := make([]bson.M, 0, len(vals))
		for _, v := range vals {
			rows = append(rows, bson.M{"value": v})
		}
		return newMongoStaticCursor(rows), nil
	case "insertone", "insertmany", "updateone", "updatemany", "replaceone", "deleteone", "deletemany":
		row, err := runMongoWrite(ctx, coll, op)
		if err != nil {
			return nil, err
		}
		return newMongoStaticCursor([]bson.M{row}), nil
	}
	return nil, unsupportedMongoOpError(op.Op)
}

// runMongoWrite runs one of the write ops and reports what it changed. The
// execute handler lets a write reach here only on a connection with
// read-write enabled, for its owner or a co-owner. updateMany / deleteMany
// refuse an empty filter: it would touch every document, which is rarely
// what was meant.
func runMongoWrite(ctx context.Context, coll *mongo.Collection, op mongoOp) (bson.M, error) {
	arg := func(i int) any {
		if i < len(op.Args) {
			return op.Args[i]
		}
		return nil
	}
	upsert := func(i int) bool {
		if o, ok := arg(i).(bson.D); ok {
			for _, e := range o {
				if e.Key == "upsert" {
					b, _ := e.Value.(bool)
					return b
				}
			}
		}
		return false
	}
	if (op.Op == "updatemany" || op.Op == "deletemany") && isEmptyMongoDoc(arg(0)) {
		return nil, fmt.Errorf("%s with an empty filter would change every document in %q. Pass a filter; to really match all, use {\"_id\": {\"$exists\": true}}", op.Op, op.Collection)
	}
	switch op.Op {
	case "insertone":
		if arg(0) == nil {
			return nil, fmt.Errorf("insertOne needs a document: db.%s.insertOne({...})", op.Collection)
		}
		res, err := coll.InsertOne(ctx, arg(0))
		if err != nil {
			return nil, err
		}
		return bson.M{"insertedId": res.InsertedID}, nil
	case "insertmany":
		docs, ok := arg(0).(bson.A)
		if !ok || len(docs) == 0 {
			return nil, fmt.Errorf("insertMany needs an array of documents: db.%s.insertMany([{...}, {...}])", op.Collection)
		}
		res, err := coll.InsertMany(ctx, docs)
		if err != nil {
			return nil, err
		}
		return bson.M{"insertedCount": len(res.InsertedIDs), "insertedIds": res.InsertedIDs}, nil
	case "updateone", "updatemany", "replaceone":
		if arg(1) == nil {
			return nil, fmt.Errorf("%s needs a filter and an update: db.%s.%s({filter}, {update})", op.Op, op.Collection, op.Op)
		}
		var res *mongo.UpdateResult
		var err error
		switch op.Op {
		case "updateone":
			res, err = coll.UpdateOne(ctx, arg(0), arg(1), options.UpdateOne().SetUpsert(upsert(2)))
		case "updatemany":
			res, err = coll.UpdateMany(ctx, arg(0), arg(1), options.UpdateMany().SetUpsert(upsert(2)))
		default:
			res, err = coll.ReplaceOne(ctx, arg(0), arg(1), options.Replace().SetUpsert(upsert(2)))
		}
		if err != nil {
			return nil, err
		}
		row := bson.M{"matchedCount": res.MatchedCount, "modifiedCount": res.ModifiedCount}
		if res.UpsertedID != nil {
			row["upsertedId"] = res.UpsertedID
		}
		return row, nil
	default: // deleteone, deletemany
		if arg(0) == nil {
			return nil, fmt.Errorf("%s needs a filter: db.%s.%s({filter})", op.Op, op.Collection, op.Op)
		}
		var res *mongo.DeleteResult
		var err error
		if op.Op == "deleteone" {
			res, err = coll.DeleteOne(ctx, arg(0))
		} else {
			res, err = coll.DeleteMany(ctx, arg(0))
		}
		if err != nil {
			return nil, err
		}
		return bson.M{"deletedCount": res.DeletedCount}, nil
	}
}

func isEmptyMongoDoc(v any) bool {
	switch d := v.(type) {
	case nil:
		return true
	case bson.D:
		return len(d) == 0
	case bson.M:
		return len(d) == 0
	}
	return false
}

// unsupportedMongoOpError builds a user-facing error for a Mongo op the
// executor doesn't implement, with a "did you mean…" hint when the op
// name looks like a known confusion (e.g. `.filter(` is not a Mongo
// collection method — users coming from SQL or array methods reach for
// it instinctively).
func unsupportedMongoOpError(op string) error {
	hint := ""
	switch strings.ToLower(op) {
	case "filter":
		hint = "\n\n.filter() isn't a Mongo collection method. To filter results, pass the filter object to find():\n  db.<collection>.find({ field: { $gte: value } })"
	case "where":
		hint = "\n\n.where() isn't a top-level collection method here. Use find() with a filter object:\n  db.<collection>.find({ field: value })"
	case "select":
		hint = "\n\n.select() isn't a Mongo method. Use find() with a projection:\n  db.<collection>.find({ filter }, { fieldA: 1, fieldB: 1 })"
	case "limit", "skip", "sort", "project", "projection":
		hint = fmt.Sprintf("\n\n.%s() is a cursor modifier — chain it after find(): db.<collection>.find({...}).%s(...)", op, op)
	}
	return fmt.Errorf("operation '%s' is not supported.\n\nSupported: find, findOne, aggregate, count, countDocuments, estimatedDocumentCount, distinct, explain; on a connection with read-write enabled also insertOne, insertMany, updateOne, updateMany, replaceOne, deleteOne, deleteMany.%s", op, hint)
}

func (s *mongoSession) Close() error {
	return s.client.Disconnect(context.Background())
}

// mongoOp is the parsed form of a `db.<coll>.<op>(<args>)` expression
// plus any chained modifier calls (`.limit(N)`, `.skip(N)`, `.sort(…)`)
// that follow it. `Filter` is the first arg parsed as JSON/EJSON;
// `Distinct` carries the field name for `db.x.distinct("field", filter)`;
// `Database` is non-empty when the user used
// `db.getSiblingDB('name').coll.op(…)` to reach a non-default DB.
type mongoOp struct {
	Database   string
	Collection string
	Op         string
	Filter     any
	Distinct   string
	Chain      []chainCall
	// Args holds every argument, parsed (Filter is Args[0]); writes read
	// their document / update / options from it.
	Args []any
	// Projection is find / findOne's second argument.
	Projection any
}

// chainCall describes a `.method(arg)` tagged onto the base op — the
// shell-style way of expressing limits, skips, sorts, etc.
type chainCall struct {
	Method string
	Arg    string // raw text — parsed by the driver according to the method
}

// parseMongoExpr walks the expression with a small balanced-paren
// tokenizer so chained method calls (e.g. `db.x.find().limit(100)`)
// are handled cleanly. Regexes choke on the nested `()` so we hand-roll
// it — the alternative was a full JS parser, which is overkill for the
// tiny dialect we accept.
func parseMongoExpr(q string) (mongoOp, error) {
	s := strings.TrimSpace(q)
	op := mongoOp{}

	// Optional `db.getSiblingDB('name').` prefix.
	if strings.HasPrefix(s, "db.getSiblingDB") {
		rest := strings.TrimPrefix(s, "db.getSiblingDB")
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "(") {
			return op, fmt.Errorf("expected '(' after getSiblingDB")
		}
		name, after, err := captureBalanced(rest, 1)
		if err != nil {
			return op, fmt.Errorf("getSiblingDB arg: %w", err)
		}
		name = strings.TrimSpace(name)
		if len(name) < 2 || (name[0] != '\'' && name[0] != '"') {
			return op, fmt.Errorf("getSiblingDB expects a quoted DB name, got %q", name)
		}
		op.Database = strings.Trim(name, "'\"")
		// After the `)` we expect `.<coll>...`
		s = strings.TrimSpace(after)
		if !strings.HasPrefix(s, ".") {
			return op, fmt.Errorf("expected '.<collection>' after getSiblingDB(...)")
		}
		s = s[1:]
	} else if strings.HasPrefix(s, "db.") {
		s = s[3:]
		// Shorthand: `db.<dbname>.<coll>.<op>(...)` (three dots before the
		// `(`) is treated as `db.getSiblingDB('<dbname>').<coll>.<op>(...)`.
		// We detect this by counting dots in the path that ends at the
		// first `(`. Two segments → coll + op. Three+ segments → first is
		// dbname, rest is coll (allowing dotted collection names), last
		// is op. Anything inside `[...]` / `(...)` / `{...}` or strings
		// doesn't count.
		if op.Database == "" {
			if dbName, rest, ok := splitThreeDotPrefix(s); ok {
				op.Database = dbName
				s = rest
			}
		}
	} else {
		return op, fmt.Errorf("expected db.<collection>.<op>(...) — try db.<dbname>.<collection>.<op>(...) or db.getSiblingDB('name').<collection>.<op>(...) to address another database")
	}

	// `getCollection("name")` — real shell idiom for collection names
	// with unusual chars (dots, spaces, dashes). Treat it as a more
	// explicit way of saying "the next token is a collection".
	if strings.HasPrefix(s, "getCollection") {
		rest := strings.TrimPrefix(s, "getCollection")
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "(") {
			return op, fmt.Errorf("expected '(' after getCollection")
		}
		name, after, err := captureBalanced(rest, 1)
		if err != nil {
			return op, fmt.Errorf("getCollection arg: %w", err)
		}
		name = strings.TrimSpace(name)
		if len(name) < 2 || (name[0] != '\'' && name[0] != '"') {
			return op, fmt.Errorf("getCollection expects a quoted collection name, got %q", name)
		}
		op.Collection = strings.Trim(name, "'\"")
		s = strings.TrimSpace(after)
		if !strings.HasPrefix(s, ".") {
			return op, fmt.Errorf("expected '.<op>(…)' after getCollection(...)")
		}
		s = s[1:]
	} else {
		// Collection name = everything up to the next `.` (no nesting yet).
		dotIdx := strings.Index(s, ".")
		if dotIdx <= 0 {
			return op, fmt.Errorf("missing operation after collection")
		}
		op.Collection = s[:dotIdx]
		s = s[dotIdx+1:]
	}

	// Op name = letters up to `(`.
	parenIdx := strings.Index(s, "(")
	if parenIdx <= 0 {
		return op, fmt.Errorf("missing '(' after operation name")
	}
	op.Op = strings.ToLower(strings.TrimSpace(s[:parenIdx]))

	// Base args = balanced (...)
	body, after, err := captureBalanced(s, parenIdx+1)
	if err != nil {
		return op, fmt.Errorf("%s args: %w", op.Op, err)
	}

	// Zero or more `.method(arg)` chain calls.
	tail := strings.TrimSpace(after)
	for tail != "" {
		if !strings.HasPrefix(tail, ".") {
			return op, fmt.Errorf("unexpected trailing text: %q", tail)
		}
		tail = tail[1:]
		pi := strings.Index(tail, "(")
		if pi <= 0 {
			return op, fmt.Errorf("incomplete chain call %q", tail)
		}
		method := strings.ToLower(strings.TrimSpace(tail[:pi]))
		arg, after2, err := captureBalanced(tail, pi+1)
		if err != nil {
			return op, fmt.Errorf(".%s arg: %w", method, err)
		}
		op.Chain = append(op.Chain, chainCall{Method: method, Arg: strings.TrimSpace(arg)})
		tail = strings.TrimSpace(after2)
	}

	return finishParseMongoExpr(op, strings.TrimSpace(body))
}

// splitThreeDotPrefix recognises a `<dbname>.<rest>` prefix when the
// path leading to the first `(` has three or more `.`-separated
// segments. It returns the first segment as the DB name and the rest
// (starting at the next segment) as the remainder to be parsed as
// `<coll>.<op>(`. Returns ok=false if the path has only two segments
// (i.e. plain `<coll>.<op>`) or doesn't look like a method call.
func splitThreeDotPrefix(s string) (dbName string, rest string, ok bool) {
	// Find the first `(` outside any string literal — that's where the
	// op's arg list starts. Then count `.`s before it.
	parenIdx := -1
	var inSingle, inDouble bool
	var prev byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble && prev != '\\':
			inSingle = !inSingle
		case c == '"' && !inSingle && prev != '\\':
			inDouble = !inDouble
		case c == '(' && !inSingle && !inDouble:
			parenIdx = i
		}
		if parenIdx >= 0 {
			break
		}
		prev = c
	}
	if parenIdx <= 0 {
		return "", "", false
	}
	path := s[:parenIdx]
	if strings.Count(path, ".") < 2 {
		// "<coll>.<op>" has exactly one dot. We need two or more to
		// treat the first segment as a DB name.
		return "", "", false
	}
	dot := strings.IndexByte(path, '.')
	if dot <= 0 {
		return "", "", false
	}
	first := path[:dot]
	// Reject reserved prefixes that could collide with shell helpers.
	switch first {
	case "getSiblingDB", "getCollection":
		return "", "", false
	}
	return first, s[dot+1:], true
}

// captureBalanced walks `s` starting at `start` (already inside an open
// paren) and returns the body between that open paren and its matching
// close, plus the rest of `s` AFTER the close. Respects nested `()`,
// `{}`, `[]`, and single/double quoted strings.
func captureBalanced(s string, start int) (body string, after string, err error) {
	depth := 1
	var inSingle, inDouble bool
	var prev byte
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble && prev != '\\':
			inSingle = !inSingle
		case c == '"' && !inSingle && prev != '\\':
			inDouble = !inDouble
		case !inSingle && !inDouble && (c == '(' || c == '{' || c == '['):
			depth++
		case !inSingle && !inDouble && (c == ')' || c == '}' || c == ']'):
			depth--
			if depth == 0 {
				return s[start:i], s[i+1:], nil
			}
		}
		prev = c
	}
	return "", "", fmt.Errorf("unbalanced parentheses")
}

// finishParseMongoExpr handles the body of a Mongo call once the op +
// collection have been resolved. Shared between the bare `db.x.op(…)`
// and the `db.getSiblingDB('y').x.op(…)` forms.
func finishParseMongoExpr(op mongoOp, body string) (mongoOp, error) {
	body = mongoRegexLiterals(body)
	// Empty parens — no filter (e.g. `db.x.find()`)
	if body == "" {
		op.Filter = bson.M{}
		return op, nil
	}

	// distinct takes ("field", filter?)
	if op.Op == "distinct" {
		parts := splitTopLevelArgs(body)
		if len(parts) == 0 {
			return op, fmt.Errorf("distinct requires a field name")
		}
		field, err := unquote(strings.TrimSpace(parts[0]))
		if err != nil {
			return op, fmt.Errorf("distinct field: %w", err)
		}
		op.Distinct = field
		if len(parts) > 1 {
			f, err := parseJSONish(parts[1])
			if err != nil {
				return op, err
			}
			op.Filter = f
		} else {
			op.Filter = bson.M{}
		}
		return op, nil
	}

	// Everything else: first arg = filter / pipeline JSON; the rest are
	// parsed too (find's projection, a write's update / options).
	for _, part := range splitTopLevelArgs(body) {
		v, err := parseJSONish(part)
		if err != nil {
			return op, err
		}
		op.Args = append(op.Args, v)
	}
	op.Filter = op.Args[0]
	if (op.Op == "find" || op.Op == "findone") && len(op.Args) > 1 {
		op.Projection = op.Args[1]
	}
	return op, nil
}

// splitTopLevelArgs splits a `,`-separated argument list, respecting
// nested `{}` `[]` `()` and string literals. Lightweight — covers the
// queries our intent parser already vetted, not arbitrary JS.
func splitTopLevelArgs(s string) []string {
	parts := []string{}
	depth := 0
	var inSingle, inDouble bool
	last := 0
	for i, r := range s {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{', '[', '(':
			if !inSingle && !inDouble {
				depth++
			}
		case '}', ']', ')':
			if !inSingle && !inDouble {
				depth--
			}
		case ',':
			if depth == 0 && !inSingle && !inDouble {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, s[last:])
	return parts
}

func unquote(s string) (string, error) {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1], nil
	}
	return "", fmt.Errorf("expected quoted string, got %q", s)
}

func parseJSONish(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return bson.M{}, nil
	}
	// `bson.UnmarshalExtJSON` understands Mongo's extended JSON form
	// (ObjectID, ISODate, NumberLong, ...) in addition to plain JSON.
	// Allow single quotes by replacing them with double — best-effort
	// for shell-y syntax.
	cleaned := normalizeShellJSON(mongoRegexLiterals(s))
	var out any
	if err := bson.UnmarshalExtJSON([]byte(cleaned), false, &out); err != nil {
		// Fall back to plain JSON for the cases where ExtJSON is too
		// strict (free-form numeric literals, etc.).
		if err2 := json.Unmarshal([]byte(cleaned), &out); err2 != nil {
			return nil, fmt.Errorf("parse mongo arg: %w\n\nWrite arguments as JSON, e.g. {\"name\": {\"$regex\": \"^abc\", \"$options\": \"i\"}}. Unquoted keys, 'single quotes' and /regex/flags literals are accepted too", err)
		}
	}
	return out, nil
}

// mongoRegexLiterals rewrites shell regex literals (/pattern/flags) in value
// position — after : , [ ( or at the start — into extended JSON
// {"$regex": "pattern", "$options": "flags"}. Text inside quotes is left
// alone; anything that doesn't close like a regex is left as it was.
func mongoRegexLiterals(s string) string {
	var b strings.Builder
	var quote byte
	prev := byte('(') // the start counts as a value position
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			} else if c == quote {
				quote = 0
				prev = c
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if c == '/' && strings.IndexByte(":,[(", prev) >= 0 {
			if pattern, flags, end, ok := scanMongoRegex(s, i); ok {
				p, _ := json.Marshal(pattern)
				b.WriteString(`{"$regex": ` + string(p))
				if flags != "" {
					b.WriteString(`, "$options": "` + flags + `"`)
				}
				b.WriteString("}")
				i = end - 1
				prev = '}'
				continue
			}
		}
		b.WriteByte(c)
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
	}
	return b.String()
}

// scanMongoRegex reads /pattern/flags starting at s[start] == '/'. A slash
// inside [...] or after a backslash doesn't close it; flags are i m s x.
func scanMongoRegex(s string, start int) (pattern, flags string, end int, ok bool) {
	inClass := false
	for i := start + 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == '\n':
			return "", "", 0, false
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			if i == start+1 {
				return "", "", 0, false // "//" is not a regex
			}
			j := i + 1
			for j < len(s) && strings.IndexByte("imsx", s[j]) >= 0 {
				j++
			}
			return s[start+1 : i], s[i+1 : j], j, true
		}
	}
	return "", "", 0, false
}

// normalizeShellJSON is a tiny convenience pass over Mongo shell-style
// input: replaces unquoted keys (`{foo: 1}` → `{"foo":1}`) and single
// quotes with doubles. Doesn't handle every edge case — power users
// can paste strict JSON if the heuristic mangles their query.
func normalizeShellJSON(s string) string {
	// `{foo: ...}` → `{"foo": ...}` — match identifier keys before a colon.
	keyRE := regexp.MustCompile(`([{,]\s*)([a-zA-Z_$][a-zA-Z0-9_$]*)\s*:`)
	s = keyRE.ReplaceAllString(s, `$1"$2":`)
	// Convert single-quoted strings to double-quoted ones, escaping any
	// pre-existing double quotes inside.
	singleRE := regexp.MustCompile(`'((?:\\'|[^'])*)'`)
	s = singleRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		inner = strings.ReplaceAll(inner, `"`, `\"`)
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		return `"` + inner + `"`
	})
	return s
}

// ──────────────────────────────────────────────────────────────────────
// Mongo cursors (live + static)
// ──────────────────────────────────────────────────────────────────────

type mongoCursor struct {
	cur   *mongo.Cursor
	cols  []ColumnMeta
	limit int
	read  int
	first bson.M
}

func newMongoCursor(cur *mongo.Cursor, limit int) Cursor {
	return &mongoCursor{cur: cur, limit: limit}
}

func (c *mongoCursor) Columns() []ColumnMeta {
	if c.cols == nil {
		// Zero-row results never trip inferMongoCols — return [] not nil
		// so the JSON response stays `[]` and the FE doesn't NPE on map.
		return []ColumnMeta{}
	}
	return c.cols
}

func (c *mongoCursor) Next(ctx context.Context) (Row, error) {
	if c.limit > 0 && c.read >= c.limit {
		return nil, nil
	}
	if !c.cur.Next(ctx) {
		if err := c.cur.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var doc bson.M
	if err := c.cur.Decode(&doc); err != nil {
		return nil, err
	}
	if c.first == nil {
		c.first = doc
		c.cols = inferMongoCols(doc)
	}
	c.read++
	return rowFromDoc(doc, c.cols), nil
}

func (c *mongoCursor) Close() error {
	return c.cur.Close(context.Background())
}

type mongoStaticCursor struct {
	docs  []bson.M
	idx   int
	cols  []ColumnMeta
	first bool
}

func newMongoStaticCursor(docs []bson.M) Cursor {
	return &mongoStaticCursor{docs: docs}
}

func (c *mongoStaticCursor) Columns() []ColumnMeta {
	if c.cols == nil {
		return []ColumnMeta{}
	}
	return c.cols
}

func (c *mongoStaticCursor) Next(ctx context.Context) (Row, error) {
	if c.idx >= len(c.docs) {
		return nil, nil
	}
	doc := c.docs[c.idx]
	c.idx++
	if !c.first {
		c.cols = inferMongoCols(doc)
		c.first = true
	}
	return rowFromDoc(doc, c.cols), nil
}

func (c *mongoStaticCursor) Close() error { return nil }

func inferMongoCols(doc bson.M) []ColumnMeta {
	cols := make([]ColumnMeta, 0, len(doc))
	for k, v := range doc {
		cols = append(cols, ColumnMeta{Name: k, Type: bsonTypeName(v)})
	}
	return cols
}

func rowFromDoc(doc bson.M, cols []ColumnMeta) Row {
	out := make(Row, len(cols))
	for i, c := range cols {
		out[i] = doc[c.Name]
	}
	return out
}

// parseIntArg trims a chain method arg and tries to read it as a
// non-negative integer. Used for .limit/.skip whose args are simple
// numeric literals.
func parseIntArg(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func docToRows(doc bson.M) []bson.M {
	if doc == nil {
		return nil
	}
	return []bson.M{doc}
}
