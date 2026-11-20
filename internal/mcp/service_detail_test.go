package mcp

import (
	"strings"
	"testing"
)

func TestDropDetailCharts(t *testing.T) {
	body := `{"data":{"service":"api","summary":{"rps":1},"charts":[{"id":"throughput","series":[{"name":"requests","points":[[1,2]]}]},{"id":"other","title":"CPU usage","series":[]}]}}`
	got := dropDetailCharts(body)
	if strings.Contains(got, `"points"`) || !strings.Contains(got, "throughput, CPU usage") || !strings.Contains(got, `"summary"`) {
		t.Fatalf("got %s", got)
	}
	if dropDetailCharts("not json") != "not json" {
		t.Fatal("non-JSON must pass through")
	}
}
