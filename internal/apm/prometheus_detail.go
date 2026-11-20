package apm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/servicemap"
)

// Service detail from Prometheus metrics: the requests a service receives,
// read from the first family that has them (OTel service graph, Istio,
// Linkerd, Hubble). Coroot's node-agent metrics count calls by peer IP, so
// they can't be narrowed to one service here (Coroot's own API can).

var _ ServiceDetailer = prometheusProvider{}

func (p prometheusProvider) ServiceDetail(ctx context.Context, c *Client, req DetailRequest) (*ServiceDetail, error) {
	q := promQuerier{c: c, prefix: "/api/v1"}
	return PromServiceDetail(ctx, q, q, req, "prometheus")
}

// promFam builds one family's expressions for a service.
type promFam struct {
	name string
	// requests: the request counter with the service's labels; failed: the
	// same for failed requests. bucket / sum / count: the latency histogram.
	requests, failed, bucket, sum, count string
	msScale                              float64 // 1000 when the histogram is in seconds
	// Optional breakdowns (instant, over the whole range): label to group by.
	endpointLabel, instanceLabel string
	endpointReq, endpointFailed  string // counters for the endpoint breakdown (if not requests/failed)
}

func promLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// sel builds equality matchers (label, value, …); every value is quoted, so
// a service name from the request can't add matchers. Empty values are
// skipped.
func sel(pairs ...string) string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			b = append(b, pairs[i]+"="+promLabel(pairs[i+1]))
		}
	}
	return strings.Join(b, ", ")
}

func promFamilies(svc, ns string) map[string]promFam {
	otel := sel("server", svc)
	span := sel("service_name", svc) + `, span_kind=~"SPAN_KIND_SERVER|server"`
	istio := sel("reporter", "destination", "destination_workload", svc, "destination_workload_namespace", ns)
	lk := sel("direction", "inbound", "deployment", svc, "namespace", ns)
	hub := sel("destination_workload", svc, "destination_namespace", ns)
	return map[string]promFam{
		FamOTel: {name: FamOTel,
			requests: "traces_service_graph_request_total{" + otel + "}", failed: "traces_service_graph_request_failed_total{" + otel + "}",
			bucket:  `{__name__=~"traces_service_graph_request_server(_seconds)?_bucket", ` + otel + "}",
			sum:     `{__name__=~"traces_service_graph_request_server(_seconds)?_sum", ` + otel + "}",
			count:   `{__name__=~"traces_service_graph_request_server(_seconds)?_count", ` + otel + "}",
			msScale: 1000, endpointLabel: "span_name",
			endpointReq:    `{__name__=~"traces_spanmetrics_calls_total|traces_span_metrics_calls_total|calls_total", ` + span + "}",
			endpointFailed: `{__name__=~"traces_spanmetrics_calls_total|traces_span_metrics_calls_total|calls_total", status_code="STATUS_CODE_ERROR", ` + span + "}",
		},
		FamIstio: {name: FamIstio,
			requests: "istio_requests_total{" + istio + "}", failed: "istio_requests_total{" + istio + `, response_code=~"5..|0"}`,
			bucket: "istio_request_duration_milliseconds_bucket{" + istio + "}", sum: "istio_request_duration_milliseconds_sum{" + istio + "}",
			count: "istio_request_duration_milliseconds_count{" + istio + "}", msScale: 1,
		},
		FamLinkerd: {name: FamLinkerd,
			requests: "response_total{" + lk + "}", failed: "response_total{" + lk + `, classification="failure"}`,
			bucket: "response_latency_ms_bucket{" + lk + "}", sum: "response_latency_ms_sum{" + lk + "}", count: "response_latency_ms_count{" + lk + "}",
			msScale: 1, instanceLabel: "pod",
		},
		FamHubble: {name: FamHubble,
			requests: "hubble_http_requests_total{" + hub + "}", failed: "hubble_http_requests_total{" + hub + `, status=~"5.."}`,
			bucket: "hubble_http_request_duration_seconds_bucket{" + hub + "}", sum: "hubble_http_request_duration_seconds_sum{" + hub + "}",
			count: "hubble_http_request_duration_seconds_count{" + hub + "}", msScale: 1000,
		},
	}
}

