package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Exercise our actual registry through the exposition boundary, not only DTO
// getters. Both text escaping and delimited protobuf must retain label values,
// histogram counts/sums and fresh gauge callbacks on successive scrapes.
func TestMetricsExpositionRoundTrip(t *testing.T) {
	for _, format := range []struct{ name, accept string }{
		{"text", "text/plain; version=0.0.4"},
		{"protobuf", "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited"},
	} {
		t.Run(format.name, func(t *testing.T) {
			busy := 2.0
			m := New(Sources{TabsBusy: func() float64 { return busy }})
			// Synthetic hostile label text must remain one value, never extra
			// metric lines; no real request URLs or private hosts are needed.
			host := "測試.example\"\\\nforged_metric 42"
			m.FetchTotal.WithLabelValues(OutcomeOK, host).Add(3)
			m.FetchDuration.WithLabelValues(OutcomeOK).Observe(0.125)
			m.FetchDuration.WithLabelValues(OutcomeOK).Observe(1.5)
			handler := promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
			scrape := func() map[string]*dto.MetricFamily {
				t.Helper()
				req := httptest.NewRequest("GET", "/metrics", nil)
				req.Header.Set("Accept", format.accept)
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != 200 {
					t.Fatalf("scrape: %d %s", res.Code, res.Body.String())
				}
				response := res.Result()
				defer response.Body.Close()
				if !strings.Contains(response.Header.Get("Content-Type"), strings.Split(format.accept, ";")[0]) {
					t.Fatalf("wrong negotiated format: %s", response.Header.Get("Content-Type"))
				}
				decoder := expfmt.NewDecoder(response.Body, expfmt.ResponseFormat(response.Header))
				families := map[string]*dto.MetricFamily{}
				for {
					family := new(dto.MetricFamily)
					err := decoder.Decode(family)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if families[family.GetName()] != nil {
						t.Fatalf("duplicate family %q", family.GetName())
					}
					families[family.GetName()] = family
				}
				return families
			}
			first := scrape()
			counter := first["browser_fetch_fetch_total"]
			if counter.GetType() != dto.MetricType_COUNTER || len(counter.GetMetric()) != 1 || counter.Metric[0].GetCounter().GetValue() != 3 {
				t.Fatalf("counter: %v", counter)
			}
			labels := map[string]string{}
			for _, label := range counter.Metric[0].GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if len(labels) != 2 || labels["host"] != host || labels["outcome"] != OutcomeOK || first["forged_metric"] != nil {
				t.Fatalf("labels changed/injected: %#v", labels)
			}
			histFamily := first["browser_fetch_fetch_duration_seconds"]
			if histFamily.GetType() != dto.MetricType_HISTOGRAM || len(histFamily.GetMetric()) != 1 {
				t.Fatalf("histogram: %v", histFamily)
			}
			hist := histFamily.Metric[0].GetHistogram()
			if hist.GetSampleCount() != 2 || hist.GetSampleSum() != 1.625 {
				t.Fatalf("lost histogram samples: %v", hist)
			}
			buckets := map[float64]uint64{}
			for _, bucket := range hist.GetBucket() {
				buckets[bucket.GetUpperBound()] = bucket.GetCumulativeCount()
			}
			if buckets[0.25] != 1 || buckets[1] != 1 || buckets[2] != 2 || buckets[60] != 2 {
				t.Fatalf("wrong cumulative buckets: %v", buckets)
			}
			gauge := first["browser_fetch_tabs_busy"]
			if len(gauge.GetMetric()) != 1 || gauge.Metric[0].GetGauge().GetValue() != 2 {
				t.Fatalf("gauge: %v", gauge)
			}
			busy = 7
			second := scrape()["browser_fetch_tabs_busy"]
			if len(second.GetMetric()) != 1 || second.Metric[0].GetGauge().GetValue() != 7 || gauge.Metric[0].GetGauge().GetValue() != 2 {
				t.Fatalf("stale gauge or mutated prior scrape: first=%v second=%v", gauge, second)
			}
		})
	}
}
