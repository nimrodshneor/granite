package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/nimrodshn/granite/pkg/enforce"
	"github.com/nimrodshn/granite/pkg/metrics"
	"github.com/nimrodshn/granite/pkg/model"
	"github.com/nimrodshn/granite/pkg/policy"
	"github.com/nimrodshn/granite/pkg/telemetry"
	"github.com/spf13/cobra"
)

const (
	defaultAgentObject     = "/sys/fs/cgroup/granite"
	defaultAppMetricsURL   = "http://localhost:8080/metrics"
	defaultGPUMetricsURL   = "http://localhost:8000/metrics"
	defaultAgentListen     = ":8081"
	defaultAgentInterval   = 5 * time.Second
	minAgentInterval       = time.Second
	agentScrapeTimeout     = 5 * time.Second
	agentReadHeaderTimeout = 3 * time.Second
	cpuWeightFile          = "cpu.weight"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the node-local telemetry and policy loop (observe by default)",
	RunE:  runAgent,
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
	f.StringVar(&agentCmdLineArgs.object, "object", defaultAgentObject, "Cgroup v2 object path for this agent")
	f.StringVar(&agentCmdLineArgs.appURL, "app-url", defaultAppMetricsURL, "Inference engine metrics URL")
	f.StringVar(&agentCmdLineArgs.gpuURL, "gpu-url", defaultGPUMetricsURL, "GPU metrics URL")
	f.StringVar(&agentCmdLineArgs.cgroup, "cgroup", "", "Target inference process cgroup (absolute path)")
	f.StringVar(&agentCmdLineArgs.listen, "listen", defaultAgentListen, "Agent HTTP listen address")
	f.BoolVar(&agentCmdLineArgs.enable, "enforce", false, "Enable CPU weight enforcement (requires --cgroup)")
	f.DurationVar(&agentCmdLineArgs.interval, "interval", defaultAgentInterval, "Telemetry and policy loop interval")
}

func runAgent(cmd *cobra.Command, _ []string) (retErr error) {
	if cmd.Context().Err() != nil {
		return nil
	}
	agent, err := newRuntimeAgent(agentCmdLineArgs)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, agent.close()) }()

	server, serveErr, err := startAgentServer(agent.args.listen, &agent.reports)
	if err != nil {
		return err
	}
	defer server.Close()
	return agent.run(cmd.Context(), serveErr, cmd.OutOrStdout())
}

// runtimeAgent owns node-local resources and policy state for one target process.
// Its policy loop is sequential; only the HTTP report store is shared.
type runtimeAgent struct {
	args      agentArgs
	identity  string
	scheduler *telemetry.Scheduler
	scraper   *metrics.Scraper
	engine    *policy.Engine
	weight    *enforce.Weight
	reports   reportStore
}

func validateAgentArgs(args agentArgs, cfg policy.Config) error {
	if args.pid == 0 || args.interval < minAgentInterval || cfg.TTFTSLO <= 0 || cfg.ITLSLO <= 0 {
		return fmt.Errorf("positive --pid and SLOs, and --interval >= 1s are required")
	}
	if args.enable && (args.cgroup == "" || !filepath.IsAbs(args.cgroup)) {
		return fmt.Errorf("--enforce requires an absolute --cgroup")
	}
	return nil
}

func newRuntimeAgent(args agentArgs) (*runtimeAgent, error) {
	cfg := policy.DefaultConfig()
	if err := validateAgentArgs(args, cfg); err != nil {
		return nil, err
	}
	identity, err := processIdentity(args.pid, args.cgroup, args.enable)
	if err != nil {
		return nil, fmt.Errorf("failed to get process identity: %w", err)
	}
	scheduler, err := telemetry.Open(args.object, args.pid)
	if err != nil {
		return nil, fmt.Errorf("failed to open telemetry: %w", err)
	}
	return &runtimeAgent{
		args: args, identity: identity, scheduler: scheduler,
		scraper: &metrics.Scraper{VLLMURL: args.appURL, GPUURL: args.gpuURL},
		engine:  policy.New(cfg),
		weight:  &enforce.Weight{Path: filepath.Join(args.cgroup, cpuWeightFile)},
	}, nil
}

// close restores the intervention before detaching telemetry. Rollback errors
// remain visible to RunE even when another error caused the loop to stop.
func (a *runtimeAgent) close() error {
	defer a.scheduler.Close()
	if err := a.weight.Rollback(); err != nil {
		return fmt.Errorf("restore cpu.weight during cleanup: %w", err)
	}
	return nil
}

func (a *runtimeAgent) run(ctx context.Context, serveErr <-chan error, output io.Writer) error {
	ticker := time.NewTicker(a.args.interval)
	defer ticker.Stop()
	encoder := json.NewEncoder(output)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("server error: %w", err)
		case now := <-ticker.C:
			if err := a.step(ctx, now, encoder); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return nil
				}
				return err
			}
		}
	}
}

func (a *runtimeAgent) step(ctx context.Context, now time.Time, encoder *json.Encoder) error {
	if err := a.checkIdentity(); err != nil {
		return err
	}
	observation, collectionErr := a.observe(ctx, now)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	decision := a.engine.Step(observation)
	if err := a.enforceDecision(decision); err != nil {
		return err
	}
	r := report{Observation: observation, Decision: decision, Enforcement: a.args.enable}
	if collectionErr != nil {
		r.Error = collectionErr.Error()
	}
	a.reports.publish(r)
	if err := encoder.Encode(r); err != nil {
		return fmt.Errorf("failed to write report: %w", err)
	}
	return nil
}

func (a *runtimeAgent) checkIdentity() error {
	current, err := processIdentity(a.args.pid, a.args.cgroup, a.args.enable)
	if err != nil {
		return fmt.Errorf("check target process identity: %w", err)
	}
	if current != a.identity {
		return fmt.Errorf("target process identity/cgroup changed")
	}
	return nil
}

// Collection failures invalidate observations so the policy can end a trial;
// they do not terminate the agent, which can recover on a later interval.
func (a *runtimeAgent) observe(ctx context.Context, now time.Time) (model.Observation, error) {
	scrapeCtx, cancel := context.WithTimeout(ctx, min(a.args.interval, agentScrapeTimeout))
	defer cancel()
	o, scrapeErr := a.scraper.Sample(scrapeCtx, now)
	if ctx.Err() != nil {
		return o, ctx.Err()
	}
	delay, count, kernelErr := a.scheduler.Sample()
	o.RunnableDelaySeconds, o.Samples = delay, count
	o.Valid = o.Valid && kernelErr == nil
	return o, errors.Join(scrapeErr, kernelErr)
}

func (a *runtimeAgent) enforceDecision(d model.Decision) error {
	switch d.Action {
	case "apply":
		if a.args.enable {
			if err := a.weight.Apply(); err != nil {
				return fmt.Errorf("apply cpu.weight: %w", err)
			}
		}
	case "rollback":
		if err := a.weight.Rollback(); err != nil {
			return fmt.Errorf("restore cpu.weight: %w", err)
		}
	}
	return nil
}
