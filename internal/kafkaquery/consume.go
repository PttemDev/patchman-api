package kafkaquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"patchman-be/internal/models"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Consume peeks messages from a topic WITHOUT joining a consumer group
// and WITHOUT committing any offset. It builds a throwaway client with a
// direct partition assignment, polls until it has the requested count (or
// reaches the end / times out), then closes the client. Because there is
// no group, Kafka has no offset to commit — browsing is invisible to real
// consumers.
//
// This is the single guarantee behind the product requirement "auto-fetch
// but NEVER commit": there is no CommitRecords/CommitOffsets call anywhere
// in this package, and kgo.ConsumerGroup is never set.
//
// Peek budget: the poll loop stops once it has the target, after
// peekIdleTimeout without a new record, or at peekMaxDuration / the ctx
// deadline — whichever comes first — and returns what arrived (TimedOut
// set when that is short of the target).
// Peek fetch sizing and budget.
//
// franz-go's default lets one fetch response carry up to 50 MB and only
// hands records over once the whole response is read. "Last N" over many
// partitions puts every partition's tail batch into that one response; on
// a bulk-produced topic (≈1 MB batches) over a slow link (VPN, desktop) it
// took longer than the old 8 s budget, so the peek came back empty while a
// single partition worked. A 1 MB cap makes the broker answer with about
// one tail batch per response (Kafka still sends a first batch larger than
// the cap whole), so records arrive incrementally, and the budget keeps
// polling as long as they do.
const (
	peekFetchMaxBytes = 1 << 20
	peekPollTimeout   = 1500 * time.Millisecond
	peekIdleTimeout   = 8 * time.Second  // no new record for this long → stop
	peekMaxDuration   = 20 * time.Second // hard cap even while records arrive
)

