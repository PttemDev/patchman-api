package kafkaquery

import (
	"context"

	"github.com/twmb/franz-go/pkg/kadm"
)

// partSize is one partition's on-disk size: the LEADER replica's log size
// (≈ the partition's data) and the sum over every replica (disk footprint).
// leader is -1 when the leading broker didn't report the partition.
type partSize struct {
	leader   int64
	replicas int64
}

// topicLeaders maps topic → partition → leader broker id, from metadata.
type topicLeaders map[string]map[int32]int32

// aggregateSizes folds DescribeLogDirs output (broker → dir → topic →
// partition) into per-partition sizes. The leader size comes from the broker
// that leads the partition; "future" replicas (an in-progress log-dir move)
// are skipped so a move doesn't double count.
func aggregateSizes(all kadm.DescribedAllLogDirs, leaders topicLeaders) map[string]map[int32]partSize {
	out := map[string]map[int32]partSize{}
	for broker, dirs := range all {
		for _, dir := range dirs {
			if dir.Err != nil {
				continue
			}
			for topic, parts := range dir.Topics {
				for p, dp := range parts {
					if dp.IsFuture {
						continue
					}
					m := out[topic]
					if m == nil {
						m = map[int32]partSize{}
						out[topic] = m
					}
					ps, ok := m[p]
					if !ok {
						ps = partSize{leader: -1}
					}
					ps.replicas += dp.Size
					if l, ok := leaders[topic][p]; ok && l == broker {
						ps.leader = dp.Size
					}
					m[p] = ps
				}
			}
		}
	}
	return out
}

// sizeReport is the outcome of describeSizes. ok=false means nothing could be
// described (typically no DESCRIBE-on-CLUSTER ACL) — every size is unknown.
// complete=false means some broker failed: leader sizes are still exact where
// the leader answered, but replica totals would under-count, so callers
// report them as unknown.
type sizeReport struct {
	parts    map[string]map[int32]partSize
	ok       bool
	complete bool
}

// describeSizes runs DescribeLogDirs for `set` (nil = every topic) across all
// brokers and aggregates per partition. Best-effort by design: sizes are an
// enrichment, never a reason to fail the topic view.
func describeSizes(ctx context.Context, adm *kadm.Client, set kadm.TopicsSet, leaders topicLeaders) sizeReport {
	all, err := adm.DescribeAllLogDirs(ctx, set)
	if len(all) == 0 {
		return sizeReport{}
	}
	return sizeReport{parts: aggregateSizes(all, leaders), ok: true, complete: err == nil}
}

// topicTotals sums one topic's partition sizes for the given partition ids:
// leader total (-1 if any partition's leader size is unknown) and replica
// total (-1 unless the report is complete).
func (r sizeReport) topicTotals(topic string, partitions []int32) (leader, replicas int64) {
	if !r.ok {
		return -1, -1
	}
	leader, replicas = 0, 0
	for _, p := range partitions {
		ps, ok := r.parts[topic][p]
		if !ok || ps.leader < 0 {
			leader = -1
		} else if leader >= 0 {
			leader += ps.leader
		}
		if ok {
			replicas += ps.replicas
		}
	}
	if !r.complete {
		replicas = -1
	}
	return leader, replicas
}

// partition returns one partition's (leader, replicas) sizes, -1 when unknown.
func (r sizeReport) partition(topic string, p int32) (leader, replicas int64) {
	if !r.ok {
		return -1, -1
	}
	ps, ok := r.parts[topic][p]
	if !ok {
		return -1, -1
	}
	replicas = ps.replicas
	if !r.complete {
		replicas = -1
	}
	return ps.leader, replicas
}
