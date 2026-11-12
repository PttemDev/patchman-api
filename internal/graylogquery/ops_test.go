package graylogquery

import (
	"testing"
	"time"
)

func TestBucketFor(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Minute:     "5s",
		15 * time.Minute:    "30s",
		time.Hour:           "1m",
		24 * time.Hour:      "30m",
		30 * 24 * time.Hour: "12h",
		0:                   "",
	}
	for span, want := range cases {
		if got := bucketFor(span); got != want {
			t.Errorf("bucketFor(%s) = %q, want %q", span, got, want)
		}
	}
}

// Graylog only returns buckets with messages; the gaps are filled with zero
// so the bars are evenly spaced in time.
func TestFillBuckets(t *testing.T) {
	in := []Bucket{{Time: "2026-11-12T07:01:00.000Z", Count: 2}, {Time: "2026-11-12T07:04:00.000Z", Count: 5}}
	out := fillBuckets(in, "1m", "2026-11-12T07:00:30.000Z", "2026-11-12T07:05:30.000Z")
	want := []int64{0, 2, 0, 0, 5, 0}
	if len(out) != len(want) {
		t.Fatalf("got %d buckets: %+v", len(out), out)
	}
	for i, c := range want {
		if out[i].Count != c {
			t.Fatalf("bucket %d = %+v, want count %d (all %+v)", i, out[i], c, out)
		}
	}
	if out[0].Time != "2026-11-12T07:00:00.000Z" {
		t.Fatalf("first bucket = %s", out[0].Time)
	}
	// Daily buckets of a Graylog user in Istanbul (UTC+3) start at 21:00Z:
	// the grid follows them instead of UTC midnight.
	ist := []Bucket{{Time: "2026-11-09T21:00:00.000Z", Count: 4}, {Time: "2026-11-11T21:00:00.000Z", Count: 1}}
	got := fillBuckets(ist, "1d", "2026-11-09T08:00:00.000Z", "2026-11-12T08:00:00.000Z")
	if len(got) != 4 || got[0].Time != "2026-11-08T21:00:00.000Z" || got[1].Count != 4 || got[2].Count != 0 || got[3].Count != 1 {
		t.Fatalf("istanbul daily = %+v", got)
	}
	// Buckets that don't share one grid are kept as Graylog sent them.
	odd := []Bucket{{Time: "2026-11-12T07:01:17.000Z", Count: 1}, {Time: "2026-11-12T07:03:00.000Z", Count: 1}}
	if got := fillBuckets(odd, "1m", "2026-11-12T07:00:30.000Z", "2026-11-12T07:05:30.000Z"); len(got) != 2 {
		t.Fatalf("off-grid = %+v", got)
	}
	// Auto interval: unchanged.
	if got := fillBuckets(in, "", "2026-11-12T07:00:30.000Z", "2026-11-12T07:05:30.000Z"); len(got) != 2 {
		t.Fatalf("auto = %+v", got)
	}
}

func TestSearchValidate(t *testing.T) {
	s := SearchRequest{Streams: []string{" a ", "", "b"}, Limit: 5000, Sort: "x"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Query != "*" || s.Limit != MaxLimit || s.Sort != "desc" || s.Range.Type != "relative" || s.Range.Range != 300 || len(s.Streams) != 2 || s.Streams[0] != "a" {
		t.Fatalf("defaults = %+v", s)
	}
	for _, bad := range []TimeRange{
		{Type: "absolute", From: "2026-11-12T08:00:00Z", To: "2026-11-12T07:00:00Z"},
		{Type: "absolute", From: "yesterday", To: "2026-11-12T07:00:00Z"},
		{Type: "keyword"},
		{Type: "relative", Range: -5},
		{Type: "sometime"},
	} {
		r := SearchRequest{Range: bad}
		if err := r.Validate(); err == nil {
			t.Errorf("range %+v should be rejected", bad)
		}
	}
}
