package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"patchman-be/internal/plugins/egress"
	"patchman-be/internal/plugins/manifest"
)

var demoWasm []byte

func TestMain(m *testing.M) {
	// PM_TEST_GUEST_WASM runs the suite against a prebuilt guest (e.g. the
	// demo built with TinyGo) instead of building it with go.
	if p := os.Getenv("PM_TEST_GUEST_WASM"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			panic(err)
		}
		demoWasm = b
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "pmrt")
	if err != nil {
		panic(err)
	}
	out := filepath.Join(dir, "demo.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./cmd/demo")
	cmd.Dir = "testdata/guest"
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build demo guest: %v\n%s", err, b)
		os.Exit(1)
	}
	if demoWasm, err = os.ReadFile(out); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var demoOps = []string{"nap", "echo", "whoami", "config", "secret", "counter", "fail", "fetch", "spin", "membomb", "extism_http", "wasi", "stdout_flood", "log_flood", "badptr"}

func demoManifest(t *testing.T, id string, extra string) *manifest.Manifest {
	t.Helper()
	ops := make([]string, len(demoOps))
	for i, o := range demoOps {
		ops[i] = fmt.Sprintf("%q: {}", o)
	}
	raw := fmt.Sprintf(`{"id": %q, "version": "1.0.0", "name": "demo", "publisher": "test",
	  "connection": {"secretFields": ["token"]},
	  "permissions": {"net": [{"host": "api.test", "ports": [80]}, {"from": "connection.host", "ports": [80]}]},
	  %s
	  "ops": {%s}}`, id, extra, strings.Join(ops, ","))
	m, err := manifest.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type fakeResolver map[string]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if ip, ok := f[host]; ok {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	return nil, errors.New("nxdomain")
}

// testRT serves each manifest with the demo wasm. Egress goes through the real
// guard; checked public test IPs are pointed at httptest servers.
func testRT(t *testing.T, o Options, ms ...*manifest.Manifest) (*Runtime, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host+r.URL.Path+" auth="+r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	target := strings.TrimPrefix(srv.URL, "http://")
	res := fakeResolver{"api.test": "203.0.113.1", "a.test": "203.0.113.2", "b.test": "203.0.113.3", "lo.test": "127.0.0.1"}
	o.NewGuard = func(p egress.Policy) *egress.Guard {
		return egress.NewGuardWith(p, res, func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, target) // only ever reached with a checked IP
		})
	}
	byID := map[string]*manifest.Manifest{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	sum := sha256.Sum256(demoWasm)
	rt := New(func(_ context.Context, id string) (*manifest.Manifest, []byte, string, error) {
		m, ok := byID[id]
		if !ok {
			return nil, nil, "", ErrNotFound
		}
		return m, demoWasm, hex.EncodeToString(sum[:]), nil
	}, o)
	t.Cleanup(rt.Close)
	return rt, srv
}

func conn(id, token string, cfg map[string]string) *Connection {
	if cfg == nil {
		cfg = map[string]string{}
	}
	return &Connection{ID: id, Version: "1", Config: cfg, Secrets: map[string]string{"token": token}}
}

func call(t *testing.T, rt *Runtime, id string, c *Connection, op, in string) string {
	t.Helper()
	out, _, err := rt.Call(context.Background(), id, c, op, []byte(in))
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return string(out)
}

func TestEchoPoolAndConnectionIsolation(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", "")
	rt, _ := testRT(t, Options{}, m)
	a, b := conn("A", "tokA", nil), conn("B", "tokB", nil)
	if got := call(t, rt, m.ID, a, "echo", "hi"); got != "hi" {
		t.Fatalf("echo = %q", got)
	}
	if call(t, rt, m.ID, a, "counter", "") != "1" || call(t, rt, m.ID, a, "counter", "") != "2" {
		t.Fatal("instance not reused within a connection")
	}
	if got := call(t, rt, m.ID, b, "counter", ""); got != "1" {
		t.Fatalf("connection B shares A's instance: counter=%s", got)
	}
	// Edited connection (new version) gets a fresh instance.
	a2 := conn("A", "tokA", nil)
	a2.Version = "2"
	if got := call(t, rt, m.ID, a2, "counter", ""); got != "1" {
		t.Fatalf("edited connection reused stale instance: %s", got)
	}
	rt.DropConnection(m.ID, "B")
	if got := call(t, rt, m.ID, b, "counter", ""); got != "1" {
		t.Fatalf("dropped connection kept its instance: %s", got)
	}
}

