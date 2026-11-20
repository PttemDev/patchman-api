package redisquery

import (
	"context"
	"strconv"
	"strings"

	"patchman-be/internal/models"
)

// Keyspace returns the number of keys in each logical database that has any
// (INFO keyspace: "db0:keys=12,expires=0,avg_ttl=0"). Databases without keys
// are absent. Servers that disable INFO return an error; callers treat the
// counts as optional.
func (r *Registry) Keyspace(ctx context.Context, conn models.RedisConnection) (map[int]int64, error) {
	cl, err := r.client(conn, conn.DBIndex)
	if err != nil {
		return nil, err
	}
	raw, err := cl.Info(ctx, "keyspace").Result()
	if err != nil {
		return nil, err
	}
	return parseKeyspace(raw), nil
}

func parseKeyspace(raw string) map[int]int64 {
	out := map[int]int64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		name, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(name, "db") {
			continue
		}
		db, err := strconv.Atoi(strings.TrimPrefix(name, "db"))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(rest, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok && k == "keys" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					out[db] = n
				}
			}
		}
	}
	return out
}
