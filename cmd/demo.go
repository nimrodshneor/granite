package cmd

import (
	"encoding/json"
	"time"

	"github.com/nimrodshn/granite/pkg/model"
	"github.com/nimrodshn/granite/pkg/policy"
	"github.com/spf13/cobra"
)

func newDemo() *cobra.Command {
	return &cobra.Command{Use: "demo", Short: "Run a synthetic policy trial without host mutations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			engine := policy.New(policy.DefaultConfig())
			start := time.Now()
			for i := 0; i < 14; i++ {
				o := model.Observation{At: start.Add(time.Duration(i) * 5 * time.Second), TTFTP95Seconds: .8, ITLP95Seconds: .08,
					RequestsPerSecond: 20, GPUUtilization: .5, RunnableDelaySeconds: .012, Samples: 100, Valid: true}
				if i >= 3 && i < 9 {
					o.TTFTP95Seconds, o.ITLP95Seconds, o.RunnableDelaySeconds = .4, .04, .002
				}
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Synthetic   bool              `json:"synthetic"`
					Observation model.Observation `json:"observation"`
					Decision    model.Decision    `json:"decision"`
				}{true, o, engine.Step(o)}); err != nil {
					return err
				}
			}
			return nil
		}}
}