func TestSecretIsolationConcurrent(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", `"limits": {"concurrency": 16},`)
	rt, _ := testRT(t, Options{}, m)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i%4)
			out, _, err := rt.Call(context.Background(), m.ID, conn(id, "tok-"+id, nil), "whoami", nil)
			if err != nil {
				errs <- err
			} else if string(out) != "tok-"+id {
				errs <- fmt.Errorf("conn %s saw %q", id, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSecretsAndConfigBoundaries(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", "")
	rt, _ := testRT(t, Options{}, m)
	c := &Connection{ID: "A", Version: "1",
		Config:  map[string]string{"host": "a.test", "token": "leaked-into-config"},
		Secrets: map[string]string{"token": "tok", "password": "pw"}}
	if got := call(t, rt, m.ID, c, "secret", "token"); got != "tok" {
		t.Errorf("declared secret = %q", got)
	}
	if got := call(t, rt, m.ID, c, "secret", "password"); got != "!" {
		t.Errorf("undeclared secret returned %q", got)
	}
	if got := call(t, rt, m.ID, c, "config", "token"); got != "!" {
		t.Errorf("config_get returned a secret field: %q", got)
	}
	if got := call(t, rt, m.ID, c, "config", "host"); got != "a.test" {
		t.Errorf("config host = %q", got)
	}
	if _, _, err := rt.Call(context.Background(), m.ID, c, "nope", nil); err == nil {
		t.Error("undeclared op ran")
	}
}

func TestEgressPerConnection(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", "")
	rt, _ := testRT(t, Options{}, m)
	a := conn("A", "tok", map[string]string{"host": "a.test"})
	if got := call(t, rt, m.ID, a, "fetch", "http://api.test/x"); !strings.HasPrefix(got, "OK 200 api.test/x") {
		t.Errorf("fixed host: %q", got)
	}
	if got := call(t, rt, m.ID, a, "fetch", "http://a.test/y"); !strings.HasPrefix(got, "OK 200 a.test/y") {
		t.Errorf("connection host: %q", got)
	}
	for _, u := range []string{"http://b.test/", "http://lo.test/", "http://127.0.0.1/", "http://api.test:8080/", "file:///etc/passwd", "http://u:p@api.test/"} {
		if got := call(t, rt, m.ID, a, "fetch", u); !strings.HasPrefix(got, "ERR") {
			t.Errorf("%s: %q", u, got)
		}
	}
	// Same plugin, other connection: its own host only.
	b := conn("B", "tok", map[string]string{"host": "b.test"})
	if got := call(t, rt, m.ID, b, "fetch", "http://a.test/"); !strings.HasPrefix(got, "ERR") {
		t.Errorf("B reached A's host: %q", got)
	}
	// Edited connection: new host applies, old one no longer does.
	a2 := conn("A", "tok", map[string]string{"host": "b.test"})
	a2.Version = "2"
	if got := call(t, rt, m.ID, a2, "fetch", "http://a.test/"); !strings.HasPrefix(got, "ERR") {
		t.Errorf("edited connection kept the old host: %q", got)
	}
	// Bad host in config fails the call instead of widening anything.
	bad := conn("C", "tok", map[string]string{"host": "https://evil/"})
	if _, _, err := rt.Call(context.Background(), m.ID, bad, "fetch", []byte("http://api.test/")); err == nil {
		t.Error("malformed connection host accepted")
	}
}

func TestSandboxEscapes(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", `"limits": {"timeoutSeconds": 1, "memoryMB": 64},`)
	rt, _ := testRT(t, Options{}, m)
	c := conn("A", "tok", nil)
	start := time.Now()
	if _, _, err := rt.Call(context.Background(), m.ID, c, "spin", nil); err == nil {
		t.Error("spin returned")
	}
	if d := time.Since(start); d > 3*time.Second && !raceEnabled {
		t.Errorf("spin stopped after %v", d)
	}
	if _, _, err := rt.Call(context.Background(), m.ID, c, "membomb", nil); err == nil {
		t.Error("membomb returned")
	}
	if got := call(t, rt, m.ID, c, "echo", "ok"); got != "ok" {
		t.Errorf("runtime unusable after a killed call: %q", got)
	}
	if got := call(t, rt, m.ID, c, "wasi", ""); got != "file:true,dir:true/0,env:0,dial:true" {
		t.Errorf("wasi = %q", got)
	}
	if out, _, err := rt.Call(context.Background(), m.ID, c, "extism_http", nil); err == nil && string(out) == "200" {
		t.Error("Extism HTTP reached the network")
	}
	if _, _, err := rt.Call(context.Background(), m.ID, c, "fail", nil); err == nil {
		t.Error("fail returned no error")
	}
	if got := call(t, rt, m.ID, c, "badptr", ""); !strings.Contains(got, "bad request") {
		t.Errorf("badptr = %q", got)
	}
	_, info, err := rt.Call(context.Background(), m.ID, c, "log_flood", nil)
	if (err != nil && !raceEnabled) || len(info.Logs) != maxLogsPerCall {
		t.Errorf("log_flood: err=%v logs=%d", err, len(info.Logs))
	}
}

func TestWasiOutputEnvNeverHonoured(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", "")
	rt, _ := testRT(t, Options{}, m)
	t.Setenv(wasiOutputEnv, "1")
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	_, _, err := rt.Call(context.Background(), m.ID, conn("A", "t", nil), "stdout_flood", nil)
	os.Stdout = stdout
	w.Close()
	leaked, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) > 0 {
		t.Errorf("guest stdout reached the process: %d bytes", len(leaked))
	}
	if os.Getenv(wasiOutputEnv) != "" {
		t.Error("env var not cleared before the instance")
	}
}

func TestConcurrencyCap(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", `"limits": {"concurrency": 1, "timeoutSeconds": 2},`)
	rt, _ := testRT(t, Options{}, m)
	go rt.Call(context.Background(), m.ID, conn("A", "t", nil), "spin", nil)
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := rt.Call(ctx, m.ID, conn("B", "t", nil), "echo", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second call should wait for the slot, got %v", err)
	}
}

func TestLRUAndCache(t *testing.T) {
	m1 := demoManifest(t, "dev.test.one", "")
	m2 := demoManifest(t, "dev.test.two", "")
	cacheDir := t.TempDir()
	// Budget fits one demo plugin only.
	rt, _ := testRT(t, Options{CacheDir: cacheDir, BudgetBytes: estimate(len(demoWasm)) + 1}, m1, m2)
	c := conn("A", "t", nil)
	call(t, rt, m1.ID, c, "echo", "1")
	call(t, rt, m2.ID, c, "echo", "2")
	ids, used := rt.Loaded()
	if len(ids) != 1 || ids[0] != m2.ID || used != estimate(len(demoWasm)) {
		t.Fatalf("after LRU: loaded=%v used=%d", ids, used)
	}
	if got := call(t, rt, m1.ID, c, "echo", "again"); got != "again" {
		t.Fatalf("evicted plugin did not reload: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "dev.test.one")); err != nil {
		t.Fatalf("no compile cache dir: %v", err)
	}
	rt.Invalidate(m1.ID, true)
	if _, err := os.Stat(filepath.Join(cacheDir, "dev.test.one")); !os.IsNotExist(err) {
		t.Errorf("uninstall left the compile cache: %v", err)
	}
	if _, _, err := rt.Call(context.Background(), "dev.test.missing", c, "echo", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing plugin: %v", err)
	}
}

func TestWasmSizeCap(t *testing.T) {
	m := demoManifest(t, "dev.test.big", "")
	rt := New(func(context.Context, string) (*manifest.Manifest, []byte, string, error) {
		return m, make([]byte, manifest.MaxWasmBytes+1), "x", nil
	}, Options{})
	defer rt.Close()
	if _, _, err := rt.Call(context.Background(), m.ID, conn("A", "t", nil), "echo", nil); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("oversized wasm: %v", err)
	}
}

func TestValidate(t *testing.T) {
	m := demoManifest(t, "dev.test.demo", "")
	if err := Validate(context.Background(), m, demoWasm); err != nil {
		t.Fatalf("demo wasm: %v", err)
	}
	m2 := demoManifest(t, "dev.test.demo", "")
	m2.Ops["not_exported"] = manifest.Op{}
	if err := Validate(context.Background(), m2, demoWasm); err == nil || !strings.Contains(err.Error(), "not exported") {
		t.Errorf("missing export: %v", err)
	}
	if err := Validate(context.Background(), m, []byte("\x00asm garbage")); err == nil {
		t.Error("garbage wasm accepted")
	}
	// Minimal module importing a pm_v1 function this build does not provide.
	bad := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type: () -> ()
		0x02, 0x12, 0x01, 0x05, 'p', 'm', '_', 'v', '1', 0x08, 't', 'c', 'p', '_', 'd', 'i', 'a', 'l', 0x00, 0x00}
	mt := demoManifest(t, "dev.test.demo", "")
	mt.Ops = map[string]manifest.Op{}
	if err := Validate(context.Background(), mt, bad); err == nil || !strings.Contains(err.Error(), "tcp_dial") {
		t.Errorf("unknown pm_v1 import: %v", err)
	}
}

