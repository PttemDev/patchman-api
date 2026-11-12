package rabbitquery

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// Live tests against a real broker provisioned like the scratchpad
// rabbit/provision.sh (vhost "/" with orders.events → orders.created /
// orders.paid (quorum) / audit.stream, dlx → orders.dlq, …):
//
//	PATCHMAN_TEST_RABBIT=http://localhost:15674 PATCHMAN_TEST_RABBIT_AUTH=admin:admin \
//	PATCHMAN_TEST_RABBIT_AMQP_PORT=5674 go test ./internal/rabbitquery -run Live -v
func liveConn(t *testing.T) (*Registry, models.RabbitConnection) {
	t.Helper()
	u := os.Getenv("PATCHMAN_TEST_RABBIT")
	auth := os.Getenv("PATCHMAN_TEST_RABBIT_AUTH")
	if u == "" || auth == "" {
		t.Skip("PATCHMAN_TEST_RABBIT / PATCHMAN_TEST_RABBIT_AUTH not set")
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	user, pass, _ := strings.Cut(auth, ":")
	enc, _ := cipher.Encrypt(pass)
	opts := map[string]any{}
	if p := os.Getenv("PATCHMAN_TEST_RABBIT_AMQP_PORT"); p != "" {
		opts["amqpPort"] = p
	}
	ob, _ := json.Marshal(opts)
	return NewRegistry(cipher), models.RabbitConnection{ID: "live", URL: u, Username: user, PasswordEnc: enc, OptionsJSON: string(ob), UpdatedAt: time.Unix(1, 0)}
}

func TestLiveRabbit(t *testing.T) {
	reg, conn := liveConn(t)
	ctx := context.Background()

	tr, err := reg.Test(ctx, conn)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	t.Logf("version %s cluster %s user %s tags %v vhosts %v amqp=%v %s", tr.Version, tr.ClusterName, tr.User, tr.Tags, tr.Vhosts, tr.AMQPOK, tr.AMQPError)
	if !tr.AMQPOK {
		t.Fatalf("AMQP probe failed: %s", tr.AMQPError)
	}

	ov, err := reg.Overview(ctx, conn)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(ov.Nodes) == 0 {
		t.Errorf("nodes missing: %s", ov.NodesError)
	}

	qs, err := reg.Queues(ctx, conn, ListParams{Vhost: "/", Name: "^orders", Regex: true})
	if err != nil {
		t.Fatalf("queues: %v", err)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(qs, &page)
	if len(page.Items) < 3 {
		t.Fatalf("expected ≥3 orders queues, got %s", qs)
	}
	if _, ok := page.Items[0]["type"]; !ok {
		t.Errorf("columns dropped type: %v", page.Items[0])
	}

	for name, mode := range map[string]string{"orders.created": "requeue", "orders.paid": "requeue", "audit.stream": "stream"} {
		qd, err := reg.Queue(ctx, conn, "/", name)
		if err != nil {
			t.Fatalf("queue %s: %v", name, err)
		}
		if qd.Peek.Mode != mode {
			t.Errorf("%s peek mode %s, want %s", name, qd.Peek.Mode, mode)
		}
		t.Logf("%s peek %+v", name, qd.Peek)
	}

	// orders.created has a 10-minute TTL: publish a fresh message to peek at.
	if _, err := reg.Publish(ctx, conn, PublishParams{Vhost: "/", Exchange: "orders.events", RoutingKey: "order.created.live", Payload: `{"live":true}`}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msgs, err := reg.GetMessages(ctx, conn, GetParams{Vhost: "/", Queue: "orders.created", Count: 3})
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	var got []map[string]any
	_ = json.Unmarshal(msgs, &got)
	if len(got) == 0 {
		t.Fatalf("peek returned nothing: %s", msgs)
	}

	sm, err := reg.StreamRead(ctx, conn, StreamParams{Vhost: "/", Queue: "audit.stream", Offset: "first", Count: 5})
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if len(sm) == 0 || sm[0].Offset == nil {
		t.Fatalf("stream read returned %d messages (offset %v)", len(sm), sm)
	}
	t.Logf("stream first offset %d payload %.40s", *sm[0].Offset, sm[0].Payload)

	// Tail orders.events while publishing one message.
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	recv := make(chan Message, 4)
	go func() {
		_ = reg.Tail(tctx, conn, TailParams{Vhost: "/", Exchange: "orders.events", RoutingKey: "#"},
			func(m Message) error { recv <- m; cancel(); return nil }, func() error { return nil })
	}()
	time.Sleep(time.Second)
	if _, err := reg.Publish(ctx, conn, PublishParams{Vhost: "/", Exchange: "orders.events", RoutingKey: "order.tail.test", Payload: "tail me"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case m := <-recv:
		if m.Payload != "tail me" || m.RoutingKey != "order.tail.test" {
			t.Errorf("tail got %+v", m)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("tail received nothing")
	}

	// Move: dlq → back to origin (x-death), then the header-less one fails cleanly.
	res, err := reg.Move(ctx, conn, MoveParams{Vhost: "/", Source: "orders.dlq", Target: "origin", Count: 2})
	if err != nil {
		t.Fatalf("move origin: %v", err)
	}
	t.Logf("move origin: %+v", res)
	if res.Moved == 0 {
		t.Errorf("nothing moved")
	}
	res, err = reg.Move(ctx, conn, MoveParams{Vhost: "/", Source: "orders.dlq", Target: "queue", TargetQueue: "eu.orders", Count: 1})
	if err != nil || res.Moved != 1 {
		t.Fatalf("move queue: %+v %v", res, err)
	}
	res, err = reg.Move(ctx, conn, MoveParams{Vhost: "/", Source: "eu.orders", Target: "exchange", TargetExchange: "nope.unbound", Count: 1})
	if err == nil && res.Stopped == "" {
		t.Errorf("move to a missing exchange should stop: %+v", res)
	}
	t.Logf("move to missing exchange: %+v %v", res, err)

	topo, err := reg.Topology(ctx, conn, "/")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if !strings.Contains(string(topo.Bindings), "order.created.*") {
		t.Errorf("bindings missing: %.200s", topo.Bindings)
	}
	ex, err := reg.Exchange(ctx, conn, "/", "")
	if err != nil {
		t.Fatalf("default exchange: %v", err)
	}
	_ = ex
}

// TestLiveRabbitWrites runs every broker-changing call on throwaway names.
func TestLiveRabbitWrites(t *testing.T) {
	reg, conn := liveConn(t)
	ctx := context.Background()
	ex, q := "pm.test.ex", "pm.test.q"
	_ = reg.DeleteQueue(ctx, conn, "/", q, false, false)
	_ = reg.DeleteExchange(ctx, conn, "/", ex)
	if err := reg.CreateExchange(ctx, conn, ExchangeSpec{Vhost: "/", Name: ex, Type: "direct", Durable: true}); err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if err := reg.CreateQueue(ctx, conn, QueueSpec{Vhost: "/", Name: q, Durable: true, Arguments: map[string]any{"x-queue-type": "quorum"}}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	for _, key := range []string{"k1", ""} {
		if err := reg.Bind(ctx, conn, BindingSpec{Vhost: "/", Source: ex, Destination: q, DestType: "queue", RoutingKey: key}); err != nil {
			t.Fatalf("bind %q: %v", key, err)
		}
	}
	for _, key := range []string{"k1", ""} {
		routed, err := reg.Publish(ctx, conn, PublishParams{Vhost: "/", Exchange: ex, RoutingKey: key, Payload: "x"})
		if err != nil || !routed {
			t.Fatalf("publish %q: routed=%v err=%v", key, routed, err)
		}
	}
	if err := reg.Purge(ctx, conn, "/", q); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, key := range []string{"k1", ""} {
		if err := reg.Unbind(ctx, conn, BindingSpec{Vhost: "/", Source: ex, Destination: q, DestType: "queue", RoutingKey: key}); err != nil {
			t.Fatalf("unbind %q: %v", key, err)
		}
	}
	if routed, _ := reg.Publish(ctx, conn, PublishParams{Vhost: "/", Exchange: ex, RoutingKey: "k1", Payload: "x"}); routed {
		t.Error("still routed after unbind")
	}
	// Quorum queues support neither if-empty nor if-unused; delete plainly.
	if err := reg.DeleteQueue(ctx, conn, "/", q, false, false); err != nil {
		t.Fatalf("delete queue: %v", err)
	}
	// Classic queues honour if-empty.
	cq := "pm.test.classic"
	if err := reg.CreateQueue(ctx, conn, QueueSpec{Vhost: "/", Name: cq, Durable: true}); err != nil {
		t.Fatalf("create classic: %v", err)
	}
	if err := reg.DeleteQueue(ctx, conn, "/", cq, true, true); err != nil {
		t.Fatalf("delete classic (if-empty, if-unused): %v", err)
	}
	if err := reg.DeleteExchange(ctx, conn, "/", ex); err != nil {
		t.Fatalf("delete exchange: %v", err)
	}
	if err := reg.DeleteExchange(ctx, conn, "/", "amq.direct"); err == nil {
		t.Error("built-in exchange delete allowed")
	}

	// Close PatchMan's own pooled AMQP connection; the next AMQP call redials.
	if _, err := reg.StreamRead(ctx, conn, StreamParams{Vhost: "/", Queue: "audit.stream", Count: 1}); err != nil {
		t.Fatalf("stream read: %v", err)
	}
	time.Sleep(6 * time.Second) // management stats list new connections with a delay
	raw, err := reg.Connections(ctx, conn, ListParams{PageSize: 500})
	if err != nil {
		t.Fatalf("connections: %v", err)
	}
	var page struct {
		Items []struct {
			Name             string         `json:"name"`
			ClientProperties map[string]any `json:"client_properties"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &page)
	closed := 0
	for _, c := range page.Items {
		if c.ClientProperties["connection_name"] == "PatchMan" {
			if err := reg.CloseConnection(ctx, conn, c.Name, "live test"); err != nil {
				t.Fatalf("close connection: %v", err)
			}
			closed++
		}
	}
	if closed == 0 {
		t.Fatalf("PatchMan's AMQP connection not listed: %.300s", raw)
	}
	time.Sleep(time.Second)
	if _, err := reg.StreamRead(ctx, conn, StreamParams{Vhost: "/", Queue: "audit.stream", Count: 1}); err != nil {
		t.Fatalf("stream read after the connection was closed: %v", err)
	}
}
