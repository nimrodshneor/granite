package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
)

const (
	agentStatusPath       = "/status"
	agentHealthPath       = "/healthz"
	agentMetricsPath      = "/metrics"
	contentTypeHeader     = "Content-Type"
	jsonContentType       = "application/json"
	prometheusContentType = "text/plain; version=0.0.4"
)

// reportStore shares the latest complete report with concurrent HTTP handlers.
type reportStore struct {
	mu     sync.RWMutex
	latest report
}

func (s *reportStore) publish(r report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = r
}

func (s *reportStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(agentStatusPath, func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		w.Header().Set(contentTypeHeader, jsonContentType)
		_ = json.NewEncoder(w).Encode(s.latest)
	})
	mux.HandleFunc(agentHealthPath, func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if !s.latest.Observation.Valid {
			http.Error(w, "telemetry unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc(agentMetricsPath, func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		w.Header().Set(contentTypeHeader, prometheusContentType)
		o := s.latest.Observation
		valid := 0
		if o.Valid {
			valid = 1
		}
		_, _ = fmt.Fprintf(w, "granite_telemetry_valid %d\ngranite_ttft_p95_seconds %g\ngranite_itl_p95_seconds %g\ngranite_runnable_delay_mean_seconds %g\ngranite_scheduling_samples %d\ngranite_gpu_utilization_ratio %g\ngranite_requests_per_second %g\n", valid, o.TTFTP95Seconds, o.ITLP95Seconds, o.RunnableDelaySeconds, o.Samples, o.GPUUtilization, o.RequestsPerSecond)
	})

	return mux
}

func startAgentServer(address string, reports *reportStore) (*http.Server, <-chan error, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to listen: %w", err)
	}
	server := &http.Server{Handler: reports.handler(), ReadHeaderTimeout: agentReadHeaderTimeout}
	// Buffered so shutdown cannot strand the server goroutine sending its result.
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	return server, serveErr, nil
}