func (r *Registry) Consume(ctx context.Context, conn models.KafkaConnection, req ConsumeRequest) (*ConsumeResult, error) {
	if req.Topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	n := req.N
	if n <= 0 {
		n = 100
	}
	if n > 5000 {
		n = 5000 // hard ceiling — browse tool, not a firehose
	}
	mode := req.Mode
	if mode == "" {
		mode = "last"
	}

	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}

	// Resolve the partitions + their start/end offsets so we can bound the
	// peek precisely (and know when we've reached the end).
	startOffsets, err := adm.ListStartOffsets(ctx, req.Topic)
	if err != nil {
		return nil, fmt.Errorf("list start offsets: %w", err)
	}
	endOffsets, err := adm.ListEndOffsets(ctx, req.Topic)
	if err != nil {
		return nil, fmt.Errorf("list end offsets: %w", err)
	}
	ends, ok := endOffsets[req.Topic]
	if !ok || len(ends) == 0 {
		return &ConsumeResult{Messages: []Message{}, Partitions: 0}, nil
	}
	starts := startOffsets[req.Topic]

	// Which partitions are we reading?
	parts := make([]int32, 0, len(ends))
	for p := range ends {
		if req.Partition >= 0 && p != req.Partition {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return &ConsumeResult{Messages: []Message{}, Partitions: 0}, nil
	}

	// timestamp mode needs a per-partition offset lookup up front.
	var tsOffsets map[int32]int64
	if mode == "timestamp" {
		lo, err := adm.ListOffsetsAfterMilli(ctx, req.TimestampMs, req.Topic)
		if err != nil {
			return nil, fmt.Errorf("list offsets after timestamp: %w", err)
		}
		tsOffsets = map[int32]int64{}
		for p, o := range lo[req.Topic] {
			tsOffsets[p] = o.Offset
		}
	}

	perPart := n/len(parts) + 1
	assignment := map[int32]kgo.Offset{}
	unread := map[int32]int64{} // assigned partition → its last offset (end - 1) until seen
	available := 0
	for _, p := range parts {
		start := starts[p].Offset
		end := ends[p].Offset
		if end <= start {
			continue // empty partition
		}
		var at int64
		switch mode {
		case "first":
			at = start
		case "offset":
			at = req.Offset
			if at < start {
				at = start
			}
		case "timestamp":
			o, has := tsOffsets[p]
			if !has || o < 0 {
				continue // no record at/after the timestamp on this partition
			}
			at = o
		default: // "last"
			at = end - int64(perPart)
			if at < start {
				at = start
			}
		}
		if at >= end {
			continue
		}
		assignment[p] = kgo.NewOffset().At(at)
		unread[p] = end - 1
		available += int(end - at)
	}
	if len(assignment) == 0 {
		return &ConsumeResult{Messages: []Message{}, Partitions: len(parts)}, nil
	}

	target := n
	if available < target {
		target = available
	}

	// Throwaway consumer: direct partition assignment, NO group.
	cfg, err := r.resolve(conn)
	if err != nil {
		return nil, err
	}
	opts, err := baseOpts(cfg)
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{req.Topic: assignment}),
		kgo.FetchMaxBytes(peekFetchMaxBytes),
		// Transaction markers occupy offsets; seeing them is how a
		// partition whose tail is a commit marker counts as read to its
		// end. They are never returned as messages.
		kgo.KeepControlRecords(),
	)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	defer cl.Close()

	start := time.Now()
	hardStop := start.Add(peekMaxDuration)
	lastRecord := start // last poll that returned any record (data or control)
	msgs := make([]Message, 0, target)
	truncated, timedOut := false, false

	// The offset range can hold fewer records than `target` counts
	// (transaction markers, compaction gaps), so every partition read to
	// its end also finishes the peek — without waiting out the idle timeout.
	for len(msgs) < target && len(unread) > 0 {
		now := time.Now()
		if ctx.Err() != nil || now.After(hardStop) || now.Sub(lastRecord) >= peekIdleTimeout {
			timedOut = true
			break
		}
		pollCtx, cancel := context.WithTimeout(ctx, peekPollTimeout)
		fetches := cl.PollFetches(pollCtx)
		cancel()
		if errs := fetches.Errors(); len(errs) > 0 {
			// Surface the first real (non-context) error; a context
			// deadline/cancel on a poll just means "no more right now".
			for _, e := range errs {
				if e.Err != nil && !errors.Is(e.Err, context.DeadlineExceeded) && !errors.Is(e.Err, context.Canceled) {
					return nil, fmt.Errorf("fetch %s[%d]: %w", e.Topic, e.Partition, e.Err)
				}
			}
		}
		progressed := false
		fetches.EachRecord(func(rec *kgo.Record) {
			progressed = true
			if last, ok := unread[rec.Partition]; ok && rec.Offset >= last {
				delete(unread, rec.Partition)
			}
			if rec.Attrs.IsControl() {
				return
			}
			if len(msgs) >= n {
				truncated = true
				return
			}
			val, enc := encodeBytes(rec.Value)
			var key *string
			if rec.Key != nil {
				k, _ := encodeBytes(rec.Key)
				key = &k
			}
			var headers []MessageHeader
			for _, h := range rec.Headers {
				hv, _ := encodeBytes(h.Value)
				headers = append(headers, MessageHeader{Key: h.Key, Value: hv})
			}
			msgs = append(msgs, Message{
				Partition:     rec.Partition,
				Offset:        rec.Offset,
				TimestampMs:   rec.Timestamp.UnixMilli(),
				Key:           key,
				Value:         val,
				ValueEncoding: enc,
				Headers:       headers,
			})
		})
		if progressed {
			lastRecord = time.Now()
		}
	}
	if len(msgs) > n {
		msgs = msgs[:n]
		truncated = true
	}
	sortMessages(msgs)
	return &ConsumeResult{
		Messages:   msgs,
		Partitions: len(assignment),
		Truncated:  truncated,
		TimedOut:   timedOut,
		FetchMs:    int(time.Since(start).Milliseconds()),
	}, nil
}
