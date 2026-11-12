package kafkaquery

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"patchman-be/internal/models"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Live "Last N over all partitions" peek against a real broker, gated by
// PATCHMAN_TEST_KAFKA (bootstrap servers, e.g. localhost:19092). The topic
// (PATCHMAN_TEST_KAFKA_TOPIC, default peek30) must exist with 30 partitions;
// PATCHMAN_TEST_KAFKA_PRODUCE=1 first fills every partition with
// bulk-produced, lz4-compressed ≈1 MB batches; =tx with small committed
// transactions instead (every partition then ends on a commit marker).
//
// The regression this guards only shows on a slow link. Throttle the broker
// container's egress before running (and `tc qdisc del dev eth0 root` after):
//
//	docker run --rm --net container:<kafka> --cap-add NET_ADMIN nicolaka/netshoot \
//	  tc qdisc add dev eth0 root tbf rate 3mbit burst 128kb latency 2s
//
// With franz-go's default 50 MB fetch responses that returned 0 messages
// after ~9 s; with the 1 MB cap the records arrive incrementally.
func TestConsumeLive_LastNAllPartitions(t *testing.T) {
	brokers := os.Getenv("PATCHMAN_TEST_KAFKA")
	if brokers == "" {
		t.Skip("PATCHMAN_TEST_KAFKA not set")
	}
	topic := os.Getenv("PATCHMAN_TEST_KAFKA_TOPIC")
	if topic == "" {
		topic = "peek30"
	}
	ctx := context.Background()
	switch os.Getenv("PATCHMAN_TEST_KAFKA_PRODUCE") {
	case "":
	case "tx":
		produceTransactions(t, ctx, brokers, topic, 30)
	default:
		produceBulk(t, ctx, brokers, topic, 30, 2000)
	}

	r := NewRegistry(nil)
	conn := models.KafkaConnection{BootstrapServers: brokers, SecurityProtocol: "PLAINTEXT"}
	conn.ID = "live-test"
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		res, err := r.Consume(ctx, conn, ConsumeRequest{Topic: topic, N: 100, Mode: "last", Partition: -1})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		t.Logf("run %d: %d msgs from %d partitions in %v (timedOut=%v)", i, len(res.Messages), res.Partitions, time.Since(t0).Round(time.Millisecond), res.TimedOut)
		if len(res.Messages) == 0 {
			t.Errorf("run %d: no messages", i)
		}
		// Every partition read to its end finishes the peek: a fully read
		// transactional topic (fewer records than offsets) is not "timed out".
		if res.TimedOut && os.Getenv("PATCHMAN_TEST_KAFKA_PRODUCE") == "tx" {
			t.Errorf("run %d: transactional tail reported as timed out", i)
		}
	}
}

func produceBulk(t *testing.T, ctx context.Context, brokers, topic string, partitions int32, perPartition int) {
	t.Helper()
	p, err := kgo.NewClient(kgo.SeedBrokers(brokers), kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()), kgo.MaxBufferedRecords(200000))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rnd := rand.New(rand.NewSource(1))
	buf := make([]byte, 1500)
	var recs []*kgo.Record
	for part := int32(0); part < partitions; part++ {
		for i := 0; i < perPartition; i++ {
			rnd.Read(buf)
			recs = append(recs, &kgo.Record{Topic: topic, Partition: part, Value: []byte(fmt.Sprintf(`{"id":"p%d-%d","blob":"%x"}`, part, i, buf))})
		}
	}
	if err := p.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		t.Fatal(err)
	}
}

func produceTransactions(t *testing.T, ctx context.Context, brokers, topic string, partitions int32) {
	t.Helper()
	p, err := kgo.NewClient(kgo.SeedBrokers(brokers), kgo.RecordPartitioner(kgo.ManualPartitioner()), kgo.TransactionalID("patchman-peek-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for txn := 0; txn < 5; txn++ {
		if err := p.BeginTransaction(); err != nil {
			t.Fatal(err)
		}
		for part := int32(0); part < partitions; part++ {
			for i := 0; i < 5; i++ {
				p.Produce(ctx, &kgo.Record{Topic: topic, Partition: part, Value: []byte(fmt.Sprintf("tx%d-p%d-%d", txn, part, i))}, nil)
			}
		}
		if err := p.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.EndTransaction(ctx, kgo.TryCommit); err != nil {
			t.Fatal(err)
		}
	}
}
