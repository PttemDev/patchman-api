package handlers

import (
	"net/url"
	"testing"
	"time"
)

func TestK8sLogQuery_Defaults(t *testing.T) {
	o, err := logOptionsFromQuery(url.Values{"pod": {"api-1"}, "namespace": {"prod"}, "follow": {"1"}, "timestamps": {"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Pod != "api-1" || o.Namespace != "prod" || !o.Follow || !o.Timestamps || o.Previous {
		t.Fatalf("unexpected %+v", o)
	}
	if o.TailLines != 500 || o.LimitBytes != 0 || o.SinceTime != nil {
		t.Fatalf("defaults: tail=%d limit=%d since=%v", o.TailLines, o.LimitBytes, o.SinceTime)
	}
}

func TestK8sLogQuery_TailAllIsCapped(t *testing.T) {
	o, err := logOptionsFromQuery(url.Values{"pod": {"p"}, "tailLines": {"all"}})
	if err != nil {
		t.Fatal(err)
	}
	if o.TailLines != 0 || o.LimitBytes != fullLogLimitBytes {
		t.Fatalf("tail=all → tail=%d limit=%d", o.TailLines, o.LimitBytes)
	}
}

// sinceTime (the FE's resume cursor after a dropped follow stream) keeps its
// nanoseconds here; StreamLogs truncates it to the API's whole seconds.
func TestK8sLogQuery_SinceTime(t *testing.T) {
	o, err := logOptionsFromQuery(url.Values{"pod": {"p"}, "tailLines": {"5000"}, "sinceTime": {"2026-11-11T10:00:01.123456789Z"}})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 11, 10, 0, 1, 123456789, time.UTC)
	if o.SinceTime == nil || !o.SinceTime.Equal(want) || o.TailLines != 5000 {
		t.Fatalf("since=%v tail=%d", o.SinceTime, o.TailLines)
	}
	if _, err := logOptionsFromQuery(url.Values{"pod": {"p"}, "sinceTime": {"yesterday"}}); err == nil {
		t.Fatal("bad sinceTime accepted")
	}
}

func TestK8sLogQuery_PodRequired(t *testing.T) {
	if _, err := logOptionsFromQuery(url.Values{"pod": {"  "}}); err == nil {
		t.Fatal("empty pod accepted")
	}
}
