package redisquery

import "testing"

func TestParseKeyspace(t *testing.T) {
	got := parseKeyspace("# Keyspace\r\ndb0:keys=12,expires=1,avg_ttl=0\r\ndb3:keys=4,expires=0,avg_ttl=0\r\nbogus\r\n")
	if len(got) != 2 || got[0] != 12 || got[3] != 4 {
		t.Fatalf("got %v", got)
	}
}
