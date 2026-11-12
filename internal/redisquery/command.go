package redisquery

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"
)

// CommandClass is the risk classification of a console command.
type CommandClass int

const (
	ClassRead CommandClass = iota
	ClassWrite
	ClassBlocked
)

// readCommands are inspection commands — safe under a read grant.
var readCommands = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "EXISTS": true, "TYPE": true,
	"TTL": true, "PTTL": true, "HGET": true, "HGETALL": true, "HMGET": true,
	"HKEYS": true, "HVALS": true, "HLEN": true, "HSCAN": true, "LRANGE": true,
	"LINDEX": true, "LLEN": true, "SMEMBERS": true, "SSCAN": true, "SCARD": true,
	"SISMEMBER": true, "ZRANGE": true, "ZSCORE": true, "ZCARD": true, "ZRANK": true,
	"ZSCAN": true, "XRANGE": true, "XREVRANGE": true, "XLEN": true, "XINFO": true,
	"SCAN": true, "RANDOMKEY": true, "DBSIZE": true, "OBJECT": true, "MEMORY": true,
	"BITCOUNT": true, "PFCOUNT": true, "INFO": true, "PING": true, "ECHO": true,
	"TIME": true, "COMMAND": true, "LOLWUT": true,
}

// writeCommands mutate data — require a write grant.
var writeCommands = map[string]bool{
	"SET": true, "SETEX": true, "SETNX": true, "GETSET": true, "GETDEL": true,
	"APPEND": true, "INCR": true, "INCRBY": true, "DECR": true, "DEL": true,
	"UNLINK": true, "EXPIRE": true, "PEXPIRE": true, "PERSIST": true, "RENAME": true,
	"RENAMENX": true, "COPY": true, "HSET": true, "HSETNX": true, "HDEL": true,
	"HINCRBY": true, "LPUSH": true, "RPUSH": true, "LPOP": true, "RPOP": true,
	"LSET": true, "LREM": true, "LTRIM": true, "SADD": true, "SREM": true,
	"SPOP": true, "ZADD": true, "ZREM": true, "ZINCRBY": true, "ZPOPMIN": true,
	"ZPOPMAX": true, "XADD": true, "XDEL": true, "XTRIM": true, "PFADD": true,
}

// blockedCommands are never executed under any grant — administrative,
// dangerous, blocking, scripting, or subscription commands that have no
// place in a bounded browse tool.
var blockedCommands = map[string]bool{
	"KEYS": true, "FLUSHALL": true, "FLUSHDB": true, "SWAPDB": true, "CONFIG": true,
	"SHUTDOWN": true, "DEBUG": true, "SAVE": true, "BGSAVE": true, "BGREWRITEAOF": true,
	"SLAVEOF": true, "REPLICAOF": true, "CLUSTER": true, "FAILOVER": true, "MIGRATE": true,
	"SCRIPT": true, "EVAL": true, "EVALSHA": true, "FUNCTION": true, "MONITOR": true,
	"SUBSCRIBE": true, "PSUBSCRIBE": true, "BLPOP": true, "BRPOP": true, "WAIT": true,
	"ACL": true, "MODULE": true,
}

// Classify buckets a command name. Blocked wins over everything; an unknown
// command is Blocked (deny by default).
func Classify(cmd string) CommandClass {
	name := strings.ToUpper(strings.TrimSpace(cmd))
	if blockedCommands[name] {
		return ClassBlocked
	}
	if writeCommands[name] {
		return ClassWrite
	}
	if readCommands[name] {
		return ClassRead
	}
	return ClassBlocked
}

// RunCommand classifies args[0], rejects blocked commands outright, gates
// writes behind allowWrite, then executes via client.Do and renders the
// RESP reply to a JSON-friendly value (binary bulk strings become base64).
func (r *Registry) RunCommand(ctx context.Context, conn models.RedisConnection, db int, args []string, allowWrite bool) (*CommandResult, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("empty command")
	}
	class := Classify(args[0])
	switch class {
	case ClassBlocked:
		return nil, fmt.Errorf("command %q is not permitted", strings.ToUpper(args[0]))
	case ClassWrite:
		if !allowWrite {
			return nil, errWriteRequired
		}
	}

	cl, err := r.client(conn, db)
	if err != nil {
		return nil, err
	}
	iargs := make([]any, len(args))
	for i, a := range args {
		iargs[i] = a
	}
	start := time.Now()
	raw, cmdErr := cl.Do(ctx, iargs...).Result()
	dur := int(time.Since(start).Milliseconds())
	if cmdErr != nil && cmdErr.Error() != "redis: nil" {
		return nil, cmdErr
	}
	kind := "read"
	if class == ClassWrite {
		kind = "write"
	}
	return &CommandResult{
		Reply:      renderReply(raw),
		Kind:       kind,
		DurationMs: dur,
		Write:      class == ClassWrite,
	}, nil
}

// errWriteRequired is the sentinel the handler maps to a 403 so the console
// write-gate returns the same message as every other write endpoint.
var errWriteRequired = fmt.Errorf("write access required")

// IsWriteRequired reports whether an error is the write-gate sentinel.
func IsWriteRequired(err error) bool { return err == errWriteRequired }

// renderReply converts a raw go-redis reply into a JSON-friendly value.
// UTF-8 strings pass through; non-UTF-8 bulk strings become base64.
func renderReply(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if utf8.ValidString(t) {
			return t
		}
		return base64.StdEncoding.EncodeToString([]byte(t))
	case []byte:
		s, _ := encodeBytes(t)
		return s
	case int64, int, bool, float64:
		return t
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = renderReply(e)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for k, val := range t {
			out[fmt.Sprintf("%v", k)] = renderReply(val)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			out[k] = renderReply(val)
		}
		return out
	default:
		return fmt.Sprintf("%v", t)
	}
}
