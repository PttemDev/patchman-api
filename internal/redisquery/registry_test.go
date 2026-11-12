package redisquery

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// No client name by default, and a configured one never goes through
// go-redis's ClientName (which fails the connection when CLIENT SETNAME is
// denied) — it's applied best-effort on connect instead.
func TestBuildOptionsClientName(t *testing.T) {
	opts, err := buildOptions(ConnectionConfig{Address: "h:6379"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ClientName != "" || opts.OnConnect != nil {
		t.Errorf("default: ClientName=%q OnConnect set=%v, want neither", opts.ClientName, opts.OnConnect != nil)
	}
	opts, _ = buildOptions(ConnectionConfig{Address: "h:6379", Options: map[string]any{"clientName": " ops-tool "}}, 0)
	if opts.ClientName != "" || opts.OnConnect == nil {
		t.Errorf("configured: ClientName=%q OnConnect set=%v, want best-effort OnConnect only", opts.ClientName, opts.OnConnect != nil)
	}
}

// Against a real Redis (PATCHMAN_REDIS_ACL_ADDR, e.g. a docker redis:7 with
// `ACL SETUSER devuser on >devpass ~* &* +@all -client`): a user without the
// CLIENT command can still work, with or without a configured name, and the
// default user still gets the name.
func TestRedisACLWithoutClientCommand(t *testing.T) {
	addr := os.Getenv("PATCHMAN_REDIS_ACL_ADDR")
	if addr == "" {
		t.Skip("set PATCHMAN_REDIS_ACL_ADDR to run against a real Redis")
	}
	ctx := context.Background()
	run := func(user, pass string, options map[string]any) (redis.UniversalClient, error) {
		opts, err := buildOptions(ConnectionConfig{Address: addr, Username: user, Password: pass, Options: options}, 0)
		if err != nil {
			return nil, err
		}
		cl := redis.NewUniversalClient(opts)
		_, _, err = cl.Scan(ctx, 0, "*", 10).Result()
		return cl, err
	}
	for _, options := range []map[string]any{nil, {"clientName": "patchman"}} {
		cl, err := run("devuser", "devpass", options)
		if err != nil {
			t.Errorf("devuser options=%v: %v", options, err)
		}
		if cl != nil {
			_ = cl.Close()
		}
	}
	cl, err := run("", "", map[string]any{"clientName": "patchman-test"})
	if err != nil {
		t.Fatalf("default user: %v", err)
	}
	defer cl.Close()
	if name, _ := cl.ClientGetName(ctx).Result(); name != "patchman-test" {
		t.Errorf("default user client name = %q, want patchman-test", name)
	}
}
