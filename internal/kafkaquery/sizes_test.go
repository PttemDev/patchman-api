package kafkaquery

import (
	"testing"

	"github.com/twmb/franz-go/pkg/kadm"
)

func logDir(broker int32, parts ...kadm.DescribedLogDirPartition) kadm.DescribedLogDirs {
	topics := kadm.DescribedLogDirTopics{}
	for _, p := range parts {
		p.Broker = broker
		if topics[p.Topic] == nil {
			topics[p.Topic] = map[int32]kadm.DescribedLogDirPartition{}
		}
		topics[p.Topic][p.Partition] = p
	}
	return kadm.DescribedLogDirs{"/data": {Broker: broker, Dir: "/data", Topics: topics}}
}

func TestAggregateSizes(t *testing.T) {
	// orders: p0 led by 1 (replica on 2), p1 led by 2 (replica on 1).
	// Broker 2 also has a FUTURE copy of p0 that must not be counted.
	all := kadm.DescribedAllLogDirs{
		1: logDir(1,
			kadm.DescribedLogDirPartition{Topic: "orders", Partition: 0, Size: 100},
			kadm.DescribedLogDirPartition{Topic: "orders", Partition: 1, Size: 40}),
		2: logDir(2,
			kadm.DescribedLogDirPartition{Topic: "orders", Partition: 0, Size: 90},
			kadm.DescribedLogDirPartition{Topic: "orders", Partition: 1, Size: 50}),
	}
	all[2]["/data2"] = kadm.DescribedLogDir{Broker: 2, Dir: "/data2", Topics: kadm.DescribedLogDirTopics{
		"orders": {0: {Broker: 2, Topic: "orders", Partition: 0, Size: 999, IsFuture: true}},
	}}
	leaders := topicLeaders{"orders": {0: 1, 1: 2}}

	r := sizeReport{parts: aggregateSizes(all, leaders), ok: true, complete: true}
	if l, rep := r.partition("orders", 0); l != 100 || rep != 190 {
		t.Fatalf("p0 = (%d,%d), want (100,190)", l, rep)
	}
	if l, rep := r.partition("orders", 1); l != 50 || rep != 90 {
		t.Fatalf("p1 = (%d,%d), want (50,90)", l, rep)
	}
	if l, rep := r.topicTotals("orders", []int32{0, 1}); l != 150 || rep != 280 {
		t.Fatalf("topic = (%d,%d), want (150,280)", l, rep)
	}

	// A broker failed: leader sizes stay exact where the leader answered, the
	// replica total is unknown (it would under-count).
	delete(all, 2)
	r = sizeReport{parts: aggregateSizes(all, leaders), ok: true, complete: false}
	if l, rep := r.partition("orders", 0); l != 100 || rep != -1 {
		t.Fatalf("partial p0 = (%d,%d), want (100,-1)", l, rep)
	}
	if l, rep := r.topicTotals("orders", []int32{0, 1}); l != -1 || rep != -1 {
		t.Fatalf("partial topic = (%d,%d), want (-1,-1) — p1's leader (2) didn't report", l, rep)
	}

	// Nothing described (no ACL): everything unknown.
	if l, rep := (sizeReport{}).topicTotals("orders", []int32{0}); l != -1 || rep != -1 {
		t.Fatalf("no-report totals = (%d,%d), want (-1,-1)", l, rep)
	}
}
