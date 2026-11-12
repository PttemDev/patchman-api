package k8sclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// Live pod-log tests against a real cluster (a throwaway k3s in Docker):
//
//	docker run --privileged --name patchman-test-k3s -p 16443:6443 -d rancher/k3s:v1.30.6-k3s1 server
//	# kubeconfig: docker exec patchman-test-k3s cat /etc/rancher/k3s/k3s.yaml (server → https://127.0.0.1:16443)
//	# pod "logger" (container "app") printing `"seq":N` / `seq=N` every 100ms to stdout + stderr
//	PATCHMAN_TEST_K8S_KUBECONFIG=/path/kubeconfig.yaml go test ./internal/k8sclient -run K8sLogsLive -v
func liveK8s(t *testing.T) (*Registry, models.K8sConnection) {
	t.Helper()
	path := os.Getenv("PATCHMAN_TEST_K8S_KUBECONFIG")
	if path == "" {
		t.Skip("PATCHMAN_TEST_K8S_KUBECONFIG not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := cipher.Encrypt(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return NewRegistry(cipher, false), models.K8sConnection{ID: "live", KubeconfigEnc: enc}
}

// lockedBuf is an io.Writer safe to read while StreamLogs writes into it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var liveSeqRe = regexp.MustCompile(`seq["=:]+(\d+)`)

type liveLine struct {
	ts  time.Time
	raw string
	seq int
}

// liveLines pulls the `data:` log lines (timestamped) out of the SSE output.
func liveLines(t *testing.T, sse string) []liveLine {
	t.Helper()
	var out []liveLine
	for _, l := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(l, "data: ") {
			continue
		}
		body := strings.TrimPrefix(l, "data: ")
		tsStr, rest, ok := strings.Cut(body, " ")
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			continue
		}
		ll := liveLine{ts: ts, raw: rest, seq: -1}
		if m := liveSeqRe.FindStringSubmatch(rest); m != nil {
			ll.seq, _ = strconv.Atoi(m[1])
		}
		out = append(out, ll)
	}
	return out
}

func TestK8sLogsLive_FollowIsOrderedAndGapFree(t *testing.T) {
	reg, conn := liveK8s(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var buf lockedBuf
	err := reg.StreamLogs(ctx, conn, "", LogStreamOptions{
		Namespace: "default", Pod: "logger", Container: "app",
		Follow: true, TailLines: 200, Timestamps: true,
	}, &buf, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	lines := liveLines(t, buf.String())
	if len(lines) < 220 { // 200 tail + ~4s × 10 lines/s of follow
		t.Fatalf("got %d lines, want ≥220 (tail + follow)", len(lines))
	}
	inversions := 0
	for i := 1; i < len(lines); i++ {
		if lines[i].seq != lines[i-1].seq+1 {
			t.Errorf("line %d: seq %d after %d (gap or reorder)", i, lines[i].seq, lines[i-1].seq)
		}
		if lines[i].ts.Before(lines[i-1].ts) {
			inversions++
		}
	}
	t.Logf("%d lines, seq %d..%d, timestamp inversions=%d", len(lines), lines[0].seq, lines[len(lines)-1].seq, inversions)
	if inversions > 0 {
		t.Errorf("%d timestamp inversions in a single-container stream", inversions)
	}
}

// A resume from a line's own timestamp (truncated to the second by the API)
// re-delivers that line and everything after it: an overlap the FE dedupes on
// the timestamp, never a gap.
func TestK8sLogsLive_SinceTimeResumeOverlapsNoGap(t *testing.T) {
	reg, conn := liveK8s(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first lockedBuf
	if err := reg.StreamLogs(ctx, conn, "", LogStreamOptions{
		Namespace: "default", Pod: "logger", Container: "app", TailLines: 50, Timestamps: true,
	}, &first, nil); err != nil {
		t.Fatal(err)
	}
	a := liveLines(t, first.String())
	if len(a) < 20 {
		t.Fatalf("only %d lines", len(a))
	}
	cursor := a[len(a)-10] // resume from a line 10 back
	time.Sleep(500 * time.Millisecond)
	var second lockedBuf
	since := cursor.ts
	if err := reg.StreamLogs(ctx, conn, "", LogStreamOptions{
		Namespace: "default", Pod: "logger", Container: "app", TailLines: 5000, Timestamps: true, SinceTime: &since,
	}, &second, nil); err != nil {
		t.Fatal(err)
	}
	b := liveLines(t, second.String())
	if len(b) == 0 {
		t.Fatal("resume returned no lines")
	}
	if b[0].ts.After(cursor.ts) {
		t.Fatalf("resume starts after the cursor (gap): first=%v cursor=%v", b[0].ts, cursor.ts)
	}
	// Dedupe the way the FE does: drop lines up to and including the cursor.
	i := 0
	for i < len(b) && !b[i].ts.After(cursor.ts) {
		i++
	}
	if i >= len(b) {
		t.Fatal("nothing new after the cursor")
	}
	if b[i].seq != cursor.seq+1 {
		t.Fatalf("after dedupe the stream continues at seq %d, want %d", b[i].seq, cursor.seq+1)
	}
	t.Logf("resume from seq %d: %d overlap lines dropped, continues at seq %d (%d new lines)", cursor.seq, i, b[i].seq, len(b)-i)
}
