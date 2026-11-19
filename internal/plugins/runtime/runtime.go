// Package runtime runs plugin backends (PLUGIN_MARKETPLACE_RFC ADR-1, §14.4):
// Extism on wazero with Extism's own HTTP disabled, our pm_v1 host functions
// as the plugin's only capabilities, and:
//   - lazy compile on first call, LRU eviction under a memory budget;
//   - a per-plugin on-disk compilation cache (cold ~563 ms → warm ~20 ms, §14.3);
//   - an instance pool per (plugin, connection): connections never share an
//     instance, so one connection's secrets never sit in another's memory;
//   - a per-plugin concurrency cap and a per-call deadline.
package runtime

import (
	"container/list"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"patchman-be/internal/plugins/egress"
	"patchman-be/internal/plugins/manifest"
)

// wasiOutputEnv makes Extism wire guest stdout/stderr to the process. The
// desktop sidecar's stdout is the shell's pipe (1.58.1 crash class), so it
// must never be set when an instance is created (§14.4 item 7).
const wasiOutputEnv = "EXTISM_ENABLE_WASI_OUTPUT"

// ErrNotFound: the loader has no such plugin (not installed or disabled).
var ErrNotFound = errors.New("plugin not found")

// Loader returns an installed, enabled plugin's manifest and wasm.
type Loader func(ctx context.Context, pluginID string) (m *manifest.Manifest, wasm []byte, sha256 string, err error)

// Options configure a Runtime. Zero values pick the defaults below.
type Options struct {
	// CacheDir holds one compilation-cache directory per plugin; "" keeps
	// compiled code in memory only.
	CacheDir string
	// BudgetBytes bounds the estimated memory of compiled plugins (LRU).
	// Default 300 MB (§14.4 item 2, an assumption, not a measurement).
	BudgetBytes int64
	// RestrictPrivate / PrivateAllow: hosted egress policy (egress.Policy).
	RestrictPrivate bool
	PrivateAllow    []netip.Prefix
	MaxIdlePerConn  int           // idle instances kept per connection; default 2
	MaxCallsPerInst int           // an instance is retired after this many calls; default 1000
	IdleTTL         time.Duration // idle instances are closed after this; default 5 min
	// Log receives plugin log lines (already rate-limited per call).
	Log func(pluginID, connID, level, msg string)
	// NewGuard builds the egress guard for a connection. Tests swap it.
	NewGuard func(egress.Policy) *egress.Guard
}

// Connection is one configured plugin connection as the runtime sees it.
// Version changes whenever config or secrets change; pooled instances and
// HTTP clients are keyed by it, so an edit never reuses stale state.
type Connection struct {
	ID      string
	Version string
	Config  map[string]string
	Secrets map[string]string
}

// CallInfo is what a call reports besides its output.
type CallInfo struct {
	Logs     []string
	Duration time.Duration
	Reused   bool // served by a pooled instance
}

type Runtime struct {
	opts   Options
	loader Loader

	mu      sync.Mutex
	plugins map[string]*entry
	lru     *list.List // front = most recently used; values are *entry
	used    int64
	loading map[string]*loadWait
	gen     map[string]uint64 // per plugin; Invalidate bumps it
	closed  bool
	stop    chan struct{}
}

type loadWait struct {
	done chan struct{}
	e    *entry
	err  error
}

type entry struct {
	id       string
	sha      string
	m        *manifest.Manifest
	compiled *extism.CompiledPlugin
	cache    wazero.CompilationCache
	cacheDir string
	est      int64
	sem      chan struct{}
	elem     *list.Element

	mu      sync.Mutex
	refs    int                  // calls in flight
	dead    bool                 // evicted/invalidated: close once refs hits 0
	pools   map[string][]*pooled // key: connID + "\x00" + version
	clients map[string]*http.Client
	gen     map[string]int // per connection; DropConnection bumps it
}

type pooled struct {
	inst  *extism.Plugin
	conn  string
	gen   int
	calls int
	idle  time.Time
}

