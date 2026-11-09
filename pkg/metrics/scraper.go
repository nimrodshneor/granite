// Package metrics adapts Prometheus histograms to interval observations.
package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/nimrodshn/granite/pkg/model"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"
)

type histogram struct {
	count   uint64
	buckets map[float64]uint64
}

type Scraper struct {
	VLLMURL, GPUURL string
	Client          *http.Client
	previous        map[string]histogram
	completed       float64
	at              time.Time
}

func fetch(ctx context.Context, client *http.Client, url string) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: HTTP %d", url, resp.StatusCode)
	}
	p := expfmt.NewTextParser(prommodel.UTF8Validation)
	return p.TextToMetricFamilies(resp.Body)
}

func readHistogram(f *dto.MetricFamily) (histogram, error) {
	h := histogram{buckets: map[float64]uint64{}}
	if f == nil || f.GetType() != dto.MetricType_HISTOGRAM {
		return h, fmt.Errorf("missing histogram")
	}
	for _, m := range f.Metric {
		h.count += m.GetHistogram().GetSampleCount()
		for _, b := range m.GetHistogram().Bucket {
			h.buckets[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}
	return h, nil
}

func interval(current, prev histogram) (histogram, error) {
	h := histogram{buckets: map[float64]uint64{}}
	if current.count < prev.count || len(current.buckets) != len(prev.buckets) {
		return h, fmt.Errorf("histogram reset or schema change")
	}
	h.count = current.count - prev.count
	for bound, count := range current.buckets {
		old, ok := prev.buckets[bound]
		if !ok || count < old {
			return h, fmt.Errorf("histogram reset or schema change")
		}
		h.buckets[bound] = count - old
	}
	return h, nil
}

func quantile(h histogram, q float64) (float64, error) {
	if h.count == 0 {
		return 0, fmt.Errorf("no requests in interval")
	}
	bounds := make([]float64, 0, len(h.buckets))
	for b := range h.buckets {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)
	target, lower, previous := float64(h.count)*q, 0.0, uint64(0)
	for _, upper := range bounds {
		count := h.buckets[upper]
		if float64(count) >= target {
			if math.IsInf(upper, 1) {
				return 0, fmt.Errorf("quantile exceeds finite buckets")
			}
			if count == previous {
				return upper, nil
			}
			return lower + (upper-lower)*(target-float64(previous))/float64(count-previous), nil
		}
		lower, previous = upper, count
	}
	return 0, fmt.Errorf("quantile exceeds histogram")
}

// Sample requires a dedicated single-workload vLLM endpoint and a GPU endpoint
// exposing exactly one DCGM_FI_DEV_GPU_UTIL series for that workload's GPU.
func (s *Scraper) Sample(ctx context.Context, now time.Time) (model.Observation, error) {
	o := model.Observation{At: now}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	app, err := fetch(ctx, client, s.VLLMURL)
	if err != nil {
		s.previous = nil
		return o, err
	}
	names := []string{"vllm:time_to_first_token_seconds", "vllm:inter_token_latency_seconds"}
	// Earlier vLLM releases exported the same token-level signal under this name.
	if app[names[1]] == nil {
		names[1] = "vllm:time_per_output_token_seconds"
	}
	current := map[string]histogram{}
	for _, name := range names {
		h, err := readHistogram(app[name])
		if err != nil {
			s.previous = nil
			return o, fmt.Errorf("%s: %w", name, err)
		}
		current[name] = h
	}
	fCompleted := app["vllm:request_success_total"]
	if fCompleted == nil {
		fCompleted = app["vllm:request_success"]
	}
	if fCompleted == nil || fCompleted.GetType() != dto.MetricType_COUNTER || len(fCompleted.Metric) == 0 {
		s.previous = nil
		return o, fmt.Errorf("missing vllm:request_success_total counter")
	}
	completed := 0.0
	for _, m := range fCompleted.Metric {
		completed += m.GetCounter().GetValue()
	}
	if math.IsNaN(completed) || math.IsInf(completed, 0) || completed < 0 {
		s.previous = nil
		return o, fmt.Errorf("invalid completed request counter")
	}
	prev, at, prevCompleted := s.previous, s.at, s.completed
	s.previous, s.at = current, now
	s.completed = completed
	if prev == nil || !now.After(at) {
		return o, fmt.Errorf("warming up histogram interval")
	}
	deltas := make([]histogram, 2)
	for i, name := range names {
		deltas[i], err = interval(current[name], prev[name])
		if err != nil {
			return o, err
		}
	}
	o.TTFTP95Seconds, err = quantile(deltas[0], .95)
	if err != nil {
		return o, err
	}
	o.ITLP95Seconds, err = quantile(deltas[1], .95)
	if err != nil {
		return o, err
	}
	if completed < prevCompleted {
		return o, fmt.Errorf("completed request counter reset")
	}
	o.RequestsPerSecond = (completed - prevCompleted) / now.Sub(at).Seconds()
	gpu, err := fetch(ctx, client, s.GPUURL)
	if err != nil {
		return o, err
	}
	f := gpu["DCGM_FI_DEV_GPU_UTIL"]
	if f == nil || f.GetType() != dto.MetricType_GAUGE || len(f.Metric) != 1 {
		return o, fmt.Errorf("GPU endpoint must expose exactly one DCGM_FI_DEV_GPU_UTIL gauge")
	}
	o.GPUUtilization = f.Metric[0].GetGauge().GetValue() / 100
	if math.IsNaN(o.GPUUtilization) || math.IsInf(o.GPUUtilization, 0) || o.GPUUtilization < 0 || o.GPUUtilization > 1 {
		o.GPUUtilization = 0
		return o, fmt.Errorf("invalid GPU utilization")
	}
	o.Valid = true
	return o, nil
}
