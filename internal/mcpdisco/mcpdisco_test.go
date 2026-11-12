package mcpdisco

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useTempConfig points Path() at a temp dir for the test.
func useTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := configDir
	configDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { configDir = old })
	return filepath.Join(dir, "PatchMan", "mcp.json")
}

// healthy starts a fake PatchMan backend answering /healthz.
func healthy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/mcp"
}

// deadURL is a loopback address nothing listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u + "/api/mcp"
}

func mustRead(t *testing.T) Info {
	t.Helper()
	i, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

// A second backend shutting down must not delete the entry of the app that
// announced itself after it (the bug that cut bridges off mid-session).
func TestRemoveIfOwnerKeepsOthersEntry(t *testing.T) {
	p := useTempConfig(t)
	if err := Write(Info{URL: "http://127.0.0.1:1/api/mcp", PID: 222}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIfOwner(111); err != nil {
		t.Fatal(err)
	}
	if mustRead(t).PID != 222 {
		t.Fatal("another backend's entry was removed")
	}
	if err := RemoveIfOwner(222); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("own entry should be gone, stat err = %v", err)
	}
	// Missing file: nothing to do. Unparseable file: removed.
	if err := RemoveIfOwner(222); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	if err := RemoveIfOwner(5); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("corrupt file should be removed")
	}
}

func TestKeepOnce(t *testing.T) {
	useTempConfig(t)
	me := Info{URL: healthy(t), PID: 100, Token: "pmcp_me", StartedAt: time.Now().UTC()}

	// Missing file → announce.
	if rewrote, err := keepOnce(me); err != nil || !rewrote || mustRead(t).PID != 100 {
		t.Fatalf("missing: rewrote=%v err=%v", rewrote, err)
	}
	// Our own entry → untouched.
	if rewrote, _ := keepOnce(me); rewrote {
		t.Fatal("own entry rewritten")
	}
	// Another live PatchMan announced itself → leave it (last started wins).
	other := Info{URL: healthy(t), PID: 200, Token: "pmcp_other"}
	_ = Write(other)
	if rewrote, _ := keepOnce(me); rewrote || mustRead(t).PID != 200 {
		t.Fatal("a live backend's entry was taken over")
	}
	// That one crashed (file left behind, nothing answers) → take over,
	// even though its PID may now belong to some unrelated process.
	crashed := Info{URL: deadURL(t), PID: os.Getpid(), Token: "pmcp_dead"}
	_ = Write(crashed)
	if rewrote, err := keepOnce(me); err != nil || !rewrote {
		t.Fatalf("crashed: rewrote=%v err=%v", rewrote, err)
	}
	if got := mustRead(t); got.PID != 100 || got.Token != "pmcp_me" {
		t.Fatalf("after takeover = %+v", got)
	}
	// Something answers on the port but isn't PatchMan (no /healthz 200).
	stranger := httptest.NewServer(http.NotFoundHandler())
	defer stranger.Close()
	_ = Write(Info{URL: stranger.URL + "/api/mcp", PID: 300})
	if rewrote, _ := keepOnce(me); !rewrote || mustRead(t).PID != 100 {
		t.Fatal("a non-PatchMan listener should not keep the entry")
	}
	// Garbage file → announce.
	p, _ := Path()
	_ = os.WriteFile(p, []byte("nope"), 0o600)
	if rewrote, _ := keepOnce(me); !rewrote || mustRead(t).PID != 100 {
		t.Fatal("corrupt file not replaced")
	}
}