// A reinstall that narrows permissions while a load is in flight must not
// leave the old manifest compiled in.
func TestInvalidateDuringLoad(t *testing.T) {
	wide := demoManifest(t, "dev.test.demo", "")
	narrow := demoManifest(t, "dev.test.demo", "")
	narrow.Permissions.Net = []manifest.NetRule{{Host: "b.test", Ports: []int{80}}}
	var mu sync.Mutex
	cur := wide
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	rt, _ := testRT(t, Options{}, wide)
	sum := sha256.Sum256(demoWasm)
	rt.loader = func(_ context.Context, id string) (*manifest.Manifest, []byte, string, error) {
		mu.Lock()
		m, block := cur, first
		first = false
		mu.Unlock()
		if block {
			close(entered)
			<-release
		}
		return m, demoWasm, hex.EncodeToString(sum[:]), nil
	}
	done := make(chan string)
	go func() {
		out, _, err := rt.Call(context.Background(), wide.ID, conn("A", "t", nil), "fetch", []byte("http://api.test/"))
		done <- fmt.Sprint(string(out), err)
	}()
	<-entered
	mu.Lock()
	cur = narrow
	mu.Unlock()
	rt.Invalidate(wide.ID, false)
	close(release)
	if got := <-done; !strings.HasPrefix(got, "ERR") {
		t.Fatalf("call used the invalidated manifest: %q", got)
	}
}

// time.Sleep in a guest must block the call, not spin a core: wazero needs
// WithSysNanosleep, without it a sleeping plugin burned ~50% CPU (§14.10).
func TestGuestSleepDoesNotSpin(t *testing.T) {
	if raceEnabled {
		t.Skip("CPU budget is meaningless under -race")
	}
	m := demoManifest(t, "dev.test.demo", "")
	rt, _ := testRT(t, Options{}, m)
	c := conn("A", "t", nil)
	call(t, rt, m.ID, c, "echo", "warm") // compile outside the measurement
	var before, after syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	start := time.Now()
	call(t, rt, m.ID, c, "nap", "")
	wall := time.Since(start)
	syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	cpu := time.Duration(after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano())
	if wall < 900*time.Millisecond {
		t.Fatalf("nap returned after %v", wall)
	}
	if cpu > 300*time.Millisecond {
		t.Errorf("1 s guest sleep used %v CPU", cpu)
	}
	t.Logf("nap: wall=%v cpu=%v", wall.Round(time.Millisecond), cpu.Round(time.Millisecond))
}
