package redisquery

import (
	"context"
	"fmt"
	"time"

	"patchman-be/internal/models"

	"github.com/redis/go-redis/v9"
)

// capElements bounds how many elements a single collection value read
// returns per page (hash fields, list items, set members, zset members,
// stream entries). Browse tool, not a bulk exporter.
const capElements = 200

// ScanKeys walks the keyspace with SCAN (never KEYS) and enriches the
// returned keys with TYPE + PTTL + MEMORY USAGE via a single pipeline.
//
// limit is the number of MATCHING keys wanted (the browser's COUNT box /
// the MCP tool's count), not the SCAN COUNT hint: SCAN is called in a loop
// (runScanLoop) until that many keys matched, the cursor wrapped, or the
// time/iteration budget ran out — a single SCAN page of a selective pattern
// is usually empty on a large keyspace. The returned cursor continues the
// scan ("Load more"). Cluster mode scans each master in turn behind a
// composite cursor (clusterPager).
func (r *Registry) ScanKeys(ctx context.Context, conn models.RedisConnection, db int, match string, cursor uint64, limit int) (*KeyListing, error) {
	cl, err := r.client(conn, db)
	if err != nil {
		return nil, err
	}
	if match == "" {
		match = "*"
	}
	limit = clampScanLimit(limit)

	start := time.Now()
	var page scanPageFunc
	if cc, ok := cl.(*redis.ClusterClient); ok {
		shards, serr := clusterShardPagers(ctx, cc)
		if serr != nil {
			return nil, serr
		}
		page = clusterPager(shards)
	} else {
		page = func(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
			return cl.Scan(ctx, cursor, match, count).Result()
		}
	}
	out, err := runScanLoop(ctx, page, match, cursor, scanBudget{Limit: limit, Duration: scanTimeBudget})
	if err != nil {
		return nil, err
	}
	keys := out.Keys

	entries := make([]KeyEntry, 0, len(keys))
	if len(keys) > 0 {
		typeCmds := make([]*redis.StatusCmd, len(keys))
		pttlCmds := make([]*redis.Cmd, len(keys))
		memCmds := make([]*redis.Cmd, len(keys))
		pipe := cl.Pipeline()
		for i, k := range keys {
			typeCmds[i] = pipe.Type(ctx, k)
			pttlCmds[i] = pipe.Do(ctx, "PTTL", k)
			memCmds[i] = pipe.Do(ctx, "MEMORY", "USAGE", k)
		}
		// A per-key error (e.g. a key that raced away) must not abort the
		// whole page; individual cmd reads below fall back to sentinels.
		_, _ = pipe.Exec(ctx)
		for i, k := range keys {
			typ := typeCmds[i].Val()
			ttlMs, terr := pttlCmds[i].Int64()
			if terr != nil {
				ttlMs = -2
			}
			sizeBytes, merr := memCmds[i].Int64()
			if merr != nil {
				sizeBytes = -1
			}
			entries = append(entries, KeyEntry{Key: k, Type: typ, TtlMs: ttlMs, SizeBytes: sizeBytes})
		}
	}

	return &KeyListing{
		Keys:       entries,
		Cursor:     out.Cursor,
		Complete:   out.Complete,
		DB:         effectiveDB(conn, db),
		ScanMs:     int(time.Since(start).Milliseconds()),
		Pattern:    match,
		Limit:      limit,
		StopReason: out.StopReason,
		Iterations: out.Iterations,
	}, nil
}

