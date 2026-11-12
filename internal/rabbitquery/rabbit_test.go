package rabbitquery

import (
	"crypto/rand"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

func TestPeekInfo(t *testing.T) {
	cases := []struct {
		name    string
		m       queueMeta
		version string
		mode    string
		risk    string
		limit   int
	}{
		{"classic", queueMeta{Type: "classic"}, "4.4.0", "requeue", "redelivered", 0},
		{"stream", queueMeta{Type: "stream"}, "3.13.7", "stream", "none", 0},
		// 4.x: requeued gets bump x-acquired-count, not the delivery limit.
		{"quorum 4.x default limit", queueMeta{Type: "quorum", DeliveryLimit: float64(20)}, "4.4.0", "requeue", "redelivered", 20},
		{"quorum 3.x no limit", queueMeta{Type: "quorum"}, "3.13.7", "requeue", "redelivered", 0},
		{"quorum 3.x arg limit", queueMeta{Type: "quorum", Arguments: map[string]any{"x-delivery-limit": float64(3)}}, "3.13.7", "requeue", "delivery_limit", 3},
		{"quorum 3.x policy limit", queueMeta{Type: "quorum", Effective: map[string]any{"delivery-limit": float64(5)}}, "3.12.1", "requeue", "delivery_limit", 5},
		{"quorum lower of arg and policy", queueMeta{Type: "quorum", Arguments: map[string]any{"x-delivery-limit": float64(9)}, Effective: map[string]any{"delivery-limit": float64(4)}}, "3.13.0", "requeue", "delivery_limit", 4},
	}
	for _, c := range cases {
		p := peekInfo(c.m, c.version)
		if p.Mode != c.mode || p.Risk != c.risk || p.DeliveryLimit != c.limit {
			t.Errorf("%s: got %+v", c.name, p)
		}
	}
	if !peekInfo(queueMeta{Type: "classic", Effective: map[string]any{"dead-letter-exchange": "dlx"}}, "4.0.0").HasDLX {
		t.Error("policy DLX not detected")
	}
}

func TestTagList(t *testing.T) {
	if got := tagList("administrator, monitoring"); !reflect.DeepEqual(got, []string{"administrator", "monitoring"}) {
		t.Errorf("3.x tags: %v", got)
	}
	if got := tagList([]any{"management"}); !reflect.DeepEqual(got, []string{"management"}) {
		t.Errorf("4.x tags: %v", got)
	}
	if got := tagList(nil); len(got) != 0 {
		t.Errorf("nil tags: %v", got)
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"rabbit.example.com:15672":        "http://rabbit.example.com:15672",
		"https://rabbit.example.com/api/": "https://rabbit.example.com",
		"http://h:15672/rabbitmq/":        "http://h:15672/rabbitmq",
		" http://h:15672/rabbitmq/api ":   "http://h:15672/rabbitmq",
	} {
		got, err := BaseURL(models.RabbitConnection{URL: in})
		if err != nil || got != want {
			t.Errorf("%q → %q, %v (want %q)", in, got, err, want)
		}
	}
	if _, err := BaseURL(models.RabbitConnection{URL: ""}); err == nil {
		t.Error("empty URL accepted")
	}
}

func TestAMQPURI(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(key))
	enc, _ := cipher.Encrypt("p@ss/word")
	r := NewRegistry(cipher)
	conn := models.RabbitConnection{URL: "https://mq.example.com:15671", Username: "app", PasswordEnc: enc}
	uri, tls, err := r.amqpURI(conn, "/")
	if err != nil || tls || !strings.HasPrefix(uri, "amqp://app:p%40ss%2Fword@mq.example.com:5672/%2F") {
		t.Errorf("default vhost uri %q tls=%v err=%v", uri, tls, err)
	}
	uri, _, _ = r.amqpURI(conn, "orders")
	if !strings.HasSuffix(uri, ":5672/orders") {
		t.Errorf("named vhost uri %q", uri)
	}
	conn.OptionsJSON = `{"amqpHost":"amqp.internal","amqpTLS":true}`
	uri, tls, _ = r.amqpURI(conn, "/")
	if !tls || !strings.HasPrefix(uri, "amqps://app:p%40ss%2Fword@amqp.internal:5671/") {
		t.Errorf("tls uri %q", uri)
	}
	conn.OptionsJSON = `{"amqpPort":"5674"}`
	uri, _, _ = r.amqpURI(conn, "/")
	if !strings.Contains(uri, "mq.example.com:5674") {
		t.Errorf("port override %q", uri)
	}
}

func TestStreamOffset(t *testing.T) {
	if streamOffset("") != "first" || streamOffset("last") != "last" || streamOffset("next") != "next" {
		t.Error("named offsets")
	}
	if streamOffset("42") != int64(42) {
		t.Errorf("numeric offset: %#v", streamOffset("42"))
	}
	if ts, ok := streamOffset("1792900000000").(time.Time); !ok || ts.UnixMilli() != 1792900000000 {
		t.Errorf("epoch ms: %#v", streamOffset("1792900000000"))
	}
	if _, ok := streamOffset("2026-10-26T10:00:00Z").(time.Time); !ok {
		t.Error("RFC3339")
	}
}