// New creates a runtime. Close it to release compiled code and instances.
func New(loader Loader, o Options) *Runtime {
	if o.BudgetBytes == 0 {
		o.BudgetBytes = 300 << 20
	}
	if o.MaxIdlePerConn == 0 {
		o.MaxIdlePerConn = 2
	}
	if o.MaxCallsPerInst == 0 {
		o.MaxCallsPerInst = 1000
	}
	if o.IdleTTL == 0 {
		o.IdleTTL = 5 * time.Minute
	}
	if o.Log == nil {
		o.Log = func(string, string, string, string) {}
	}
	if o.NewGuard == nil {
		o.NewGuard = egress.NewGuard
	}
	os.Unsetenv(wasiOutputEnv)
	rt := &Runtime{opts: o, loader: loader, plugins: map[string]*entry{}, lru: list.New(), loading: map[string]*loadWait{}, gen: map[string]uint64{}, stop: make(chan struct{})}
	go rt.reaper()
	return rt
}

// estimate is the budget cost of a compiled plugin: 1 MiB + 7× the wasm size.
// Calibrated on §14.3 (Go 2.3 MB → ~16 MB peak RSS, Rust 85 KB → ~1.9 MB);
// an estimate, not a measurement of this plugin.
func estimate(wasm int) int64 { return 1<<20 + 7*int64(wasm) }