// KeyMeta returns the full metadata block for one key.
func (r *Registry) KeyMeta(ctx context.Context, conn models.RedisConnection, db int, key string) (*KeyMeta, error) {
	cl, err := r.client(conn, db)
	if err != nil {
		return nil, err
	}
	typ, err := cl.Type(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if typ == "none" {
		return nil, fmt.Errorf("key not found")
	}
	pttl, _ := cl.Do(ctx, "PTTL", key).Int64()
	ttl := pttl
	if pttl > 0 {
		ttl = pttl / 1000
	}
	mem, merr := cl.Do(ctx, "MEMORY", "USAGE", key).Int64()
	if merr != nil {
		mem = -1
	}
	enc, _ := cl.Do(ctx, "OBJECT", "ENCODING", key).Text()
	length := keyLength(ctx, cl, typ, key)
	return &KeyMeta{
		Key: key, Type: typ, Encoding: enc,
		TtlMs: ttl, PttlMs: pttl, MemoryBytes: mem, Length: length,
	}, nil
}

// keyLength returns the type-appropriate cardinality (-1 when unknown).
func keyLength(ctx context.Context, cl redis.UniversalClient, typ, key string) int64 {
	switch typ {
	case "string":
		if n, err := cl.StrLen(ctx, key).Result(); err == nil {
			return n
		}
	case "hash":
		if n, err := cl.HLen(ctx, key).Result(); err == nil {
			return n
		}
	case "list":
		if n, err := cl.LLen(ctx, key).Result(); err == nil {
			return n
		}
	case "set":
		if n, err := cl.SCard(ctx, key).Result(); err == nil {
			return n
		}
	case "zset":
		if n, err := cl.ZCard(ctx, key).Result(); err == nil {
			return n
		}
	case "stream":
		if n, err := cl.XLen(ctx, key).Result(); err == nil {
			return n
		}
	}
	return -1
}

// GetValue reads a value type-aware. start/stop bound the index-addressable
// types (list, zset); cursor paginates the scan-addressable types (hash,
// set). Streams return the first capElements entries.
func (r *Registry) GetValue(ctx context.Context, conn models.RedisConnection, db int, key string, start, stop int64, cursor uint64) (*ValueResult, error) {
	cl, err := r.client(conn, db)
	if err != nil {
		return nil, err
	}
	typ, err := cl.Type(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if typ == "none" {
		return nil, fmt.Errorf("key not found")
	}
	pttl, _ := cl.Do(ctx, "PTTL", key).Int64()
	mem, merr := cl.Do(ctx, "MEMORY", "USAGE", key).Int64()
	if merr != nil {
		mem = -1
	}
	enc, _ := cl.Do(ctx, "OBJECT", "ENCODING", key).Text()

	res := &ValueResult{Key: key, Type: typ, ValueEncoding: "utf8", TtlMs: pttl, MemoryBytes: mem}
	_ = enc

	switch typ {
	case "string":
		b, gerr := cl.Get(ctx, key).Bytes()
		if gerr != nil {
			return nil, gerr
		}
		s, encFlag := encodeBytes(b)
		res.String = &s
		res.ValueEncoding = encFlag

	case "hash":
		fields, nextCur, herr := cl.HScan(ctx, key, cursor, "*", capElements).Result()
		if herr != nil {
			return nil, herr
		}
		m := map[string]string{}
		for i := 0; i+1 < len(fields); i += 2 {
			m[fields[i]] = fields[i+1]
		}
		res.Hash = m
		res.Cursor = nextCur
		res.Truncated = nextCur != 0

	case "list":
		if stop <= 0 {
			stop = start + capElements - 1
		}
		if stop-start+1 > capElements {
			stop = start + capElements - 1
		}
		items, lerr := cl.LRange(ctx, key, start, stop).Result()
		if lerr != nil {
			return nil, lerr
		}
		res.List = items
		total, _ := cl.LLen(ctx, key).Result()
		res.Truncated = stop+1 < total

	case "set":
		members, nextCur, serr := cl.SScan(ctx, key, cursor, "*", capElements).Result()
		if serr != nil {
			return nil, serr
		}
		res.Set = members
		res.Cursor = nextCur
		res.Truncated = nextCur != 0

	case "zset":
		if stop <= 0 {
			stop = start + capElements - 1
		}
		if stop-start+1 > capElements {
			stop = start + capElements - 1
		}
		zs, zerr := cl.ZRangeWithScores(ctx, key, start, stop).Result()
		if zerr != nil {
			return nil, zerr
		}
		out := make([]ZMember, 0, len(zs))
		for _, z := range zs {
			member, _ := z.Member.(string)
			out = append(out, ZMember{Member: member, Score: z.Score})
		}
		res.ZSet = out
		total, _ := cl.ZCard(ctx, key).Result()
		res.Truncated = stop+1 < total

	case "stream":
		entries, xerr := cl.XRangeN(ctx, key, "-", "+", capElements+1).Result()
		if xerr != nil {
			return nil, xerr
		}
		truncated := false
		if len(entries) > capElements {
			entries = entries[:capElements]
			truncated = true
		}
		out := make([]StreamEntry, 0, len(entries))
		for _, e := range entries {
			fields := map[string]string{}
			for k, v := range e.Values {
				fields[k] = fmt.Sprintf("%v", v)
			}
			out = append(out, StreamEntry{ID: e.ID, Fields: fields})
		}
		res.Stream = out
		res.Truncated = truncated

	default:
		return nil, fmt.Errorf("unsupported type: %s", typ)
	}
	return res, nil
}

// DeleteKey removes a key (UNLINK for non-blocking reclaim). Write-gated by
// the handler.
func (r *Registry) DeleteKey(ctx context.Context, conn models.RedisConnection, db int, key string) error {
	cl, err := r.client(conn, db)
	if err != nil {
		return err
	}
	return cl.Unlink(ctx, key).Err()
}

// SetTTL sets a key's expiry in seconds. ttlSec <= 0 PERSISTs (removes the
// expiry). Write-gated by the handler.
func (r *Registry) SetTTL(ctx context.Context, conn models.RedisConnection, db int, key string, ttlSec int64) error {
	cl, err := r.client(conn, db)
	if err != nil {
		return err
	}
	if ttlSec <= 0 {
		return cl.Persist(ctx, key).Err()
	}
	return cl.Expire(ctx, key, time.Duration(ttlSec)*time.Second).Err()
}
