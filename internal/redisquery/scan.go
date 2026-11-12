package redisquery

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// BuildMatch turns what a user typed into the key browser's search box into
// a SCAN MATCH glob:
//
//   - empty / whitespace → "*" (everything)
//   - an explicit glob   → used as-is: "order*", "*:123", "user:?", "id:[0-9]"
//   - plain text         → a contains match with glob specials escaped:
//     "order" → "*order*", `a[1` → `*a\[1*`
//
// A glob is explicit when it has an unescaped * or ?, a [...] class with a
// closing ], or a backslash escaping a glob special. Only the FE search box
// opts into this (smart=1); the raw `match` param stays a literal glob for
// the MCP tool and older clients.
func BuildMatch(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return "*"
	}
	if isExplicitGlob(s) {
		return s
	}
	return "*" + escapeGlob(s) + "*"
}

const globSpecials = `*?[]\`

func isExplicitGlob(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '?':
			return true
		case '[':
			if strings.IndexByte(s[i+1:], ']') >= 0 {
				return true
			}
		case '\\':
			if i+1 < len(s) && strings.IndexByte(globSpecials, s[i+1]) >= 0 {
				return true
			}
		}
	}
	return false
}

func escapeGlob(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(globSpecials, s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Scan-loop budget. One SCAN call returns whatever matched inside the
// COUNT-sized slice of the hash table it visited — on a big keyspace a
// selective MATCH ("order*") routinely yields an EMPTY page with a non-zero
// cursor. So a browse request keeps calling SCAN until it has `limit`
// matches, the cursor wraps to 0, or the time / iteration budget runs out.
const (
	defaultScanLimit  = 100
	maxScanLimit      = 2000
	minScanHint       = 10
	maxScanHint       = 1000
	scanTimeBudget    = 1500 * time.Millisecond
	maxScanIterations = 1000
)

// Stop reasons reported on a KeyListing that ended before the cursor wrapped.
const (
	StopLimit  = "limit"  // collected `limit` matches; more may follow the cursor
	StopBudget = "budget" // time / iteration budget ran out first
)

// scanPageFunc runs one SCAN step: (cursor, match, count) → (keys, next).
type scanPageFunc func(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error)

type scanBudget struct {
	Limit    int
	MaxIter  int
	Duration time.Duration
	now      func() time.Time // test seam; nil = time.Now
}

type scanOutcome struct {
	Keys       []string
	Cursor     uint64
	Complete   bool
	StopReason string
	Iterations int
}

// clampScanLimit applies the default + ceiling to a requested match limit.
func clampScanLimit(limit int) int {
	if limit <= 0 {
		return defaultScanLimit
	}
	if limit > maxScanLimit {
		return maxScanLimit
	}
	return limit
}

// runScanLoop drives page from cursor until one of: the cursor wraps to 0
// (Complete), at least b.Limit distinct keys were collected (StopLimit), or
// the iteration / time budget ran out (StopBudget). The last page is never
// trimmed — the returned cursor already points past it, so dropping keys
// would lose them for "Load more" — so Limit is a soft target.
//
// The COUNT hint starts at the limit and is then re-sized from the last
// page's match density to roughly what is still missing: a dense pattern
// ("*") asks for little more than the remainder (small overshoot), a sparse
// one grows to maxScanHint and covers the keyspace quickly.
func runScanLoop(ctx context.Context, page scanPageFunc, match string, cursor uint64, b scanBudget) (scanOutcome, error) {
	now := b.now
	if now == nil {
		now = time.Now
	}
	start := now()
	limit := clampScanLimit(b.Limit)
	maxIter := b.MaxIter
	if maxIter <= 0 {
		maxIter = maxScanIterations
	}
	hint := int64(limit)
	if hint < minScanHint {
		hint = minScanHint
	}
	if hint > maxScanHint {
		hint = maxScanHint
	}

	out := scanOutcome{Cursor: cursor, Keys: []string{}}
	seen := make(map[string]struct{})
	for {
		keys, next, err := page(ctx, out.Cursor, match, hint)
		if err != nil {
			// Ran out of time mid-call after real progress: keep what was
			// collected; the cursor still points at the last completed page.
			if out.Iterations > 0 && ctx.Err() != nil {
				out.StopReason = StopBudget
				return out, nil
			}
			return out, err
		}
		out.Iterations++
		hits := 0
		for _, k := range keys {
			// SCAN may return a key more than once (rehashing).
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out.Keys = append(out.Keys, k)
			hits++
		}
		out.Cursor = next
		if next == 0 {
			out.Complete = true
			return out, nil
		}
		if len(out.Keys) >= limit {
			out.StopReason = StopLimit
			return out, nil
		}
		if out.Iterations >= maxIter || now().Sub(start) >= b.Duration || ctx.Err() != nil {
			out.StopReason = StopBudget
			return out, nil
		}
		hint = nextScanHint(hint, hits, limit-len(out.Keys))
	}
}

// nextScanHint sizes the next SCAN COUNT: no hits → double; otherwise the
// remaining matches divided by the page's hit rate. Clamped to
// [minScanHint, maxScanHint].
func nextScanHint(hint int64, hits, remaining int) int64 {
	var next int64
	if hits == 0 {
		next = hint * 2
	} else {
		next = int64(math.Ceil(float64(remaining) * float64(hint) / float64(hits)))
	}
	if next < minScanHint {
		next = minScanHint
	}
	if next > maxScanHint {
		next = maxScanHint
	}
	return next
}

// Cluster SCAN has one cursor per master. To keep the wire cursor a single
// uint64 (JSON number, round-tripped by the FE / MCP "Load more"), the
// master's index in the address-sorted master list lives in the low
// clusterShardBits bits and that master's own cursor in the rest:
// composite = shardCursor<<clusterShardBits | shardIndex. Cursor 0 starts at
// master 0; finishing master i continues at (i+1, 0); finishing the last
// master returns 0 (complete).
const (
	clusterShardBits = 10
	clusterShardMask = 1<<clusterShardBits - 1
)

// clusterPager chains per-master pagers behind the composite cursor.
func clusterPager(shards []scanPageFunc) scanPageFunc {
	return func(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
		idx := int(cursor & clusterShardMask)
		shardCur := cursor >> clusterShardBits
		if idx >= len(shards) {
			return nil, 0, fmt.Errorf("cluster topology changed since this cursor was issued; scan again")
		}
		keys, next, err := shards[idx](ctx, shardCur, match, count)
		if err != nil {
			return nil, 0, err
		}
		if next == 0 {
			if idx+1 >= len(shards) {
				return keys, 0, nil
			}
			return keys, uint64(idx + 1), nil
		}
		if next > math.MaxUint64>>clusterShardBits {
			return nil, 0, fmt.Errorf("cluster scan cursor out of range")
		}
		return keys, next<<clusterShardBits | uint64(idx), nil
	}
}

// clusterShardPagers lists the cluster's masters in a stable (address) order
// and returns one SCAN pager per master. ForEachMaster runs its callback
// concurrently, hence the mutex.
func clusterShardPagers(ctx context.Context, cc *redis.ClusterClient) ([]scanPageFunc, error) {
	var mu sync.Mutex
	var nodes []*redis.Client
	err := cc.ForEachMaster(ctx, func(_ context.Context, n *redis.Client) error {
		mu.Lock()
		nodes = append(nodes, n)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no cluster masters available")
	}
	if len(nodes) > clusterShardMask+1 {
		return nil, fmt.Errorf("too many cluster masters (%d) to browse", len(nodes))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Options().Addr < nodes[j].Options().Addr })
	pagers := make([]scanPageFunc, len(nodes))
	for i, n := range nodes {
		n := n
		pagers[i] = func(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
			return n.Scan(ctx, cursor, match, count).Result()
		}
	}
	return pagers, nil
}
