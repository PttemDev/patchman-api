package k8sclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseData returns the `data:` payloads of the SSE output, in order.
func sseData(out string) []string {
	var got []string
	for _, frame := range strings.Split(out, "\n\n") {
		if strings.HasPrefix(frame, "data: ") {
			got = append(got, strings.TrimPrefix(frame, "data: "))
		}
	}
	return got
}

func runCopy(t *testing.T, heartbeat time.Duration, feed func(w *io.PipeWriter)) (string, error) {
	t.Helper()
	pr, pw := io.Pipe()
	go feed(pw)
	var buf lockedBuf
	err := copyLogLines(context.Background(), pr, &buf, nil, heartbeat)
	return buf.String(), err
}

// Lines split across reads are held until their newline, CRLF loses the CR,
// a final line without newline still comes out, order is preserved.
func TestK8sLogCopy_PartialLinesAndOrder(t *testing.T) {
	out, err := runCopy(t, 0, func(w *io.PipeWriter) {
		for _, chunk := range []string{"2026-11-11T10:00:00.1Z al", "pha\n2026-11-11T10:00:00.2Z beta\r\n", "", "gam", "ma\n\ndelta"} {
			_, _ = w.Write([]byte(chunk))
		}
		_ = w.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-11T10:00:00.1Z alpha", "2026-11-11T10:00:00.2Z beta", "gamma", "", "delta"}
	if got := sseData(out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A line longer than the cap comes out in consecutive pieces — nothing lost,
// and the stream keeps going (a bufio.Scanner used to abort it).
func TestK8sLogCopy_LongLineIsSplitNotFatal(t *testing.T) {
	long := strings.Repeat("x", maxLogLineBytes+12345)
	out, err := runCopy(t, 0, func(w *io.PipeWriter) {
		_, _ = w.Write([]byte("before\n" + long + "\nafter\n"))
		_ = w.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	got := sseData(out)
	if got[0] != "before" || got[len(got)-1] != "after" {
		t.Fatalf("first/last = %q/%q", got[0], got[len(got)-1])
	}
	if joined := strings.Join(got[1:len(got)-1], ""); joined != long {
		t.Fatalf("long line pieces rejoin to %d bytes, want %d", len(joined), len(long))
	}
}

// Many lines written as fast as possible arrive complete and in order.
func TestK8sLogCopy_BurstKeepsOrder(t *testing.T) {
	const n = 20000
	out, err := runCopy(t, time.Millisecond, func(w *io.PipeWriter) {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "line %d\n", i)
			if sb.Len() > 3000 { // odd-sized writes split lines across reads
				_, _ = w.Write([]byte(sb.String()))
				sb.Reset()
			}
		}
		_, _ = w.Write([]byte(sb.String()))
		_ = w.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	got := sseData(out)
	if len(got) != n {
		t.Fatalf("got %d lines, want %d", len(got), n)
	}
	for i, l := range got {
		if l != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d = %q", i, l)
		}
	}
}

// A follow stream opens with an SSE comment heartbeat (so the FE can arm its
// stall watchdog) and gets one per interval while idle, so an idle-timeout
// proxy doesn't cut it; the consumer sees only the real lines.
func TestK8sLogCopy_HeartbeatWhileIdle(t *testing.T) {
	out, err := runCopy(t, 10*time.Millisecond, func(w *io.PipeWriter) {
		_, _ = w.Write([]byte("first\n"))
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte("second\n"))
		_ = w.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, ": keepalive\n\n") {
		t.Fatalf("follow stream must open with a heartbeat: %q", out)
	}
	if strings.Count(out, ": keepalive\n\n") < 3 {
		t.Fatalf("no idle heartbeats in %q", out)
	}
	if got := sseData(out); fmt.Sprint(got) != "[first second]" {
		t.Fatalf("data = %q", got)
	}
}

// A read error (not EOF) is returned after the lines read before it.
func TestK8sLogCopy_ReadErrorSurfaces(t *testing.T) {
	boom := errors.New("upstream reset")
	out, err := runCopy(t, 0, func(w *io.PipeWriter) {
		_, _ = w.Write([]byte("ok\n"))
		_ = w.CloseWithError(boom)
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := sseData(out); fmt.Sprint(got) != "[ok]" {
		t.Fatalf("data = %q", got)
	}
}

// Cancelling ctx (client went away) returns promptly even while the upstream
// follow read is blocked, and closes the upstream.
func TestK8sLogCopy_CancelUnblocksFollow(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	var buf lockedBuf
	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		err = copyLogLines(ctx, pr, &buf, nil, time.Hour)
	}()
	_, _ = pw.Write([]byte("one\n"))
	cancel()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("copyLogLines did not return after cancel")
	}
	if err != nil {
		t.Fatalf("err = %v, want nil on cancel", err)
	}
	if _, werr := pw.Write([]byte("two\n")); werr == nil {
		t.Fatal("upstream still open after cancel")
	}
}
