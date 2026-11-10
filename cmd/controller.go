package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/nimrodshn/granite/pkg/discovery"
	"github.com/spf13/cobra"
)

var controllerCmd = &cobra.Command{
	Use:   "controller",
	RunE:  newController,
	Short: "Discover labeled inference Pods; emit read-only workload inventory",
}

type controllerArgs struct {
	client         discovery.Client
	selector, node string
	once           bool
	interval       time.Duration
}

var cmdLineArgs controllerArgs

const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"

func init() {
	f := controllerCmd.Flags()
	f.StringVar(&cmdLineArgs.client.URL, "api-url", "https://kubernetes.default.svc", "Kubernetes API URL")
	f.StringVar(&cmdLineArgs.client.TokenFile, "token-file", sa+"token", "Service account token file")
	f.StringVar(&cmdLineArgs.client.CAFile, "ca-file", sa+"ca.crt", "Kubernetes CA file")
	f.StringVar(&cmdLineArgs.selector, "selector", "granite.ai/engine=vllm", "Workload label selector")
	f.StringVar(&cmdLineArgs.node, "node", os.Getenv("NODE_NAME"), "Optional node name filter")
	f.BoolVar(&cmdLineArgs.once, "once", false, "Discover once and exit")
	f.DurationVar(&cmdLineArgs.interval, "interval", 30*time.Second, "Discovery interval")

}

func newController(cmd *cobra.Command, args []string) error {
	if cmd.Context().Err() != nil {
		return nil
	}
	if cmdLineArgs.interval < time.Second {
		return fmt.Errorf("--interval must be at least 1s")
	}
	for {
		workloads, err := cmdLineArgs.client.Discover(cmd.Context(),
			cmdLineArgs.selector,
			cmdLineArgs.node,
		)
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil
			}
			return fmt.Errorf("discover workloads: %w", err)
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(workloads); err != nil {
			return fmt.Errorf("write workload inventory: %w", err)
		}
		if cmdLineArgs.once {
			return nil
		}
		timer := time.NewTimer(cmdLineArgs.interval)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
