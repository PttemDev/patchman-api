package redisquery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"patchman-be/internal/models"

	"github.com/redis/go-redis/v9"
)

// ElementRef addresses one element inside a collection key for removal.
// Type must equal the key's live TYPE; exactly the field for that type is
// required (pointers so an empty hash field / member stays addressable).
type ElementRef struct {
	DB      int     `json:"db"`
	Key     string  `json:"key"`
	Type    string  `json:"type"`              // hash | list | set | zset | stream
	Field   *string `json:"field,omitempty"`   // hash  → HDEL
	Member  *string `json:"member,omitempty"`  // set → SREM, zset → ZREM
	Index   *int64  `json:"index,omitempty"`   // list: position shown to the user
	Value   *string `json:"value,omitempty"`   // list: value shown at Index (optimistic check)
	EntryID string  `json:"entryId,omitempty"` // stream → XDEL
}

// RemoveResult reports what an element removal did. KeyExists is false when
// the removed element was the last one (Redis deletes empty collections).
type RemoveResult struct {
	Removed   int64 `json:"removed"`
	KeyExists bool  `json:"keyExists"`
}

// ErrElementGone means nothing was removed: the element (or key) no longer
// exists, the key changed type, or the list moved under the user's view.
// The handler maps it to 409 so the FE refreshes instead of claiming success.
var ErrElementGone = errors.New("nothing deleted — the element changed or no longer exists. Refresh and try again")

// IsElementGone reports whether err is ErrElementGone.
func IsElementGone(err error) bool { return errors.Is(err, ErrElementGone) }

// Validate checks the ref carries the address field its type needs.
func (e ElementRef) Validate() error {
	if e.Key == "" {
		return fmt.Errorf("key is required")
	}
	switch e.Type {
	case "hash":
		if e.Field == nil {
			return fmt.Errorf("field is required for a hash")
		}
	case "set", "zset":
		if e.Member == nil {
			return fmt.Errorf("member is required for a %s", e.Type)
		}
	case "list":
		if e.Index == nil || e.Value == nil {
			return fmt.Errorf("index and value are required for a list")
		}
	case "stream":
		if e.EntryID == "" {
			return fmt.Errorf("entryId is required for a stream")
		}
	default:
		return fmt.Errorf("unsupported type %q (string keys are deleted whole)", e.Type)
	}
	return nil
}

// RemoveElement deletes one element of a collection key: HDEL / SREM / ZREM
// / XDEL, or — for a list — exactly the element at Index, and only if it
// still holds Value (see removeListIndex). Write-gated by the handler.
func (r *Registry) RemoveElement(ctx context.Context, conn models.RedisConnection, ref ElementRef) (*RemoveResult, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	cl, err := r.client(conn, ref.DB)
	if err != nil {
		return nil, err
	}
	typ, err := cl.Type(ctx, ref.Key).Result()
	if err != nil {
		return nil, err
	}
	if typ != ref.Type {
		// "none" (key gone) or replaced by another type since it was opened.
		return nil, ErrElementGone
	}
	var n int64
	switch typ {
	case "hash":
		n, err = cl.HDel(ctx, ref.Key, *ref.Field).Result()
	case "set":
		n, err = cl.SRem(ctx, ref.Key, *ref.Member).Result()
	case "zset":
		n, err = cl.ZRem(ctx, ref.Key, *ref.Member).Result()
	case "stream":
		n, err = cl.XDel(ctx, ref.Key, ref.EntryID).Result()
	case "list":
		n, err = removeListIndex(ctx, cl, ref.Key, *ref.Index, *ref.Value)
	}
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrElementGone
	}
	exists, err := cl.Exists(ctx, ref.Key).Result()
	if err != nil {
		// The removal itself succeeded; assume the key is still there.
		exists = 1
	}
	return &RemoveResult{Removed: n, KeyExists: exists > 0}, nil
}

// removeListIndex removes exactly the list element at index, and only if it
// still equals expected. Lists have no "delete by index" command and
// LREM key 1 value removes the FIRST equal value — the wrong one when the
// list holds duplicates — so this WATCHes the key, checks LINDEX, then in
// one MULTI/EXEC overwrites the slot with a unique tombstone (LSET) and
// removes that tombstone (LREM 1). The transaction is atomic (no client
// ever sees the tombstone) and aborts if anyone touched the list after the
// check. Any mismatch / abort → ErrElementGone (nothing changed).
func removeListIndex(ctx context.Context, cl redis.UniversalClient, key string, index int64, expected string) (int64, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, err
	}
	tombstone := "__patchman_removed__:" + hex.EncodeToString(buf[:])
	var removed int64
	txf := func(tx *redis.Tx) error {
		cur, err := tx.LIndex(ctx, key, index).Result()
		if errors.Is(err, redis.Nil) {
			return ErrElementGone
		}
		if err != nil {
			return err
		}
		if cur != expected {
			return ErrElementGone
		}
		var rem *redis.IntCmd
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.LSet(ctx, key, index, tombstone)
			rem = p.LRem(ctx, key, 1, tombstone)
			return nil
		})
		if err != nil {
			return err
		}
		removed = rem.Val()
		return nil
	}
	err := cl.Watch(ctx, txf, key)
	if errors.Is(err, redis.TxFailedErr) {
		return 0, ErrElementGone
	}
	if err != nil {
		return 0, err
	}
	return removed, nil
}