// get returns the compiled entry with refs incremented; callers must release.
func (rt *Runtime) get(ctx context.Context, id string) (*entry, error) {
	for {
		rt.mu.Lock()
		if rt.closed {
			rt.mu.Unlock()
			return nil, errors.New("plugin runtime closed")
		}
		if e, ok := rt.plugins[id]; ok {
			rt.lru.MoveToFront(e.elem)
			e.mu.Lock()
			e.refs++
			e.mu.Unlock()
			rt.mu.Unlock()
			return e, nil
		}
		if w, ok := rt.loading[id]; ok {
			rt.mu.Unlock()
			select {
			case <-w.done:
				if w.err != nil {
					return nil, w.err
				}
				continue // re-check under the lock (it may be evicted already)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		w := &loadWait{done: make(chan struct{})}
		rt.loading[id] = w
		startGen := rt.gen[id]
		rt.mu.Unlock()

		e, err := rt.compile(ctx, id)
		rt.mu.Lock()
		delete(rt.loading, id)
		if err == nil && rt.gen[id] != startGen {
			// Invalidated while compiling: the loader may have answered with
			// the previous manifest. Drop it and load again.
			close(w.done)
			rt.mu.Unlock()
			e.close()
			continue
		}
		if err == nil {
			e.elem = rt.lru.PushFront(e)
			rt.plugins[id] = e
			rt.used += e.est
			rt.evictLocked(e)
		}
		w.err = err
		close(w.done)
		rt.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (rt *Runtime) compile(ctx context.Context, id string) (*entry, error) {
	m, wasm, sha, err := rt.loader(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(wasm) > manifest.MaxWasmBytes {
		return nil, fmt.Errorf("plugin %s: wasm is %d bytes, cap %d", id, len(wasm), manifest.MaxWasmBytes)
	}
	var cc wazero.CompilationCache
	dir := ""
	if rt.opts.CacheDir != "" {
		dir = filepath.Join(rt.opts.CacheDir, safeDir(id))
		if cc, err = wazero.NewCompilationCacheWithDir(dir); err != nil {
			return nil, err
		}
	} else {
		cc = wazero.NewCompilationCache()
	}
	pages := uint32(m.Limits.MemoryMB * 16) // 64 KiB pages
	em := extism.Manifest{
		Wasm:         []extism.Wasm{extism.WasmData{Data: wasm, Hash: sha}},
		AllowedHosts: nil, // Extism's http_request stays deny-all; pm_v1.http_request is the only path
		Memory:       &extism.ManifestMemory{MaxPages: pages, MaxHttpResponseBytes: 0, MaxVarBytes: 64 << 10},
		Timeout:      uint64(time.Duration(m.Limits.TimeoutSeconds) * time.Second / time.Millisecond),
	}
	rc := wazero.NewRuntimeConfig().
		WithCompilationCache(cc).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages)
	cp, err := extism.NewCompiledPlugin(ctx, em, extism.PluginConfig{RuntimeConfig: rc, EnableWasi: true}, hostFunctions())
	if err != nil {
		cc.Close(context.Background())
		return nil, fmt.Errorf("plugin %s: compile: %w", id, err)
	}
	return &entry{
		id: id, sha: sha, m: m, compiled: cp, cache: cc, cacheDir: dir,
		est: estimate(len(wasm)), sem: make(chan struct{}, m.Limits.Concurrency),
		pools: map[string][]*pooled{}, clients: map[string]*http.Client{}, gen: map[string]int{},
	}, nil
}

func safeDir(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, id)
}

// evictLocked drops least-recently-used plugins until the budget fits. The
// just-loaded entry is never evicted, so one plugin larger than the budget
// still runs (alone).
func (rt *Runtime) evictLocked(keep *entry) {
	for el := rt.lru.Back(); el != nil && rt.used > rt.opts.BudgetBytes; {
		prev := el.Prev()
		if e := el.Value.(*entry); e != keep {
			rt.removeLocked(e)
		}
		el = prev
	}
}

// removeLocked unlinks e; it is closed now or when its last call ends.
func (rt *Runtime) removeLocked(e *entry) {
	if rt.plugins[e.id] != e {
		return
	}
	delete(rt.plugins, e.id)
	rt.lru.Remove(e.elem)
	rt.used -= e.est
	e.mu.Lock()
	e.dead = true
	idle := e.refs == 0
	e.mu.Unlock()
	if idle {
		e.close()
	}
}

func (e *entry) release() {
	e.mu.Lock()
	e.refs--
	closeNow := e.dead && e.refs == 0
	e.mu.Unlock()
	if closeNow {
		e.close()
	}
}

func (e *entry) close() {
	e.mu.Lock()
	pools := e.pools
	e.pools = map[string][]*pooled{}
	clients := e.clients
	e.clients = map[string]*http.Client{}
	e.mu.Unlock()
	for _, ps := range pools {
		for _, p := range ps {
			p.inst.Close(context.Background())
		}
	}
	for _, c := range clients {
		c.CloseIdleConnections()
	}
	e.compiled.Close(context.Background())
	e.cache.Close(context.Background())
}

// Invalidate drops a plugin's compiled code, instances and clients (update,
// disable). With purgeCache it also deletes the on-disk compilation cache
// (uninstall).
func (rt *Runtime) Invalidate(pluginID string, purgeCache bool) {
	rt.mu.Lock()
	rt.gen[pluginID]++
	if e, ok := rt.plugins[pluginID]; ok {
		rt.removeLocked(e)
	}
	rt.mu.Unlock()
	if purgeCache && rt.opts.CacheDir != "" {
		os.RemoveAll(filepath.Join(rt.opts.CacheDir, safeDir(pluginID)))
	}
}

// DropConnection closes a connection's pooled instances and HTTP client
// (connection edited or deleted, grant revoked).
func (rt *Runtime) DropConnection(pluginID, connID string) {
	rt.mu.Lock()
	e := rt.plugins[pluginID]
	rt.mu.Unlock()
	if e == nil {
		return
	}
	var drop []*pooled
	e.mu.Lock()
	e.gen[connID]++ // instances in flight now are closed when they come back
	for k, ps := range e.pools {
		if strings.HasPrefix(k, connID+"\x00") {
			drop = append(drop, ps...)
			delete(e.pools, k)
		}
	}
	for k, c := range e.clients {
		if strings.HasPrefix(k, connID+"\x00") {
			c.CloseIdleConnections()
			delete(e.clients, k)
		}
	}
	e.mu.Unlock()
	for _, p := range drop {
		p.inst.Close(context.Background())
	}
}

// Close releases everything. Calls in flight finish first.
func (rt *Runtime) Close() {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	rt.closed = true
	close(rt.stop)
	for _, e := range rt.plugins {
		rt.removeLocked(e)
	}
	rt.mu.Unlock()
}

// Loaded reports the plugins currently compiled and the budget in use.
func (rt *Runtime) Loaded() (ids []string, used int64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for el := rt.lru.Front(); el != nil; el = el.Next() {
		ids = append(ids, el.Value.(*entry).id)
	}
	return ids, rt.used
}

func (rt *Runtime) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rt.stop:
			return
		case <-t.C:
			rt.reapIdle(time.Now())
		}
	}
}

