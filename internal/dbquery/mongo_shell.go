package dbquery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongosh-compatible scripts run in a sandboxed JavaScript engine (goja: no
// file system, no network, no timers). The `db` object, collections and
// cursors are thin wrappers over the Go driver, so what people paste from
// mongosh — consts, template literals, arrow functions, forEach, ?. / ??,
// print, .toArray()[0] — just works.
//
// What is shown: like mongosh's REPL, the value of every top-level expression
// statement becomes a result (a cursor or an array of documents as a table,
// a document as one row, a scalar as one cell); print / printjson /
// console.log lines become an "output" result. Writes are allowed only when
// the caller may write (ExecOpts.AllowWrite) — otherwise the call throws with
// ExecOpts.WriteDenied, so a read-only connection stays read-only whatever the
// script does.

const (
	scriptMaxDocs   = 100_000 // documents one cursor may materialise
	scriptMaxOutput = 5_000   // print lines kept
	scriptMaxShown  = 50      // results kept
)

// ScriptResult is one shown value.
type ScriptResult struct {
	Label  string
	Cursor Cursor
}

// ScriptOutcome is what a script produced.
type ScriptOutcome struct {
	Output  []string
	Results []ScriptResult
}

// MongoScripter is implemented by the Mongo session.
type MongoScripter interface {
	ExecuteScript(ctx context.Context, src string, opts ExecOpts) (*ScriptOutcome, error)
}

var (
	shellUseRE  = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z0-9_\-]+)[ \t]*;?[ \t]*$`)
	shellShowRE = regexp.MustCompile(`(?m)^[ \t]*show[ \t]+(dbs|databases|collections|tables)[ \t]*;?[ \t]*$`)
)

// preprocessShell turns mongosh shell commands into JavaScript, keeping every
// line where it was so error positions stay right.
func preprocessShell(src string) string {
	// A block, not an expression statement, so it isn't shown as a result.
	src = shellUseRE.ReplaceAllString(src, `{ db = db.getSiblingDB("$1"); }`)
	return shellShowRE.ReplaceAllStringFunc(src, func(m string) string {
		if strings.Contains(m, "dbs") || strings.Contains(m, "databases") {
			return `db.adminCommand({ listDatabases: 1 }).databases;`
		}
		return `db.getCollectionNames();`
	})
}

// IsMongoJSScript reports whether q is a script for the JavaScript engine:
// valid JavaScript that is not one plain db.<coll>.<op>(…) call the simple
// parser already handles (that path keeps explain, the query bar etc.).
func IsMongoJSScript(q string) bool {
	trimmed := strings.TrimSpace(q)
	if trimmed == "" {
		return false
	}
	single := strings.TrimSuffix(trimmed, ";")
	if op, err := parseMongoExpr(single); err == nil && !hasUnquotedSemicolon(single) && simpleChain(op) {
		return false
	}
	_, err := parser.ParseFile(nil, "", preprocessShell(q), 0)
	return err == nil
}

// simpleChain: every chained method is one the single-call path handles;
// anything else (forEach, map, next…) needs the JavaScript engine.
func simpleChain(op mongoOp) bool {
	for _, c := range op.Chain {
		switch strings.ToLower(c.Method) {
		case "limit", "skip", "sort", "project", "projection", "count", "hint", "collation", "toarray", "pretty", "explain":
		default:
			return false
		}
	}
	return true
}

// prepareScript rewrites each top-level expression statement into
// __pmShow(<expr>, i) and returns the labels.
func prepareScript(src string) (string, []string, error) {
	src = preprocessShell(src)
	prog, err := parser.ParseFile(nil, "", src, 0)
	if err != nil {
		return "", nil, fmt.Errorf("script syntax error: %v", err)
	}
	type span struct{ from, to int }
	var spans []span
	for _, st := range prog.Body {
		es, ok := st.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		from, to := int(es.Expression.Idx0())-1, int(es.Expression.Idx1())-1
		if from < 0 || to > len(src) || from >= to {
			continue
		}
		spans = append(spans, span{from, to})
	}
	labels := make([]string, len(spans))
	out := src
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		expr := out[s.from:s.to]
		labels[i] = compactLabel(expr)
		out = out[:s.from] + fmt.Sprintf("__pmShow((%s), %d)", expr, i) + out[s.to:]
	}
	return out, labels, nil
}

func compactLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ── engine ─────────────────────────────────────────────────────────────

type jsEngine struct {
	ctx     context.Context
	vm      *goja.Runtime
	client  *mongo.Client
	opts    ExecOpts
	wrapped map[*goja.Object]any // ObjectId / NumberDecimal / UUID … → Go value
	cursors map[*goja.Object]*jsCursor
	dynamic map[*goja.Object]bool // db / collection handles
	out     *ScriptOutcome
	labels  []string
}

func (s *mongoSession) ExecuteScript(ctx context.Context, src string, opts ExecOpts) (*ScriptOutcome, error) {
	code, labels, err := prepareScript(src)
	if err != nil {
		return nil, err
	}
	e := &jsEngine{ctx: ctx, vm: goja.New(), client: s.client, opts: opts, wrapped: map[*goja.Object]any{},
		cursors: map[*goja.Object]*jsCursor{}, dynamic: map[*goja.Object]bool{}, out: &ScriptOutcome{}, labels: labels}
	e.vm.SetFieldNameMapper(goja.UncapFieldNameMapper())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			e.vm.Interrupt("script stopped: " + ctx.Err().Error())
		case <-stop:
		}
	}()
	e.install(s.defaultDBName)
	if _, err := e.vm.RunString(code); err != nil {
		return e.out, scriptError(err)
	}
	return e.out, nil
}

// scriptError turns a goja error into a short message with the script line
// (rewriting keeps lines in place), without Go frames.
func scriptError(err error) error {
	ex, ok := err.(*goja.Exception)
	if !ok {
		if ie, isInt := err.(*goja.InterruptedError); isInt {
			return fmt.Errorf("%v", ie.Value())
		}
		return err
	}
	msg := ex.Value().String()
	if inner := ex.Unwrap(); inner != nil {
		msg = inner.Error()
	}
	for _, f := range ex.Stack() {
		if p := f.Position(); p.Line > 0 {
			return fmt.Errorf("%s (line %d)", msg, p.Line)
		}
	}
	return fmt.Errorf("%s", msg)
}

func (e *jsEngine) throw(format string, a ...any) {
	panic(e.vm.NewGoError(fmt.Errorf(format, a...)))
}

func (e *jsEngine) canWrite(what string) {
	if !e.opts.AllowWrite {
		msg := e.opts.WriteDenied
		if msg == "" {
			msg = "this connection is read-only"
		}
		e.throw("%s is a write — blocked: %s", what, msg)
	}
}

func (e *jsEngine) install(defaultDB string) {
	vm := e.vm
	printLine := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			if len(e.out.Output) >= scriptMaxOutput {
				return
			}
			e.out.Output = append(e.out.Output, line)
		}
	}
	format := func(args []goja.Value, pretty bool) string {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			if goja.IsUndefined(a) {
				parts = append(parts, "undefined")
				continue
			}
			if s, ok := a.Export().(string); ok && !pretty {
				parts = append(parts, s)
				continue
			}
			parts = append(parts, e.inspect(a, pretty))
		}
		return strings.Join(parts, " ")
	}
	_ = vm.Set("print", func(call goja.FunctionCall) goja.Value {
		printLine(format(call.Arguments, false))
		return goja.Undefined()
	})
	_ = vm.Set("printjson", func(call goja.FunctionCall) goja.Value {
		printLine(format(call.Arguments, true))
		return goja.Undefined()
	})
	console := vm.NewObject()
	for _, name := range []string{"log", "info", "debug"} {
		_ = console.Set(name, func(call goja.FunctionCall) goja.Value {
			printLine(format(call.Arguments, false))
			return goja.Undefined()
		})
	}
	for _, name := range []string{"warn", "error"} {
		prefix := "[" + name + "] "
		_ = console.Set(name, func(call goja.FunctionCall) goja.Value {
			printLine(prefix + format(call.Arguments, false))
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)
	_ = vm.Set("sleep", func(ms int64) goja.Value {
		if ms > 5000 {
			ms = 5000
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-e.ctx.Done():
		}
		return goja.Undefined()
	})

	// BSON constructors.
	_ = vm.Set("ObjectId", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 || goja.IsUndefined(call.Argument(0)) {
			return e.toJS(bson.NewObjectID())
		}
		oid, err := bson.ObjectIDFromHex(call.Argument(0).String())
		if err != nil {
			e.throw("ObjectId(%q): %v", call.Argument(0).String(), err)
		}
		return e.toJS(oid)
	})
	isoDate := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return e.toJS(bson.NewDateTimeFromTime(time.Now()))
		}
		s := call.Argument(0).String()
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return e.toJS(bson.NewDateTimeFromTime(t))
			}
		}
		e.throw("ISODate(%q): not a date", s)
		return nil
	}
	_ = vm.Set("ISODate", isoDate)
	_ = vm.Set("NumberLong", func(v goja.Value) goja.Value { return vm.ToValue(v.ToInteger()) })
	_ = vm.Set("NumberInt", func(v goja.Value) goja.Value { return vm.ToValue(int32(v.ToInteger())) })
	_ = vm.Set("NumberDecimal", func(v goja.Value) goja.Value {
		d, err := bson.ParseDecimal128(v.String())
		if err != nil {
			e.throw("NumberDecimal(%q): %v", v.String(), err)
		}
		return e.toJS(d)
	})
	_ = vm.Set("UUID", func(call goja.FunctionCall) goja.Value {
		s := strings.ReplaceAll(call.Argument(0).String(), "-", "")
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 16 {
			e.throw("UUID(%q): expected 32 hex digits", call.Argument(0).String())
		}
		return e.toJS(bson.Binary{Subtype: 4, Data: b})
	})

	// __pmShow collects top-level expression values.
	_ = vm.Set("__pmShow", func(v goja.Value, idx int) goja.Value {
		e.show(v, idx)
		return v
	})
	_ = vm.Set("db", e.dbObject(defaultDB))
}

// ── db / collection / cursor ───────────────────────────────────────────

type dynMethods struct {
	e        *jsEngine
	methods  map[string]goja.Value
	fallback func(key string) goja.Value
	setter   func(key string, v goja.Value) bool
}

func (d *dynMethods) Get(key string) goja.Value {
	if m, ok := d.methods[key]; ok {
		return m
	}
	if d.fallback != nil {
		return d.fallback(key)
	}
	return goja.Undefined()
}
func (d *dynMethods) Set(key string, v goja.Value) bool {
	if d.setter != nil {
		return d.setter(key, v)
	}
	return false
}
func (d *dynMethods) Has(key string) bool { _, ok := d.methods[key]; return ok || d.fallback != nil }
func (d *dynMethods) Delete(string) bool  { return false }
func (d *dynMethods) Keys() []string {
	keys := make([]string, 0, len(d.methods))
	for k := range d.methods {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (e *jsEngine) fn(f func(call goja.FunctionCall) goja.Value) goja.Value { return e.vm.ToValue(f) }

func (e *jsEngine) dbObject(name string) goja.Value {
	vm := e.vm
	database := func() *mongo.Database {
		if name == "" {
			e.throw("this connection has no default database — start the script with `use <db>` or call db.getSiblingDB('name')")
		}
		return e.client.Database(name)
	}
	colls := map[string]goja.Value{}
	d := &dynMethods{e: e, methods: map[string]goja.Value{}}
	m := d.methods
	m["getSiblingDB"] = e.fn(func(c goja.FunctionCall) goja.Value { return e.dbObject(c.Argument(0).String()) })
	m["getName"] = e.fn(func(goja.FunctionCall) goja.Value { return vm.ToValue(name) })
	m["toString"] = m["getName"]
	m["getCollection"] = e.fn(func(c goja.FunctionCall) goja.Value { return e.collObject(database, c.Argument(0).String()) })
	m["getCollectionNames"] = e.fn(func(goja.FunctionCall) goja.Value {
		names, err := database().ListCollectionNames(e.ctx, bson.D{})
		if err != nil {
			e.throw("getCollectionNames: %v", err)
		}
		sort.Strings(names)
		arr := make([]any, len(names))
		for i, n := range names {
			arr[i] = n
		}
		return vm.NewArray(arr...)
	})
	m["getCollectionInfos"] = e.fn(func(c goja.FunctionCall) goja.Value {
		cur, err := database().ListCollections(e.ctx, e.docArg(c.Argument(0)))
		if err != nil {
			e.throw("getCollectionInfos: %v", err)
		}
		return e.drain(cur)
	})
	runCmd := func(db func() *mongo.Database) func(c goja.FunctionCall) goja.Value {
		return func(c goja.FunctionCall) goja.Value {
			cmd := e.docArg(c.Argument(0))
			if len(cmd) == 0 {
				e.throw("runCommand needs a command document")
			}
			if !readOnlyCommand(cmd[0].Key) {
				e.canWrite("command " + cmd[0].Key)
			}
			var res bson.D
			if err := db().RunCommand(e.ctx, cmd).Decode(&res); err != nil {
				e.throw("%s: %v", cmd[0].Key, err)
			}
			return e.toJS(res)
		}
	}
	m["runCommand"] = e.fn(runCmd(database))
	m["adminCommand"] = e.fn(runCmd(func() *mongo.Database { return e.client.Database("admin") }))
	m["stats"] = e.fn(func(goja.FunctionCall) goja.Value {
		var res bson.D
		if err := database().RunCommand(e.ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&res); err != nil {
			e.throw("stats: %v", err)
		}
		return e.toJS(res)
	})
	d.fallback = func(key string) goja.Value {
		if c, ok := colls[key]; ok {
			return c
		}
		c := e.collObject(database, key)
		colls[key] = c
		return c
	}
	obj := vm.NewDynamicObject(d)
	e.dynamic[obj] = true
	return obj
}

// readOnlyCommand: database commands a read-only connection may run.
func readOnlyCommand(name string) bool {
	switch strings.ToLower(name) {
	case "ping", "buildinfo", "hello", "ismaster", "serverstatus", "dbstats", "collstats", "listdatabases",
		"listcollections", "listindexes", "count", "distinct", "find", "connectionstatus", "hostinfo",
		"getparameter", "getcmdlineopts", "replsetgetstatus", "datasize", "explain", "validate", "top", "currentop":
		return true
	}
	return false
}

func (e *jsEngine) collObject(database func() *mongo.Database, name string) goja.Value {
	vm := e.vm
	coll := func() *mongo.Collection { return database().Collection(name) }
	d := &dynMethods{e: e, methods: map[string]goja.Value{}}
	m := d.methods
	arg := func(c goja.FunctionCall, i int) bson.D { return e.docArg(c.Argument(i)) }

	m["getName"] = e.fn(func(goja.FunctionCall) goja.Value { return vm.ToValue(name) })
	m["getFullName"] = e.fn(func(goja.FunctionCall) goja.Value { return vm.ToValue(database().Name() + "." + name) })
	m["toString"] = m["getFullName"]
	m["find"] = e.fn(func(c goja.FunctionCall) goja.Value {
		return e.cursorObject(coll, name, &cursorSpec{kind: "find", filter: arg(c, 0), projection: optDoc(e, c.Argument(1))})
	})
	m["findOne"] = e.fn(func(c goja.FunctionCall) goja.Value {
		o := options.FindOne()
		if p := optDoc(e, c.Argument(1)); p != nil {
			o.SetProjection(p)
		}
		var doc bson.D
		err := coll().FindOne(e.ctx, arg(c, 0), o).Decode(&doc)
		if err == mongo.ErrNoDocuments {
			return goja.Null()
		}
		if err != nil {
			e.throw("findOne: %v", err)
		}
		return e.toJS(doc)
	})
	countDocs := func(c goja.FunctionCall) goja.Value {
		n, err := coll().CountDocuments(e.ctx, arg(c, 0))
		if err != nil {
			e.throw("countDocuments: %v", err)
		}
		return vm.ToValue(n)
	}
	m["countDocuments"] = e.fn(countDocs)
	m["count"] = e.fn(countDocs)
	m["estimatedDocumentCount"] = e.fn(func(goja.FunctionCall) goja.Value {
		n, err := coll().EstimatedDocumentCount(e.ctx)
		if err != nil {
			e.throw("estimatedDocumentCount: %v", err)
		}
		return vm.ToValue(n)
	})
	m["distinct"] = e.fn(func(c goja.FunctionCall) goja.Value {
		var vals bson.A
		if err := coll().Distinct(e.ctx, c.Argument(0).String(), arg(c, 1)).Decode(&vals); err != nil {
			e.throw("distinct: %v", err)
		}
		return e.toJS(vals)
	})
	m["aggregate"] = e.fn(func(c goja.FunctionCall) goja.Value {
		var pipeline bson.A
		first := c.Argument(0)
		if o, ok := first.(*goja.Object); ok && o.ClassName() == "Array" {
			pipeline, _ = e.toBSON(first).(bson.A)
		} else {
			for _, a := range c.Arguments {
				pipeline = append(pipeline, e.toBSON(a))
			}
		}
		for _, st := range pipeline {
			if d, ok := st.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
				e.canWrite("an aggregate with " + d[0].Key)
			}
		}
		spec := &cursorSpec{kind: "aggregate", pipeline: pipeline}
		if fo, ok := first.(*goja.Object); ok && fo.ClassName() == "Array" {
			for _, kv := range optDoc(e, c.Argument(1)) {
				if kv.Key == "allowDiskUse" {
					b, _ := kv.Value.(bool)
					spec.allowDisk = b
				}
			}
		}
		return e.cursorObject(coll, name, spec)
	})
	m["getIndexes"] = e.fn(func(goja.FunctionCall) goja.Value {
		cur, err := coll().Indexes().List(e.ctx)
		if err != nil {
			e.throw("getIndexes: %v", err)
		}
		return e.drain(cur)
	})
	m["stats"] = e.fn(func(goja.FunctionCall) goja.Value {
		var res bson.D
		if err := database().RunCommand(e.ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&res); err != nil {
			e.throw("stats: %v", err)
		}
		return e.toJS(res)
	})

	// ── writes (blocked unless the caller may write) ──
	emptyGuard := func(op string, filter bson.D) {
		if len(filter) == 0 {
			e.throw("%s with an empty filter would change every document in %q. Pass a filter; to really match all, use {_id: {$exists: true}}", op, name)
		}
	}
	upsertOpt := func(c goja.FunctionCall, i int) bool {
		for _, kv := range arg(c, i) {
			if kv.Key == "upsert" {
				b, _ := kv.Value.(bool)
				return b
			}
		}
		return false
	}
	m["insertOne"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("insertOne")
		r, err := coll().InsertOne(e.ctx, arg(c, 0))
		if err != nil {
			e.throw("insertOne: %v", err)
		}
		return e.toJS(bson.D{{Key: "acknowledged", Value: true}, {Key: "insertedId", Value: r.InsertedID}})
	})
	m["insertMany"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("insertMany")
		docs, _ := e.toBSON(c.Argument(0)).(bson.A)
		r, err := coll().InsertMany(e.ctx, []any(docs))
		if err != nil {
			e.throw("insertMany: %v", err)
		}
		return e.toJS(bson.D{{Key: "acknowledged", Value: true}, {Key: "insertedIds", Value: bson.A(r.InsertedIDs)}})
	})
	updateRes := func(r *mongo.UpdateResult) goja.Value {
		return e.toJS(bson.D{{Key: "acknowledged", Value: true}, {Key: "matchedCount", Value: r.MatchedCount},
			{Key: "modifiedCount", Value: r.ModifiedCount}, {Key: "upsertedCount", Value: r.UpsertedCount}, {Key: "upsertedId", Value: r.UpsertedID}})
	}
	m["updateOne"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("updateOne")
		r, err := coll().UpdateOne(e.ctx, arg(c, 0), e.toBSON(c.Argument(1)), options.UpdateOne().SetUpsert(upsertOpt(c, 2)))
		if err != nil {
			e.throw("updateOne: %v", err)
		}
		return updateRes(r)
	})
	m["updateMany"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("updateMany")
		emptyGuard("updateMany", arg(c, 0))
		r, err := coll().UpdateMany(e.ctx, arg(c, 0), e.toBSON(c.Argument(1)), options.UpdateMany().SetUpsert(upsertOpt(c, 2)))
		if err != nil {
			e.throw("updateMany: %v", err)
		}
		return updateRes(r)
	})
	m["replaceOne"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("replaceOne")
		r, err := coll().ReplaceOne(e.ctx, arg(c, 0), arg(c, 1), options.Replace().SetUpsert(upsertOpt(c, 2)))
		if err != nil {
			e.throw("replaceOne: %v", err)
		}
		return updateRes(r)
	})
	deleteRes := func(r *mongo.DeleteResult) goja.Value {
		return e.toJS(bson.D{{Key: "acknowledged", Value: true}, {Key: "deletedCount", Value: r.DeletedCount}})
	}
	m["deleteOne"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("deleteOne")
		r, err := coll().DeleteOne(e.ctx, arg(c, 0))
		if err != nil {
			e.throw("deleteOne: %v", err)
		}
		return deleteRes(r)
	})
	m["deleteMany"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("deleteMany")
		emptyGuard("deleteMany", arg(c, 0))
		r, err := coll().DeleteMany(e.ctx, arg(c, 0))
		if err != nil {
			e.throw("deleteMany: %v", err)
		}
		return deleteRes(r)
	})
	findOneAnd := func(op string) func(c goja.FunctionCall) goja.Value {
		return func(c goja.FunctionCall) goja.Value {
			e.canWrite(op)
			var res *mongo.SingleResult
			switch op {
			case "findOneAndUpdate":
				res = coll().FindOneAndUpdate(e.ctx, arg(c, 0), e.toBSON(c.Argument(1)))
			case "findOneAndReplace":
				res = coll().FindOneAndReplace(e.ctx, arg(c, 0), arg(c, 1))
			default:
				res = coll().FindOneAndDelete(e.ctx, arg(c, 0))
			}
			var doc bson.D
			if err := res.Decode(&doc); err == mongo.ErrNoDocuments {
				return goja.Null()
			} else if err != nil {
				e.throw("%s: %v", op, err)
			}
			return e.toJS(doc)
		}
	}
	for _, op := range []string{"findOneAndUpdate", "findOneAndReplace", "findOneAndDelete"} {
		m[op] = e.fn(findOneAnd(op))
	}
	m["createIndex"] = e.fn(func(c goja.FunctionCall) goja.Value {
		e.canWrite("createIndex")
		model := mongo.IndexModel{Keys: arg(c, 0)}
		if o := optDoc(e, c.Argument(1)); o != nil {
			io := options.Index()
			for _, kv := range o {
				switch kv.Key {
				case "name":
					io.SetName(fmt.Sprint(kv.Value))
				case "unique":
					b, _ := kv.Value.(bool)
					io.SetUnique(b)
				}
			}
			model.Options = io
		}
		n, err := coll().Indexes().CreateOne(e.ctx, model)
		if err != nil {
			e.throw("createIndex: %v", err)
		}
		return vm.ToValue(n)
	})
	m["drop"] = e.fn(func(goja.FunctionCall) goja.Value {
		e.canWrite("drop")
		if err := coll().Drop(e.ctx); err != nil {
			e.throw("drop: %v", err)
		}
		return vm.ToValue(true)
	})
	for _, op := range []string{"bulkWrite", "renameCollection", "dropIndex", "dropIndexes", "mapReduce"} {
		op := op
		m[op] = e.fn(func(goja.FunctionCall) goja.Value {
			e.canWrite(op)
			e.throw("%s is not supported in PatchMan scripts yet", op)
			return nil
		})
	}
	// db.a.b — dotted collection names.
	d.fallback = func(key string) goja.Value { return e.collObject(database, name+"."+key) }
	obj := vm.NewDynamicObject(d)
	e.dynamic[obj] = true
	return obj
}

func optDoc(e *jsEngine, v goja.Value) bson.D {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return e.docArg(v)
}

type cursorSpec struct {
	kind       string // find | aggregate
	filter     bson.D
	projection bson.D
	pipeline   bson.A
	sort       bson.D
	skip       int64
	limit      int64
	hint       any
	collation  *options.Collation
	allowDisk  bool
	mapFn      goja.Callable
}

func (e *jsEngine) open(coll func() *mongo.Collection, spec *cursorSpec, cap int64) *mongo.Cursor {
	var (
		cur *mongo.Cursor
		err error
	)
	if spec.kind == "aggregate" {
		o := options.Aggregate()
		if spec.allowDisk {
			o.SetAllowDiskUse(true)
		}
		if spec.collation != nil {
			o.SetCollation(spec.collation)
		}
		if spec.hint != nil {
			o.SetHint(spec.hint)
		}
		cur, err = coll().Aggregate(e.ctx, spec.pipeline, o)
	} else {
		o := options.Find()
		if spec.projection != nil {
			o.SetProjection(spec.projection)
		}
		if spec.sort != nil {
			o.SetSort(spec.sort)
		}
		if spec.skip > 0 {
			o.SetSkip(spec.skip)
		}
		limit := spec.limit
		if cap > 0 && (limit <= 0 || limit > cap) {
			limit = cap
		}
		if limit > 0 {
			o.SetLimit(limit)
		}
		if spec.hint != nil {
			o.SetHint(spec.hint)
		}
		if spec.collation != nil {
			o.SetCollation(spec.collation)
		}
		cur, err = coll().Find(e.ctx, spec.filter, o)
	}
	if err != nil {
		e.throw("%s: %v", spec.kind, err)
	}
	return cur
}

// cursorObject is a lazy mongosh cursor: modifiers chain, the query runs on
// the first terminal call (toArray, forEach, next…).
func (e *jsEngine) cursorObject(coll func() *mongo.Collection, collName string, spec *cursorSpec) goja.Value {
	vm := e.vm
	obj := vm.NewObject()
	var live *mongo.Cursor
	ensure := func() *mongo.Cursor {
		if live == nil {
			live = e.open(coll, spec, 0)
		}
		return live
	}
	self := func(f func(c goja.FunctionCall)) func(c goja.FunctionCall) goja.Value {
		return func(c goja.FunctionCall) goja.Value {
			if live != nil {
				e.throw("cursor already started — modifiers must come before toArray / forEach / next")
			}
			f(c)
			return obj
		}
	}
	set := func(name string, f func(c goja.FunctionCall) goja.Value) { _ = obj.Set(name, f) }
	set("sort", self(func(c goja.FunctionCall) { spec.sort = e.docArg(c.Argument(0)); spec.pipelineStage("$sort", spec.sort) }))
	set("limit", self(func(c goja.FunctionCall) {
		spec.limit = c.Argument(0).ToInteger()
		spec.pipelineStage("$limit", spec.limit)
	}))
	set("skip", self(func(c goja.FunctionCall) {
		spec.skip = c.Argument(0).ToInteger()
		spec.pipelineStage("$skip", spec.skip)
	}))
	project := self(func(c goja.FunctionCall) {
		spec.projection = e.docArg(c.Argument(0))
		spec.pipelineStage("$project", spec.projection)
	})
	set("project", project)
	set("projection", project)
	set("hint", self(func(c goja.FunctionCall) { spec.hint = e.toBSON(c.Argument(0)) }))
	set("collation", self(func(c goja.FunctionCall) {
		var col options.Collation
		raw, _ := bson.Marshal(e.docArg(c.Argument(0)))
		_ = bson.Unmarshal(raw, &col)
		spec.collation = &col
	}))
	set("allowDiskUse", self(func(goja.FunctionCall) { spec.allowDisk = true }))
	for _, noop := range []string{"batchSize", "maxTimeMS", "comment", "readPref", "noCursorTimeout", "pretty", "readConcern", "tailable"} {
		set(noop, func(goja.FunctionCall) goja.Value { return obj })
	}
	var buffered *bson.D // a document hasNext() already read
	fetch := func() bool {
		if buffered != nil {
			return true
		}
		cur := ensure()
		if !cur.Next(e.ctx) {
			return false
		}
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			e.throw("decode: %v", err)
		}
		buffered = &d
		return true
	}
	each := func(fn func(doc bson.D) bool) {
		cur := ensure()
		n := 0
		if buffered != nil {
			d := *buffered
			buffered = nil
			n++
			if !fn(d) {
				return
			}
		}
		for cur.Next(e.ctx) {
			n++
			if n > scriptMaxDocs {
				e.throw("more than %d documents from %s — add a limit or a narrower filter", scriptMaxDocs, collName)
			}
			var doc bson.D
			if err := cur.Decode(&doc); err != nil {
				e.throw("decode: %v", err)
			}
			if !fn(doc) {
				return
			}
		}
		if err := cur.Err(); err != nil {
			e.throw("%s: %v", spec.kind, err)
		}
	}
	set("toArray", func(goja.FunctionCall) goja.Value {
		var out []any
		each(func(d bson.D) bool { out = append(out, e.toJS(d)); return true })
		return e.withToArray(vm.NewArray(out...))
	})
	set("forEach", func(c goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(c.Argument(0))
		if !ok {
			e.throw("forEach needs a function")
		}
		each(func(d bson.D) bool {
			r, err := fn(goja.Undefined(), e.toJS(d))
			if err != nil {
				panic(err)
			}
			return !(r != nil && r.ExportType() != nil && r.Export() == false)
		})
		return goja.Undefined()
	})
	set("map", func(c goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(c.Argument(0))
		if !ok {
			e.throw("map needs a function")
		}
		var out []any
		each(func(d bson.D) bool {
			r, err := fn(goja.Undefined(), e.toJS(d))
			if err != nil {
				panic(err)
			}
			out = append(out, r)
			return true
		})
		return e.withToArray(vm.NewArray(out...))
	})
	set("hasNext", func(goja.FunctionCall) goja.Value { return vm.ToValue(fetch()) })
	set("next", func(goja.FunctionCall) goja.Value {
		if !fetch() {
			e.throw("no more documents")
		}
		d := *buffered
		buffered = nil
		return e.toJS(d)
	})
	countFn := func(goja.FunctionCall) goja.Value {
		if spec.kind == "find" {
			n, err := coll().CountDocuments(e.ctx, spec.filter)
			if err != nil {
				e.throw("count: %v", err)
			}
			return vm.ToValue(n)
		}
		n := 0
		each(func(bson.D) bool { n++; return true })
		return vm.ToValue(n)
	}
	set("count", countFn)
	set("size", countFn)
	set("itcount", func(goja.FunctionCall) goja.Value {
		n := 0
		each(func(bson.D) bool { n++; return true })
		return vm.ToValue(n)
	})
	set("explain", func(c goja.FunctionCall) goja.Value {
		verbosity := "queryPlanner"
		if s, ok := c.Argument(0).Export().(string); ok && s != "" {
			verbosity = s
		}
		var inner bson.D
		if spec.kind == "aggregate" {
			inner = bson.D{{Key: "aggregate", Value: collName}, {Key: "pipeline", Value: spec.pipeline}, {Key: "cursor", Value: bson.D{}}}
		} else {
			inner = bson.D{{Key: "find", Value: collName}, {Key: "filter", Value: spec.filter}}
			if spec.sort != nil {
				inner = append(inner, bson.E{Key: "sort", Value: spec.sort})
			}
			if spec.limit > 0 {
				inner = append(inner, bson.E{Key: "limit", Value: spec.limit})
			}
		}
		var res bson.D
		if err := coll().Database().RunCommand(e.ctx, bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: verbosity}}).Decode(&res); err != nil {
			e.throw("explain: %v", err)
		}
		return e.toJS(res)
	})
	set("close", func(goja.FunctionCall) goja.Value {
		if live != nil {
			_ = live.Close(e.ctx)
		}
		return goja.Undefined()
	})
	e.cursors[obj] = &jsCursor{coll: coll, spec: spec, started: func() bool { return live != nil }}
	return obj
}

// pipelineStage keeps aggregate cursors' chained modifiers as stages.
func (s *cursorSpec) pipelineStage(stage string, v any) {
	if s.kind == "aggregate" {
		s.pipeline = append(s.pipeline, bson.D{{Key: stage, Value: v}})
	}
}

type jsCursor struct {
	coll    func() *mongo.Collection
	spec    *cursorSpec
	started func() bool
}

// withToArray lets `cursor.map(f).toArray()` keep working on the array map returns.
func (e *jsEngine) withToArray(arr *goja.Object) goja.Value {
	_ = arr.Set("toArray", func(goja.FunctionCall) goja.Value { return arr })
	return arr
}

func (e *jsEngine) drain(cur *mongo.Cursor) goja.Value {
	var out []any
	for cur.Next(e.ctx) {
		var doc bson.D
		_ = cur.Decode(&doc)
		out = append(out, e.toJS(doc))
	}
	_ = cur.Close(e.ctx)
	return e.vm.NewArray(out...)
}

// ── shown values ───────────────────────────────────────────────────────

func (e *jsEngine) show(v goja.Value, idx int) {
	if v == nil || goja.IsUndefined(v) || len(e.out.Results) >= scriptMaxShown {
		return
	}
	label := ""
	if idx >= 0 && idx < len(e.labels) {
		label = e.labels[idx]
	}
	add := func(docs []bson.D) {
		e.out.Results = append(e.out.Results, ScriptResult{Label: label, Cursor: &mongoStaticCursor{docs: docs, cols: unionMongoCols(docs)}})
	}
	if o, ok := v.(*goja.Object); ok {
		if jc, isCur := e.cursors[o]; isCur {
			if jc.started() {
				return // already consumed (e.g. a forEach expression)
			}
			cur := e.open(jc.coll, jc.spec, int64(e.opts.Limit))
			var docs []bson.D
			for cur.Next(e.ctx) && (e.opts.Limit <= 0 || len(docs) < e.opts.Limit) {
				var d bson.D
				_ = cur.Decode(&d)
				docs = append(docs, d)
			}
			_ = cur.Close(e.ctx)
			add(docs)
			return
		}
		if _, isFn := goja.AssertFunction(o); isFn {
			return
		}
		if e.dynamic[o] {
			return // a db or collection handle (e.g. `const c = db.x` echoed)
		}
	}
	val := e.toBSON(v)
	switch t := val.(type) {
	case nil:
		return
	case bson.A:
		docs := make([]bson.D, 0, len(t))
		for _, x := range t {
			if d, ok := x.(bson.D); ok {
				docs = append(docs, d)
			} else {
				docs = append(docs, bson.D{{Key: "value", Value: x}})
			}
		}
		add(docs)
	case bson.D:
		add([]bson.D{t})
	default:
		add([]bson.D{{{Key: "value", Value: t}}})
	}
}

// ── conversions ────────────────────────────────────────────────────────

func (e *jsEngine) docArg(v goja.Value) bson.D {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return bson.D{}
	}
	switch t := e.toBSON(v).(type) {
	case bson.D:
		return t
	case nil:
		return bson.D{}
	default:
		e.throw("expected a document, got %v", v)
		return nil
	}
}

func (e *jsEngine) wrap(goVal any, class, display string) goja.Value {
	o := e.vm.NewObject()
	_ = o.Set("toString", func(goja.FunctionCall) goja.Value { return e.vm.ToValue(display) })
	_ = o.Set("toJSON", func(goja.FunctionCall) goja.Value { return e.vm.ToValue(display) })
	_ = o.Set("valueOf", func(goja.FunctionCall) goja.Value { return e.vm.ToValue(display) })
	_ = o.Set("_bsontype", class)
	if class == "ObjectId" {
		_ = o.Set("toHexString", func(goja.FunctionCall) goja.Value { return e.vm.ToValue(display) })
		_ = o.Set("str", display)
		oid := goVal.(bson.ObjectID)
		_ = o.Set("getTimestamp", func(goja.FunctionCall) goja.Value { return e.toJS(bson.NewDateTimeFromTime(oid.Timestamp())) })
		_ = o.Set("equals", func(c goja.FunctionCall) goja.Value {
			other := e.toBSON(c.Argument(0))
			if s, ok := other.(string); ok {
				return e.vm.ToValue(s == display)
			}
			return e.vm.ToValue(other == goVal)
		})
	}
	e.wrapped[o] = goVal
	return o
}

func (e *jsEngine) toJS(v any) goja.Value {
	vm := e.vm
	switch t := v.(type) {
	case nil, bson.Null, bson.Undefined:
		return goja.Null()
	case bson.D:
		o := vm.NewObject()
		for _, kv := range t {
			_ = o.Set(kv.Key, e.toJS(kv.Value))
		}
		return o
	case bson.M:
		return e.toJS(mToD(t))
	case bson.A:
		vals := make([]any, len(t))
		for i, x := range t {
			vals[i] = e.toJS(x)
		}
		return vm.NewArray(vals...)
	case []any:
		return e.toJS(bson.A(t))
	case bson.ObjectID:
		return e.wrap(t, "ObjectId", t.Hex())
	case bson.DateTime:
		d, _ := vm.New(vm.Get("Date"), vm.ToValue(int64(t)))
		return d
	case time.Time:
		d, _ := vm.New(vm.Get("Date"), vm.ToValue(t.UnixMilli()))
		return d
	case bson.Decimal128:
		return e.wrap(t, "Decimal128", t.String())
	case bson.Binary:
		if t.Subtype == 4 && len(t.Data) == 16 {
			h := hex.EncodeToString(t.Data)
			return e.wrap(t, "UUID", h[0:8]+"-"+h[8:12]+"-"+h[12:16]+"-"+h[16:20]+"-"+h[20:])
		}
		return e.wrap(t, "Binary", hex.EncodeToString(t.Data))
	case bson.Timestamp:
		return e.wrap(t, "Timestamp", fmt.Sprintf("Timestamp(%d, %d)", t.T, t.I))
	case bson.Regex:
		r, err := vm.New(vm.Get("RegExp"), vm.ToValue(t.Pattern), vm.ToValue(t.Options))
		if err != nil {
			return vm.ToValue("/" + t.Pattern + "/" + t.Options)
		}
		return r
	case int32, int64, float64, string, bool, int:
		return vm.ToValue(t)
	}
	return vm.ToValue(fmt.Sprint(v))
}

func (e *jsEngine) toBSON(v goja.Value) any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	if o, ok := v.(*goja.Object); ok {
		if gv, ok := e.wrapped[o]; ok {
			return gv
		}
		switch o.ClassName() {
		case "Date":
			if t, ok := o.Export().(time.Time); ok {
				return bson.NewDateTimeFromTime(t)
			}
		case "RegExp":
			return bson.Regex{Pattern: o.Get("source").String(), Options: o.Get("flags").String()}
		case "Array":
			n := int(o.Get("length").ToInteger())
			arr := make(bson.A, n)
			for i := 0; i < n; i++ {
				arr[i] = e.toBSON(o.Get(fmt.Sprint(i)))
			}
			return arr
		case "Function":
			e.throw("a function can't be sent to MongoDB")
		}
		if _, isCur := e.cursors[o]; isCur {
			e.throw("a cursor can't be used as a value — call .toArray() first")
		}
		d := bson.D{}
		for _, k := range o.Keys() {
			if k == "toArray" {
				continue
			}
			d = append(d, bson.E{Key: k, Value: e.toBSON(o.Get(k))})
		}
		return d
	}
	switch t := v.Export().(type) {
	case int64:
		if t >= math.MinInt32 && t <= math.MaxInt32 {
			return int32(t)
		}
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			if t >= math.MinInt32 && t <= math.MaxInt32 {
				return int32(t)
			}
			return int64(t)
		}
		return t
	default:
		return t
	}
}

// inspect renders a value for print the way the result table shows it:
// documents as JSON, UUIDs as UUID('…'), ObjectIds as their hex, dates as
// ISO text; a top-level string-like value without quotes.
func (e *jsEngine) inspect(v goja.Value, pretty bool) string {
	val := plainValue(e.toBSON(v))
	var b []byte
	var err error
	if pretty {
		b, err = json.MarshalIndent(val, "", "  ")
	} else {
		b, err = json.Marshal(val)
	}
	if err != nil {
		return fmt.Sprint(val)
	}
	if len(b) > 1 && b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	return string(b)
}

// ScriptResults lists what a script produced as results: its print output
// first (one "output" column), then each shown value; a script that showed
// and printed nothing gets one row saying so.
func ScriptResults(o *ScriptOutcome) []ScriptResult {
	var out []ScriptResult
	if len(o.Output) > 0 {
		docs := make([]bson.D, len(o.Output))
		for i, line := range o.Output {
			docs[i] = bson.D{{Key: "output", Value: line}}
		}
		out = append(out, ScriptResult{Label: "print output", Cursor: &mongoStaticCursor{docs: docs, cols: unionMongoCols(docs)}})
	}
	out = append(out, o.Results...)
	if len(out) == 0 {
		docs := []bson.D{{{Key: "result", Value: "Script ran — it printed and returned nothing."}}}
		out = append(out, ScriptResult{Label: "script", Cursor: &mongoStaticCursor{docs: docs, cols: unionMongoCols(docs)}})
	}
	return out
}