// PromServiceDetail reads one service's requests from Prometheus metrics.
func PromServiceDetail(ctx context.Context, iq servicemap.PromQuerier, rq servicemap.PromRangeQuerier, req DetailRequest, provider string) (*ServiceDetail, error) {
	svc := strings.TrimSpace(req.Ref.Name)
	if svc == "" {
		return nil, &APIError{400, "unsupported", "service is required"}
	}
	end := req.End
	if end.IsZero() {
		end = time.Now()
	}
	start := end.Add(-req.Range)
	step := req.Step()
	d := &ServiceDetail{Provider: provider, Service: svc, RangeSec: int64(req.Range.Seconds()), StepSec: int64(step.Seconds()), Charts: []DetailChart{}}

	fams, err := DetectFamilies(ctx, iq, end)
	if err != nil {
		return nil, err
	}
	w := step * 2
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	win := strconv.Itoa(int(w.Seconds())) + "s"
	whole := strconv.Itoa(int(req.Range.Seconds())) + "s"

	// The first family that sees requests to this service.
	var fam promFam
	var reqSeries []servicemap.RangeSample
	for _, name := range []string{FamOTel, FamIstio, FamLinkerd, FamHubble} {
		if !fams[name] {
			continue
		}
		f := promFamilies(svc, req.Ref.Namespace)[name]
		rows, err := rq.QueryRange(ctx, fmt.Sprintf("sum(rate(%s[%s]))", f.requests, win), start, end, step)
		if err != nil {
			d.Notes = append(d.Notes, fmt.Sprintf("family_error:%s:%s", name, trimErr(err)))
			continue
		}
		if len(rows) > 0 && len(rows[0].Points) > 0 {
			fam, reqSeries = f, rows
			break
		}
	}
	if fam.name == "" {
		if fams[FamCoroot] {
			d.Notes = append(d.Notes, "coroot_agent_metrics_only")
		}
		d.Notes = append(d.Notes, "no_service_requests")
		return d, nil
	}
	d.Notes = append(d.Notes, "family:"+fam.name)

	p := &panels{d: d}
	series := func(name string, rows []servicemap.RangeSample, scale float64) DetailSeries {
		s := DetailSeries{Name: name}
		if len(rows) > 0 {
			for _, pt := range rows[0].Points {
				s.Points = append(s.Points, [2]float64{pt[0], pt[1] * scale})
			}
		}
		return s
	}
	instant := func(expr string) *float64 {
		rows, err := iq.Query(ctx, expr, end)
		if err != nil || len(rows) == 0 {
			return nil
		}
		return ptr(rows[0].Value)
	}

	p.run("charts", func() error {
		failed, err := rq.QueryRange(ctx, fmt.Sprintf("sum(rate(%s[%s])) / sum(rate(%s[%s]))", fam.failed, win, fam.requests, win), start, end, step)
		if err != nil {
			return err
		}
		lat := DetailChart{ID: "latency", Unit: UnitMs}
		if rows, err := rq.QueryRange(ctx, fmt.Sprintf("sum(rate(%s[%s])) / sum(rate(%s[%s]))", fam.sum, win, fam.count, win), start, end, step); err == nil {
			lat.Series = append(lat.Series, series("avg", rows, fam.msScale))
		}
		for _, qn := range []string{"0.95", "0.99"} {
			if rows, err := rq.QueryRange(ctx, fmt.Sprintf("histogram_quantile(%s, sum by (le) (rate(%s[%s])))", qn, fam.bucket, win), start, end, step); err == nil {
				lat.Series = append(lat.Series, series("p"+strings.TrimPrefix(qn, "0.")[:2], rows, fam.msScale))
			}
		}
		p.with(func(d *ServiceDetail) {
			d.Charts = append(d.Charts,
				DetailChart{ID: "throughput", Unit: UnitRPS, Series: []DetailSeries{series("requests", reqSeries, 1)}},
				lat,
				DetailChart{ID: "errors", Unit: UnitRatio, Series: []DetailSeries{series("errors", failed, 1)}})
		})
		return nil
	})

	p.run("summary", func() error {
		s := &DetailSummary{
			RPS:       instant(fmt.Sprintf("sum(rate(%s[%s]))", fam.requests, whole)),
			ErrorRate: instant(fmt.Sprintf("sum(rate(%s[%s])) / sum(rate(%s[%s]))", fam.failed, whole, fam.requests, whole)),
			AvgMs:     scaled(instant(fmt.Sprintf("sum(rate(%s[%s])) / sum(rate(%s[%s]))", fam.sum, whole, fam.count, whole)), fam.msScale),
			P95Ms:     scaled(instant(fmt.Sprintf("histogram_quantile(0.95, sum by (le) (rate(%s[%s])))", fam.bucket, whole)), fam.msScale),
			P99Ms:     scaled(instant(fmt.Sprintf("histogram_quantile(0.99, sum by (le) (rate(%s[%s])))", fam.bucket, whole)), fam.msScale),
		}
		if s.RPS != nil && *s.RPS > 0 && s.ErrorRate == nil {
			s.ErrorRate = ptr(0) // no failed series = no failures
		}
		p.with(func(d *ServiceDetail) { d.Summary = s })
		return nil
	})

	if fam.endpointLabel != "" {
		p.run("endpoints", func() error {
			rows, err := iq.Query(ctx, fmt.Sprintf("topk(30, sum by (%s) (rate(%s[%s])))", fam.endpointLabel, fam.endpointReq, whole), end)
			if err != nil {
				return err
			}
			failed, _ := iq.Query(ctx, fmt.Sprintf("sum by (%s) (rate(%s[%s]))", fam.endpointLabel, fam.endpointFailed, whole), end)
			fr := map[string]float64{}
			for _, r := range failed {
				fr[r.Metric[fam.endpointLabel]] = r.Value
			}
			out := make([]DetailEndpoint, 0, len(rows))
			for _, r := range rows {
				e := DetailEndpoint{Name: r.Metric[fam.endpointLabel], RPS: ptr(r.Value)}
				if r.Value > 0 {
					e.ErrorRate = ptr(fr[e.Name] / r.Value)
				}
				out = append(out, e)
			}
			sort.SliceStable(out, func(i, j int) bool { return val(out[i].RPS) > val(out[j].RPS) })
			p.with(func(d *ServiceDetail) { d.Endpoints = out })
			return nil
		})
	}
	if fam.instanceLabel != "" {
		p.run("instances", func() error {
			rows, err := iq.Query(ctx, fmt.Sprintf("sum by (%s) (rate(%s[%s]))", fam.instanceLabel, fam.requests, whole), end)
			if err != nil {
				return err
			}
			failed, _ := iq.Query(ctx, fmt.Sprintf("sum by (%s) (rate(%s[%s]))", fam.instanceLabel, fam.failed, whole), end)
			fr := map[string]float64{}
			for _, r := range failed {
				fr[r.Metric[fam.instanceLabel]] = r.Value
			}
			out := make([]DetailInstance, 0, len(rows))
			for _, r := range rows {
				in := DetailInstance{Name: r.Metric[fam.instanceLabel], RPS: ptr(r.Value)}
				if r.Value > 0 {
					in.ErrorRate = ptr(fr[in.Name] / r.Value)
				}
				out = append(out, in)
			}
			sort.SliceStable(out, func(i, j int) bool { return val(out[i].RPS) > val(out[j].RPS) })
			p.with(func(d *ServiceDetail) { d.Instances = out })
			return nil
		})
	}
	p.wait()
	sortCharts(d.Charts)
	return d, nil
}

func scaled(f *float64, k float64) *float64 {
	if f == nil || math.IsNaN(*f) {
		return nil
	}
	return ptr(*f * k)
}