func (rt *Runtime) reapIdle(now time.Time) {
	rt.mu.Lock()
	es := make([]*entry, 0, len(rt.plugins))
	for _, e := range rt.plugins {
		es = append(es, e)
	}
	rt.mu.Unlock()
	for _, e := range es {
		var drop []*pooled
		e.mu.Lock()
		for k, ps := range e.pools {
			keep := ps[:0]
			for _, p := range ps {
				if now.Sub(p.idle) > rt.opts.IdleTTL {
					drop = append(drop, p)
				} else {
					keep = append(keep, p)
				}
			}
			if len(keep) == 0 {
				delete(e.pools, k)
			} else {
				e.pools[k] = keep
			}
		}
		e.mu.Unlock()
		for _, p := range drop {
			p.inst.Close(context.Background())
		}
	}
}

// policy resolves the manifest's net rules against this connection: fixed
// hosts as declared, "from: connection.<field>" hosts from the config.
func (rt *Runtime) policy(m *manifest.Manifest, conn *Connection) (egress.Policy, error) {
	pol := egress.Policy{RestrictPrivate: rt.opts.RestrictPrivate, PrivateAllow: rt.opts.PrivateAllow}
	for _, r := range m.Permissions.Net {
		host := r.Host
		if r.From != "" {
			v := conn.Config[manifest.RefField(r.From)]
			if strings.TrimSpace(v) == "" {
				continue // field not set on this connection: rule grants nothing
			}
			h, err := manifest.NormalizeHost(v, false)
			if err != nil {
				return pol, fmt.Errorf("connection %s: %w", manifest.RefField(r.From), err)
			}
			host = h
		}
		ports := r.Ports
		if r.PortsFrom != "" {
			p, err := manifest.ParsePort(conn.Config[manifest.RefField(r.PortsFrom)])
			if err != nil {
				return pol, fmt.Errorf("connection %s: %w", manifest.RefField(r.PortsFrom), err)
			}
			ports = []int{p}
		}
		pol.Rules = append(pol.Rules, egress.Rule{Host: host, Ports: ports, Private: r.Private})
	}
	return pol, nil
}

func (e *entry) client(rt *Runtime, conn *Connection, key string) (*http.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.clients[key]; ok {
		return c, nil
	}
	pol, err := rt.policy(e.m, conn)
	if err != nil {
		return nil, err
	}
	c := rt.opts.NewGuard(pol).HTTPClient(egress.HTTPLimits{})
	e.clients[key] = c
	return c, nil
}

func (e *entry) take(key string) *pooled {
	e.mu.Lock()
	defer e.mu.Unlock()
	ps := e.pools[key]
	if len(ps) == 0 {
		return nil
	}
	p := ps[len(ps)-1]
	e.pools[key] = ps[:len(ps)-1]
	return p
}

