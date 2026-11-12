package redisquery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRedisBuildMatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "*"},
		{"   ", "*"},
		{"order", "*order*"},
		{"  order  ", "*order*"},
		{"order*", "order*"},
		{"*:123", "*:123"},
		{"user:?", "user:?"},
		{"id:[0-9]*", "id:[0-9]*"},
		{"id:[0-9]", "id:[0-9]"},
		{`a\*b`, `a\*b`},             // deliberate escape → glob as typed
		{"a[1", `*a\[1*`},            // unclosed [ is literal
		{"a]1", `*a\]1*`},            // stray ] escaped (harmless)
		{`C:\path`, `*C:\\path*`},    // backslash not escaping a special → literal
		{"order:1 2", "*order:1 2*"}, // spaces stay
	}
	for _, c := range cases {
		if got := BuildMatch(c.in); got != c.want {
			t.Errorf("BuildMatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeKeyspace simulates SCAN over a fixed key list: cursor i means "resume
// at position i"; each call visits `count` slots and returns the ones that
// match (prefix match is enough for the loop tests).
type fakeKeyspace struct {
	keys  []string
	calls int
	hints []int64
	fail  error
}

func (f *fakeKeyspace) page(_ context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
	f.calls++
	f.hints = append(f.hints, count)
	if f.fail != nil {
		return nil, 0, f.fail
	}
	prefix := match
	if n := len(prefix); n > 0 && prefix[n-1] == '*' {
		prefix = prefix[:n-1]
	}
	start := int(cursor)
	end := start + int(count)
	if end > len(f.keys) {
		end = len(f.keys)
	}
	var out []string
	for _, k := range f.keys[start:end] {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	if end >= len(f.keys) {
		return out, 0, nil
	}
	return out, uint64(end), nil
}

// sparseKeyspace: n noise keys with `hits` order:* keys at the end.
func sparseKeyspace(noise, hits int) *fakeKeyspace { return shardKeyspace("", noise, hits) }

// shardKeyspace tags key names so several fake masters never share a key.
func shardKeyspace(tag string, noise, hits int) *fakeKeyspace {
	f := &fakeKeyspace{}
	for i := 0; i < noise; i++ {
		f.keys = append(f.keys, fmt.Sprintf("noise:%s%d", tag, i))
	}
	for i := 0; i < hits; i++ {
		f.keys = append(f.keys, fmt.Sprintf("order:%s%d", tag, i))
	}
	return f
}

// The bug: one SCAN page of a selective pattern is empty with a non-zero
// cursor. The loop must keep going until it finds the matches.
func TestRedisScanLoopKeepsScanningPastEmptyPages(t *testing.T) {
	f := sparseKeyspace(50_000, 30)
	out, err := runScanLoop(context.Background(), f.page, "order*", 0, scanBudget{Limit: 500, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 30 || !out.Complete || out.Cursor != 0 || out.StopReason != "" {
		t.Fatalf("got %d keys complete=%v cursor=%d stop=%q, want 30 complete", len(out.Keys), out.Complete, out.Cursor, out.StopReason)
	}
	if f.calls < 2 {
		t.Fatalf("expected several SCAN calls, got %d", f.calls)
	}
	// The hint grows on sparse pages but never past the ceiling.
	for _, h := range f.hints {
		if h > maxScanHint || h < minScanHint {
			t.Fatalf("hint %d outside [%d,%d]", h, minScanHint, maxScanHint)
		}
	}
	if f.hints[len(f.hints)-1] != maxScanHint {
		t.Errorf("hint never grew on sparse pages: %v", f.hints)
	}
}

func TestRedisScanLoopStopsAtLimitWithoutTrimming(t *testing.T) {
	f := sparseKeyspace(0, 5000) // every key matches
	out, err := runScanLoop(context.Background(), f.page, "order*", 0, scanBudget{Limit: 150, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != StopLimit || out.Complete {
		t.Fatalf("stop=%q complete=%v, want limit", out.StopReason, out.Complete)
	}
	if len(out.Keys) < 150 {
		t.Fatalf("got %d keys, want >= 150", len(out.Keys))
	}
	// Never trimmed: the cursor points right after the last returned key.
	if int(out.Cursor) != len(out.Keys) {
		t.Fatalf("cursor %d != keys returned %d — keys would be lost on Load more", out.Cursor, len(out.Keys))
	}
	// Load more continues exactly where it stopped, with no gaps / repeats.
	more, err := runScanLoop(context.Background(), f.page, "order*", out.Cursor, scanBudget{Limit: 150, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if more.Keys[0] != f.keys[len(out.Keys)] {
		t.Fatalf("continuation starts at %q, want %q", more.Keys[0], f.keys[len(out.Keys)])
	}
}

func TestRedisScanLoopTimeBudget(t *testing.T) {
	f := sparseKeyspace(1_000_000, 1)
	clock := time.Unix(0, 0)
	b := scanBudget{Limit: 500, Duration: 1500 * time.Millisecond, now: func() time.Time {
		clock = clock.Add(100 * time.Millisecond) // every check costs 100ms
		return clock
	}}
	out, err := runScanLoop(context.Background(), f.page, "order*", 0, b)
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != StopBudget || out.Complete || out.Cursor == 0 {
		t.Fatalf("stop=%q complete=%v cursor=%d, want budget stop with a cursor", out.StopReason, out.Complete, out.Cursor)
	}
	if out.Iterations > 16 {
		t.Fatalf("ran %d iterations past a 1.5s budget", out.Iterations)
	}
}

func TestRedisScanLoopIterationBudget(t *testing.T) {
	f := sparseKeyspace(1_000_000, 1)
	out, err := runScanLoop(context.Background(), f.page, "order*", 0, scanBudget{Limit: 10, MaxIter: 7, Duration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != StopBudget || out.Iterations != 7 || f.calls != 7 {
		t.Fatalf("stop=%q iterations=%d calls=%d, want budget after 7", out.StopReason, out.Iterations, f.calls)
	}
}

func TestRedisScanLoopDedupesAndErrors(t *testing.T) {
	// A page that repeats a key (SCAN during rehash) is deduped.
	calls := 0
	dup := func(_ context.Context, cursor uint64, _ string, _ int64) ([]string, uint64, error) {
		calls++
		if calls == 1 {
			return []string{"a", "b"}, 7, nil
		}
		return []string{"b", "c"}, 0, nil
	}
	out, err := runScanLoop(context.Background(), dup, "*", 0, scanBudget{Limit: 100, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 3 {
		t.Fatalf("keys %v, want a b c once each", out.Keys)
	}
	// An error on the first call surfaces.
	f := &fakeKeyspace{fail: errors.New("NOPERM")}
	if _, err := runScanLoop(context.Background(), f.page, "*", 0, scanBudget{Limit: 10, Duration: time.Minute}); err == nil {
		t.Fatal("want error")
	}
	// A context deadline after progress keeps the partial result.
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	slow := func(ctx context.Context, cursor uint64, _ string, _ int64) ([]string, uint64, error) {
		n++
		if n == 2 {
			cancel()
			return nil, 0, ctx.Err()
		}
		return []string{"x"}, 5, nil
	}
	out, err = runScanLoop(ctx, slow, "*", 0, scanBudget{Limit: 100, Duration: time.Minute})
	if err != nil || out.StopReason != StopBudget || len(out.Keys) != 1 || out.Cursor != 5 {
		t.Fatalf("partial: err=%v stop=%q keys=%v cursor=%d", err, out.StopReason, out.Keys, out.Cursor)
	}
}

func TestRedisClampScanLimit(t *testing.T) {
	for in, want := range map[int]int{0: defaultScanLimit, -3: defaultScanLimit, 50: 50, 1_000_000: maxScanLimit} {
		if got := clampScanLimit(in); got != want {
			t.Errorf("clampScanLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// The cluster pager walks master 0, then 1, … behind one composite cursor and
// only reports 0 (complete) after the last master wraps.
func TestRedisClusterPager(t *testing.T) {
	a := shardKeyspace("a", 2500, 3)
	b := shardKeyspace("b", 10, 0)
	c := shardKeyspace("c", 1200, 2)
	page := clusterPager([]scanPageFunc{a.page, b.page, c.page})
	out, err := runScanLoop(context.Background(), page, "order*", 0, scanBudget{Limit: 500, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Complete || len(out.Keys) != 5 {
		t.Fatalf("complete=%v keys=%v, want all 5 order keys across masters", out.Complete, out.Keys)
	}

	// Resumable mid-shard: stop on the iteration budget, then continue.
	a2, b2, c2 := shardKeyspace("a", 2500, 3), shardKeyspace("b", 10, 0), shardKeyspace("c", 1200, 2)
	page = clusterPager([]scanPageFunc{a2.page, b2.page, c2.page})
	total := 0
	cursor := uint64(0)
	for i := 0; i < 50; i++ {
		o, err := runScanLoop(context.Background(), page, "order*", cursor, scanBudget{Limit: 500, MaxIter: 1, Duration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		total += len(o.Keys)
		cursor = o.Cursor
		if o.Complete {
			break
		}
	}
	if total != 5 || cursor != 0 {
		t.Fatalf("resumed scan found %d keys (cursor %d), want 5 and complete", total, cursor)
	}

	// A cursor naming a master that no longer exists is an error, not a silent restart.
	if _, _, err := clusterPager([]scanPageFunc{a.page})(context.Background(), 5, "*", 10); err == nil {
		t.Fatal("want topology error for out-of-range shard index")
	}
}

func TestRedisElementRefValidate(t *testing.T) {
	s := func(v string) *string { return &v }
	i := func(v int64) *int64 { return &v }
	ok := []ElementRef{
		{Key: "h", Type: "hash", Field: s("")}, // empty field name is valid in Redis
		{Key: "s", Type: "set", Member: s("m")},
		{Key: "z", Type: "zset", Member: s("m")},
		{Key: "l", Type: "list", Index: i(0), Value: s("v")},
		{Key: "x", Type: "stream", EntryID: "1-0"},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: unexpected %v", r, err)
		}
	}
	bad := []ElementRef{
		{Type: "hash", Field: s("f")},               // no key
		{Key: "h", Type: "hash"},                    // no field
		{Key: "s", Type: "set"},                     // no member
		{Key: "l", Type: "list", Index: i(0)},       // no expected value
		{Key: "l", Type: "list", Value: s("v")},     // no index
		{Key: "x", Type: "stream"},                  // no id
		{Key: "k", Type: "string", Value: s("v")},   // strings are deleted whole
		{Key: "k", Type: "unknown", Member: s("m")}, // unknown type
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v: want validation error", r)
		}
	}
}

func TestRedisNextScanHint(t *testing.T) {
	cases := []struct {
		hint            int64
		hits, remaining int
		want            int64
	}{
		{100, 0, 500, 200},          // nothing matched → double
		{800, 0, 500, maxScanHint},  // capped
		{500, 499, 1, minScanHint},  // dense, 1 missing → floor
		{500, 250, 100, 200},        // 50% density, 100 missing
		{1000, 1, 400, maxScanHint}, // sparse → ceiling
	}
	for _, c := range cases {
		if got := nextScanHint(c.hint, c.hits, c.remaining); got != c.want {
			t.Errorf("nextScanHint(%d,%d,%d) = %d, want %d", c.hint, c.hits, c.remaining, got, c.want)
		}
	}
}

// A dense page that falls just short of the limit must not trigger a
// second full-size page (the "Load more doubled the page" overshoot).
func TestRedisScanLoopSmallOvershoot(t *testing.T) {
	f := &fakeKeyspace{keys: []string{"noise:0"}}
	for i := 0; i < 5000; i++ {
		f.keys = append(f.keys, fmt.Sprintf("order:%d", i))
	}
	out, err := runScanLoop(context.Background(), f.page, "order*", 0, scanBudget{Limit: 500, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != StopLimit || len(out.Keys) < 500 || len(out.Keys) > 500+minScanHint {
		t.Fatalf("got %d keys (stop %q), want 500..%d", len(out.Keys), out.StopReason, 500+minScanHint)
	}
}
