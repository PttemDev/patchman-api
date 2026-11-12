package redisquery

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"patchman-be/internal/models"

	"github.com/redis/go-redis/v9"
)

// Against a real, THROWAWAY Redis (PATCHMAN_REDIS_TEST_ADDR, e.g.
// `docker run -d -p 16379:6379 redis:7-alpine` → 127.0.0.1:16379). Uses db 9,
// skips unless it is empty, and FLUSHDBs it afterwards.
const liveTestDB = 9

func liveRegistry(t *testing.T) (*Registry, models.RedisConnection, *redis.Client) {
	t.Helper()
	addr := os.Getenv("PATCHMAN_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set PATCHMAN_REDIS_TEST_ADDR to a throwaway Redis to run")
	}
	raw := redis.NewClient(&redis.Options{Addr: addr, DB: liveTestDB})
	ctx := context.Background()
	if n, err := raw.DBSize(ctx).Result(); err != nil || n != 0 {
		_ = raw.Close()
		t.Skipf("db %d not empty (%d keys, err=%v) — refusing to touch it", liveTestDB, n, err)
	}
	t.Cleanup(func() { _ = raw.FlushDB(context.Background()).Err(); _ = raw.Close() })
	conn := models.RedisConnection{ID: "live-test", Address: addr, Mode: "standalone", DBIndex: liveTestDB}
	return NewRegistry(nil), conn, raw
}

func TestRedisLiveScanSparsePattern(t *testing.T) {
	reg, conn, raw := liveRegistry(t)
	ctx := context.Background()
	// 200k noise keys + 40 order:* keys + a few user:* keys.
	p := raw.Pipeline()
	for i := 1; i <= 200_000; i++ {
		p.Set(ctx, fmt.Sprintf("noise:%d", i), "x", 0)
		if i%10_000 == 0 {
			if _, err := p.Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 1; i <= 40; i++ {
		p.Set(ctx, fmt.Sprintf("order:%d", i), "o", 0)
	}
	for i := 1; i <= 5; i++ {
		p.Set(ctx, fmt.Sprintf("user:%d", i), "u", 0)
	}
	if _, err := p.Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// Root cause, reproduced: ONE SCAN page of `order*` (the old code path,
	// COUNT 100) is empty but the cursor is not 0.
	k, cur, err := raw.Scan(ctx, 0, "order*", 100).Result()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("single SCAN 0 MATCH order* COUNT 100 → %d keys, cursor %d", len(k), cur)

	collect := func(match string) ([]string, int) {
		var all []string
		cursor, calls := uint64(0), 0
		for {
			l, err := reg.ScanKeys(ctx, conn, liveTestDB, match, cursor, 500)
			if err != nil {
				t.Fatal(err)
			}
			calls++
			for _, e := range l.Keys {
				all = append(all, e.Key)
			}
			t.Logf("ScanKeys(%q) page %d: %d keys, %d SCAN calls, %d ms, complete=%v stop=%q", match, calls, len(l.Keys), l.Iterations, l.ScanMs, l.Complete, l.StopReason)
			if l.Complete {
				break
			}
			cursor = l.Cursor
			if calls > 50 {
				t.Fatal("scan never completed")
			}
		}
		sort.Strings(all)
		return all, calls
	}

	got, calls := collect(BuildMatch("order*"))
	if len(got) != 40 {
		t.Fatalf("order* found %d keys, want 40", len(got))
	}
	if calls != 1 {
		t.Logf("note: order* needed %d requests (budget-limited host)", calls)
	}
	if got, _ := collect(BuildMatch("order")); len(got) != 40 { // → *order*
		t.Fatalf("plain `order` found %d keys, want 40", len(got))
	}
	got, _ = collect(BuildMatch("*:15"))
	want := []string{"noise:15", "order:15"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("*:15 = %v, want %v", got, want)
	}
}

func TestRedisLiveRemoveElement(t *testing.T) {
	reg, conn, raw := liveRegistry(t)
	ctx := context.Background()
	s := func(v string) *string { return &v }
	i := func(v int64) *int64 { return &v }
	ref := func(key, typ string) ElementRef { return ElementRef{DB: liveTestDB, Key: key, Type: typ} }

	raw.HSet(ctx, "h", "f1", "v1", "f2", "v2")
	raw.RPush(ctx, "l", "a", "b", "a", "c")
	raw.SAdd(ctx, "s", "m1", "m2")
	raw.ZAdd(ctx, "z", redis.Z{Score: 1, Member: "z1"}, redis.Z{Score: 2, Member: "z2"})
	id1, _ := raw.XAdd(ctx, &redis.XAddArgs{Stream: "x", Values: map[string]any{"k": "1"}}).Result()
	raw.XAdd(ctx, &redis.XAddArgs{Stream: "x", Values: map[string]any{"k": "2"}})
	raw.Set(ctx, "str", "v", 0)

	must := func(r ElementRef, wantExists bool) {
		t.Helper()
		res, err := reg.RemoveElement(ctx, conn, r)
		if err != nil {
			t.Fatalf("%s %+v: %v", r.Type, r, err)
		}
		if res.Removed != 1 || res.KeyExists != wantExists {
			t.Fatalf("%s: %+v, want removed=1 keyExists=%v", r.Type, res, wantExists)
		}
	}
	gone := func(r ElementRef) {
		t.Helper()
		if _, err := reg.RemoveElement(ctx, conn, r); !IsElementGone(err) {
			t.Fatalf("%s %+v: err=%v, want ErrElementGone", r.Type, r, err)
		}
	}

	r := ref("h", "hash")
	r.Field = s("f1")
	must(r, true)
	gone(r) // already removed → 409, not a fake success

	// List: removes exactly index 2 (the SECOND "a"), not the first equal value.
	r = ref("l", "list")
	r.Index, r.Value = i(2), s("a")
	must(r, true)
	if got := raw.LRange(ctx, "l", 0, -1).Val(); fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("list after removing index 2 = %v, want [a b c]", got)
	}
	// Stale view: index 1 no longer holds "a" → nothing changes.
	r.Index, r.Value = i(1), s("a")
	gone(r)
	if got := raw.LRange(ctx, "l", 0, -1).Val(); fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("stale list removal changed the list: %v", got)
	}
	r.Index, r.Value = i(99), s("a")
	gone(r) // out of range

	r = ref("s", "set")
	r.Member = s("m1")
	must(r, true)
	r = ref("z", "zset")
	r.Member = s("z2")
	must(r, true)
	r = ref("x", "stream")
	r.EntryID = id1
	must(r, true)

	// Type changed under the view → refused.
	r = ref("str", "hash")
	r.Field = s("x")
	gone(r)
	if raw.Get(ctx, "str").Val() != "v" {
		t.Fatal("string key touched by a hash removal")
	}

	// Removing the last member deletes the key — reported as keyExists=false.
	r = ref("s", "set")
	r.Member = s("m2")
	must(r, false)
	if raw.Exists(ctx, "s").Val() != 0 {
		t.Fatal("empty set should be gone")
	}
	// No tombstone left behind anywhere.
	for _, k := range raw.LRange(ctx, "l", 0, -1).Val() {
		if len(k) > 20 && k[:20] == "__patchman_removed__" {
			t.Fatal("tombstone left in list")
		}
	}
}