// put returns an instance to the pool, or closes it.
func (rt *Runtime) put(e *entry, key string, p *pooled) {
	e.mu.Lock()
	if !e.dead && p.gen == e.gen[p.conn] && p.calls < rt.opts.MaxCallsPerInst && len(e.pools[key]) < rt.opts.MaxIdlePerConn {
		p.idle = time.Now()
		e.pools[key] = append(e.pools[key], p)
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	p.inst.Close(context.Background())
}

func (e *entry) newInstance(ctx context.Context) (*extism.Plugin, error) {
	if os.Getenv(wasiOutputEnv) != "" {
		os.Unsetenv(wasiOutputEnv) // something set it after startup; never honour it
	}
	mc := wazero.NewModuleConfig(). // no FS, no env, no args, stdout/stderr discarded
					WithSysWalltime().WithSysNanotime().WithSysNanosleep().WithRandSource(rand.Reader)
	return e.compiled.Instance(ctx, extism.PluginInstanceConfig{ModuleConfig: mc})
}

// Manifest returns the manifest of a plugin as the runtime would run it.
func (rt *Runtime) Manifest(ctx context.Context, pluginID string) (*manifest.Manifest, error) {
	e, err := rt.get(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	defer e.release()
	return e.m, nil
}

// Call runs op for one connection. The op must be declared in the manifest.
func (rt *Runtime) Call(ctx context.Context, pluginID string, conn *Connection, op string, input []byte) ([]byte, CallInfo, error) {
	start := time.Now()
	info := CallInfo{}
	e, err := rt.get(ctx, pluginID)
	if err != nil {
		return nil, info, err
	}
	defer e.release()
	if _, ok := e.m.Ops[op]; !ok {
		return nil, info, fmt.Errorf("plugin %s: op %q is not declared", pluginID, op)
	}
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, info, ctx.Err()
	}
	key := conn.ID + "\x00" + conn.Version
	client, err := e.client(rt, conn, key)
	if err != nil {
		return nil, info, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.m.Limits.TimeoutSeconds)*time.Second)
	defer cancel()

	p := e.take(key)
	info.Reused = p != nil
	if p == nil {
		inst, err := e.newInstance(ctx)
		if err != nil {
			return nil, info, fmt.Errorf("plugin %s: instance: %w", pluginID, err)
		}
		e.mu.Lock()
		gen := e.gen[conn.ID]
		e.mu.Unlock()
		p = &pooled{inst: inst, conn: conn.ID, gen: gen}
	}
	st := &callState{rt: rt, e: e, conn: conn, client: client}
	p.inst.SetLogger(func(l extism.LogLevel, msg string) { st.log(l.String(), msg) })
	cctx := context.WithValue(ctx, stateKey{}, st)
	rc, out, err := p.inst.CallWithContext(cctx, op, input)
	p.calls++
	info.Logs = st.logs
	info.Duration = time.Since(start)
	if err != nil {
		p.inst.Close(context.Background()) // a failed or killed instance is never reused
		if ctx.Err() != nil {
			return nil, info, fmt.Errorf("plugin %s/%s: %w", pluginID, op, ctx.Err())
		}
		return nil, info, fmt.Errorf("plugin %s/%s (rc=%d): %w", pluginID, op, rc, err)
	}
	out = append([]byte(nil), out...) // the instance's memory is reused
	rt.put(e, key, p)
	return out, info, nil
}

// Validate compiles wasm against the pm_v1 ABI without keeping it: unknown
// imports, a bad module or an oversized file fail here, at install time.
func Validate(ctx context.Context, m *manifest.Manifest, wasm []byte) error {
	if len(wasm) > manifest.MaxWasmBytes {
		return fmt.Errorf("wasm is %d bytes, cap %d", len(wasm), manifest.MaxWasmBytes)
	}
	pages := uint32(m.Limits.MemoryMB * 16)
	cc := wazero.NewCompilationCache()
	defer cc.Close(context.Background())
	rc := wazero.NewRuntimeConfig().WithCompilationCache(cc).WithMemoryLimitPages(pages)
	em := extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: wasm}}, Memory: &extism.ManifestMemory{MaxPages: pages}}
	cp, err := extism.NewCompiledPlugin(ctx, em, extism.PluginConfig{RuntimeConfig: rc, EnableWasi: true}, hostFunctions())
	if err != nil {
		return err
	}
	defer cp.Close(context.Background())

	// Imports: only the ABIs we provide. Exports: every declared op.
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	defer r.Close(context.Background())
	cm, err := r.CompileModule(ctx, wasm)
	if err != nil {
		return err
	}
	for _, f := range cm.ImportedFunctions() {
		mod, name, _ := f.Import()
		if !allowedImportModules[mod] {
			return fmt.Errorf("imports %s.%s: only pm_v1, extism:host/env and wasi_snapshot_preview1 are available", mod, name)
		}
		if mod == "pm_v1" && !pmV1Names[name] {
			return fmt.Errorf("imports pm_v1.%s: not in this PatchMan's pm_v1 ABI", name)
		}
	}
	exports := cm.ExportedFunctions()
	for op := range m.Ops {
		if _, ok := exports[op]; !ok {
			return fmt.Errorf("op %q is declared but not exported by the wasm", op)
		}
	}
	return nil
}

var allowedImportModules = map[string]bool{"pm_v1": true, "extism:host/env": true, "wasi_snapshot_preview1": true}
