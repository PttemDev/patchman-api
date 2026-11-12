package mcp

import (
	"strings"
	"testing"
)

func TestCompactGraylogSearch(t *testing.T) {
	body := `{"data":{"total":2,"interval":"1m","from":"2026-11-12T07:00:00Z","to":"2026-11-12T08:00:00Z","webUrl":"https://gl/search?q=x",
		"histogram":[{"time":"2026-11-12T07:00:00.000Z","count":0},{"time":"2026-11-12T07:01:00.000Z","count":2}],
		"messages":[{"timestamp":"2026-11-12T07:01:02.5Z","source":"api-1","message":"POST /orders\n failed","fields":{"service":"order-api","trace_id":"t1","streams":["s"]}}]}}`
	out := compactGraylogSearch(body, nil)
	for _, want := range []string{
		"2 messages match between 2026-11-12T07:00:00Z and 2026-11-12T08:00:00Z; showing 1.",
		"Histogram: 2 buckets of 1m, peak 2 at 2026-11-12T07:01:00.000Z.",
		"In Graylog: https://gl/search?q=x",
		"2026-11-12T07:01:02.500Z  api-1  POST /orders failed | service=order-api | trace_id=t1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "streams=") {
		t.Fatalf("streams field should be dropped:\n%s", out)
	}
	if only := compactGraylogSearch(body, []string{"trace_id"}); strings.Contains(only, "service=") || !strings.Contains(only, "trace_id=t1") {
		t.Fatalf("fields filter:\n%s", only)
	}
}
