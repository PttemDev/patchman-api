package redisquery

import (
	"context"
	"fmt"
	"time"

	"patchman-be/internal/models"
)

// PeekChannel subscribes to one or more channels (or a glob pattern) on a
// throwaway subscription, collects up to n messages or until the deadline
// (8s), then unsubscribes and closes. It is commit-less by design: Redis
// pub/sub is fire-and-forget with no durable consumer state, so peeking a
// channel leaves no trace and never affects other subscribers.
func (r *Registry) PeekChannel(ctx context.Context, conn models.RedisConnection, db int, channels []string, pattern string, n int) (*PubSubResult, error) {
	if len(channels) == 0 && pattern == "" {
		return nil, fmt.Errorf("channels or pattern is required")
	}
	if n <= 0 {
		n = 50
	}
	if n > 1000 {
		n = 1000
	}
	cl, err := r.client(conn, db)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(8 * time.Second)
	subCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var receive func() (channel, pat, payload string, e error)
	var closeFn func()

	if pattern != "" {
		ps := cl.PSubscribe(subCtx, pattern)
		closeFn = func() { _ = ps.Close() }
		receive = func() (string, string, string, error) {
			m, e := ps.ReceiveMessage(subCtx)
			if e != nil {
				return "", "", "", e
			}
			return m.Channel, m.Pattern, m.Payload, nil
		}
	} else {
		ps := cl.Subscribe(subCtx, channels...)
		closeFn = func() { _ = ps.Close() }
		receive = func() (string, string, string, error) {
			m, e := ps.ReceiveMessage(subCtx)
			if e != nil {
				return "", "", "", e
			}
			return m.Channel, "", m.Payload, nil
		}
	}
	defer closeFn()

	start := time.Now()
	msgs := make([]PubSubMessage, 0, n)
	truncated := false
	for len(msgs) < n {
		if time.Now().After(deadline) {
			break
		}
		channel, pat, payload, rerr := receive()
		if rerr != nil {
			// Deadline / context cancellation just means "no more right now".
			break
		}
		enc := "utf8"
		p := payload
		if s, flag := encodeBytes([]byte(payload)); flag == "base64" {
			p = s
			enc = "base64"
		}
		msgs = append(msgs, PubSubMessage{
			Channel: channel, Pattern: pat, Payload: p, PayloadEncoding: enc,
		})
	}
	if len(msgs) >= n {
		truncated = true
	}
	return &PubSubResult{
		Messages:  msgs,
		Truncated: truncated,
		FetchMs:   int(time.Since(start).Milliseconds()),
	}, nil
}
