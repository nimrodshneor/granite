package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIntervalHistogramsAndMissingGPU(t *testing.T) {
	count, gpu := 100, true
	itl := "vllm:inter_token_latency_seconds"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# TYPE vllm:request_success_total counter\nvllm:request_success_total %d\n", count)
		for _, name := range []string{"vllm:time_to_first_token_seconds", itl} {
			fmt.Fprintf(w, "# TYPE %s histogram\n%s_bucket{le=\"0.1\"} %d\n%s_bucket{le=\"1\"} %d\n%s_bucket{le=\"+Inf\"} %d\n%s_count %d\n%s_sum 1\n", name, name, count/2, name, count, name, count, name, count, name)
		}
		if gpu {
			fmt.Fprintln(w, "# TYPE DCGM_FI_DEV_GPU_UTIL gauge\nDCGM_FI_DEV_GPU_UTIL 40")
		}
	}))
	defer server.Close()
	s := &Scraper{VLLMURL: server.URL, GPUURL: server.URL}
	now := time.Now()
	if _, err := s.Sample(context.Background(), now); err == nil {
		t.Fatal("first scrape must warm up")
	}
	count += 20
	o, err := s.Sample(context.Background(), now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !o.Valid || o.RequestsPerSecond != 4 || o.GPUUtilization != .4 || o.TTFTP95Seconds < .9 {
		t.Fatalf("bad observation: %+v", o)
	}
	count = 10
	if _, err := s.Sample(context.Background(), now.Add(10*time.Second)); err == nil {
		t.Fatal("counter reset must invalidate interval")
	}
	count += 20
	gpu = false
	if o, err := s.Sample(context.Background(), now.Add(15*time.Second)); err == nil || o.Valid {
		t.Fatal("missing GPU must not be treated as headroom")
	}
	gpu, itl = true, "vllm:time_per_output_token_seconds"
	count += 20
	if _, err := s.Sample(context.Background(), now.Add(20*time.Second)); err == nil {
		t.Fatal("metric alias change requires a new baseline")
	}
	count += 20
	if o, err := s.Sample(context.Background(), now.Add(25*time.Second)); err != nil || !o.Valid {
		t.Fatalf("legacy metric: %+v, %v", o, err)
	}
}
