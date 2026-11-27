package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// startBridge serves a route that starts /bin/sh on a PTY and hands it to
// bridgePTY, then dials it. Returns the client socket and the shell's pid.
func startBridge(t *testing.T) (*websocket.Conn, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	pids := make(chan int, 1)
	r := gin.New()
	r.GET("/t", func(c *gin.Context) {
		cmd := exec.Command("/bin/sh")
		ptmx, err := pty.Start(cmd)
		if err != nil {
			t.Errorf("pty.Start: %v", err)
			return
		}
		pids <- cmd.Process.Pid
		bridgePTY(c, cmd, ptmx)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/t", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case pid := <-pids:
		return conn, pid
	case <-time.After(5 * time.Second):
		t.Fatal("shell was not started")
		return nil, 0
	}
}

// readUntil collects stdout frames until the text contains substr.
func readUntil(t *testing.T, conn *websocket.Conn, substr string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got strings.Builder
	for !strings.Contains(got.String(), substr) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q, got %q: %v", substr, got.String(), err)
		}
		var out execOutbound
		if json.Unmarshal(msg, &out) == nil && out.Type == "stdout" {
			got.WriteString(out.Data)
		}
	}
}

func TestBridgePTYStdinReachesShellAndStdoutComesBack(t *testing.T) {
	conn, _ := startBridge(t)
	if err := conn.WriteJSON(map[string]any{"type": "resize", "cols": 100, "rows": 30}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "stdin", "data": "echo pm-$((40+2))\n"}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, conn, "pm-42")
}

func TestBridgePTYClosesSocketWhenShellExits(t *testing.T) {
	conn, _ := startBridge(t)
	if err := conn.WriteJSON(map[string]any{"type": "stdin", "data": "exit 3\n"}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatalf("socket not closed after shell exit: %v", err)
			}
			return
		}
	}
}

func TestBridgePTYKillsShellWhenClientCloses(t *testing.T) {
	conn, pid := startBridge(t)
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("shell pid %d still alive after client close", pid)
}

// A read that ends inside a multi-byte character holds its first bytes back
// until the rest arrives, so the stdout frames never carry U+FFFD.
func TestCompleteUTF8(t *testing.T) {
	s := []byte("aış🙂")
	cases := []struct {
		in   []byte
		want int
	}{
		{s, len(s)},
		{s[:2], 1},                 // "a" + first byte of ı
		{s[:3], 3},                 // "aı"
		{s[:len(s)-1], len(s) - 4}, // emoji missing its last byte
		{s[:len(s)-3], len(s) - 4}, // emoji with only its first byte
		{[]byte{0xff, 'x'}, 2},     // invalid bytes pass through
		{nil, 0},
	}
	for _, c := range cases {
		if got := completeUTF8(c.in); got != c.want {
			t.Errorf("completeUTF8(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
