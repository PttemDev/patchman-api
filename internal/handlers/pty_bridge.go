package handlers

import (
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
)

// localShellChunk is the read buffer for the PTY→WS pump. Kept modest so a
// burst of output is delivered as several small stdout frames rather than one
// giant WriteJSON.
const localShellChunk = 4096

// bridgePTY upgrades the request to a WebSocket and bridges it to a child that
// is already started on ptmx: {type:stdin|resize} in, {type:stdout|exit} out.
// It owns ptmx and the child from here on and returns once the session is over.
func bridgePTY(c *gin.Context, cmd *exec.Cmd, ptmx *os.File) {
	wsConn, err := k8sExecUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}

	// teardown runs exactly once: close the PTY master (unblocks the read pump),
	// kill the child, close the socket.
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			_ = ptmx.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = wsConn.Close()
		})
	}
	defer teardown()

	writer := &wsWriter{conn: wsConn}
	done := make(chan struct{})

	_ = wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	})

	// Keepalive ping (single-writer via wsWriter mutex).
	go func() {
		t := time.NewTicker(execPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := writer.ping(); err != nil {
					return
				}
			}
		}
	}()

	// Reader goroutine: WS frames → PTY (stdin + resize).
	go func() {
		defer close(done)
		defer teardown()
		for {
			_, msg, rerr := wsConn.ReadMessage()
			if rerr != nil {
				return
			}
			var in execInbound
			if json.Unmarshal(msg, &in) != nil {
				continue
			}
			switch in.Type {
			case "stdin":
				if _, werr := ptmx.Write([]byte(in.Data)); werr != nil {
					return
				}
			case "resize":
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: in.Rows, Cols: in.Cols})
			}
		}
	}()

	// PTY → WS {type:stdout} pump. Returns on the child's exit (EOF) or teardown.
	// A read can end in the middle of a multi-byte character (ı, ş, ğ, emoji);
	// sent as is, JSON would turn its halves into U+FFFD. The incomplete tail is
	// held back and sent with the next read.
	buf := make([]byte, localShellChunk)
	var carry []byte
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			cut := completeUTF8(data)
			carry = append([]byte(nil), data[cut:]...)
			if cut > 0 {
				if werr := writer.writeJSON(execOutbound{Type: "stdout", Data: string(data[:cut])}); werr != nil {
					break
				}
			}
		}
		if rerr != nil {
			if len(carry) > 0 {
				_ = writer.writeJSON(execOutbound{Type: "stdout", Data: string(carry)})
			}
			break
		}
	}

	teardown()
	waitErr := cmd.Wait()
	out := execOutbound{Type: "exit", Code: 0}
	if waitErr != nil {
		out.Error = waitErr.Error()
		if ee, okee := waitErr.(*exec.ExitError); okee {
			out.Code = ee.ExitCode()
		} else {
			out.Code = 1
		}
	}
	_ = writer.writeJSON(out)
}

// completeUTF8 returns the length of b without a trailing, still incomplete
// UTF-8 sequence (at most 3 bytes are held back; invalid bytes pass through).
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}
