package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nimrodshn/granite/pkg/enforce"
	"github.com/nimrodshn/granite/pkg/metrics"
	"github.com/nimrodshn/granite/pkg/model"
	"github.com/nimrodshn/granite/pkg/policy"
	"github.com/nimrodshn/granite/pkg/telemetry"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the node-local telemetry and policy loop (observe by default)",
	Run:   runAgent,
}

type report struct {
	Observation model.Observation `json:"observation"`
	Decision    model.Decision    `json:"decision"`
	Enforcement bool              `json:"enforcement_enabled"`
	Error       string            `json:"error,omitempty"`
}

type agentArgs struct {
	pid                                    uint32
	object, appURL, gpuURL, cgroup, listen string
	enable                                 bool
	interval                               time.Duration
}

var agentCmdLineArgs agentArgs

func init() {
	f := agentCmd.Flags()
	f.Uint32Var(&agentCmdLineArgs.pid, "pid", 0, "Target inference process PID")
	f.StringVar(&agentCmdLineArgs.object, "object", "/sys/fs/cgroup/granite", "Cgroup v2 object path for this agent")
	f.StringVar(&agentCmdLineArgs.appURL, "app-url", "http://localhost:8080/metrics", "Inference engine metrics URL")
	f.StringVar(&agentCmdLineArgs.gpuURL, "gpu-url", "http://localhost:8000/metrics", "GPU metrics URL")
	f.StringVar(&agentCmdLineArgs.cgroup, "cgroup", "", "Target inference process cgroup (absolute path)")
	f.StringVar(&agentCmdLineArgs.listen, "listen", ":8081", "Agent HTTP listen address")
	f.BoolVar(&agentCmdLineArgs.enable, "enforce", false, "Enable CPU weight enforcement (requires --cgroup)")
	f.DurationVar(&agentCmdLineArgs.interval, "interval", 5*time.Second, "Telemetry and policy loop interval")
}

func runAgent(cmd *cobra.Command, _ []string) {
	cfg := policy.DefaultConfig()
	if agentCmdLineArgs.pid == 0 || agentCmdLineArgs.interval < time.Second || cfg.TTFTSLO <= 0 || cfg.ITLSLO <= 0 {
		log.Fatalf("positive --pid and SLOs, and --interval >= 1s are required")
	}
	if agentCmdLineArgs.enable && (agentCmdLineArgs.cgroup == "" || !filepath.IsAbs(agentCmdLineArgs.cgroup)) {
		log.Fatalf("--enforce requires an absolute --cgroup")
	}
	// Identity is fixed for this agent instance; a restarted process needs a new agent.
	identity, err := processIdentity(agentCmdLineArgs.pid, agentCmdLineArgs.cgroup, agentCmdLineArgs.enable)
	if err != nil {
		log.Fatalf("failed to get process identity: %v", err)
	}
	scheduler, err := telemetry.Open(agentCmdLineArgs.object, agentCmdLineArgs.pid)
	if err != nil {
		log.Fatalf("failed to open telemetry: %v", err)
	}
	defer scheduler.Close()
	weight := &enforce.Weight{Path: filepath.Join(agentCmdLineArgs.cgroup, "cpu.weight")}
	var mu sync.RWMutex
	latest := report{}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(latest)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		if !latest.Observation.Valid {
			http.Error(w, "telemetry unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		o := latest.Observation
		valid := 0
		if o.Valid {
			valid = 1
		}
		_, _ = fmt.Fprintf(w, "granite_telemetry_valid %d\ngranite_ttft_p95_seconds %g\ngranite_itl_p95_seconds %g\ngranite_runnable_delay_mean_seconds %g\ngranite_scheduling_samples %d\ngranite_gpu_utilization_ratio %g\ngranite_requests_per_second %g\n", valid, o.TTFTP95Seconds, o.ITLP95Seconds, o.RunnableDelaySeconds, o.Samples, o.GPUUtilization, o.RequestsPerSecond)
	})
	ln, err := net.Listen("tcp", agentCmdLineArgs.listen)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ln) }()
	defer server.Close()
	engine := policy.New(cfg)
	scraper := &metrics.Scraper{VLLMURL: agentCmdLineArgs.appURL, GPUURL: agentCmdLineArgs.gpuURL}
	ticker := time.NewTicker(agentCmdLineArgs.interval)
	defer ticker.Stop()
	for {
		select {
		case <-cmd.Context().Done():
			os.Exit(0)
		case err := <-serveErr:
			log.Fatalf("server error: %v", err)
		case now := <-ticker.C:
			current, err := processIdentity(agentCmdLineArgs.pid, agentCmdLineArgs.cgroup, agentCmdLineArgs.enable)
			if err != nil || current != identity {
				log.Fatalf("target process disappeared or identity/cgroup changed: %v", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), min(agentCmdLineArgs.interval, 5*time.Second))
			o, scrapeErr := scraper.Sample(ctx, now)
			cancel()
			delay, count, kernelErr := scheduler.Sample()
			o.RunnableDelaySeconds, o.Samples = delay, count
			o.Valid = o.Valid && kernelErr == nil
			d := engine.Step(o)
			r := report{Observation: o, Decision: d, Enforcement: agentCmdLineArgs.enable}
			if err := errors.Join(scrapeErr, kernelErr); err != nil {
				r.Error = err.Error()
			}
			if agentCmdLineArgs.enable && d.Action == "apply" {
				if err := weight.Apply(); err != nil {
					log.Fatalf("apply cpu.weight: %w", err)
				}
			}
			if d.Action == "rollback" {
				if err := weight.Rollback(); err != nil {
					log.Fatalf("restore cpu.weight: %w", err)
				}
			}
			mu.Lock()
			latest = r
			mu.Unlock()
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(r); err != nil {
				log.Fatalf("failed to write report: %v", err)
			}
		}
	}

}

func processIdentity(pid uint32, cgroup string, enforce bool) (string, error) {
	root := "/proc/" + strconv.FormatUint(uint64(pid), 10)
	b, err := os.ReadFile(root + "/stat")
	if err != nil {
		return "", err
	}
	// comm can contain whitespace and parentheses; fields after its last ')' start at field 3.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete process stat")
	}
	membership, err := os.ReadFile(root + "/cgroup")
	if err != nil {
		return "", err
	}
	if enforce {
		found := false
		for _, line := range strings.Split(string(membership), "\n") {
			if strings.HasPrefix(line, "0::/") {
				expected := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
				if filepath.Clean(cgroup) != expected || expected == "/sys/fs/cgroup" {
					return "", fmt.Errorf("--cgroup must match the target's non-root host cgroup: %s", expected)
				}
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("cgroup v2 membership required")
		}
	}
	return fields[19] + string(membership), nil
}
