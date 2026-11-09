package cmd

import "github.com/spf13/cobra"

func New() *cobra.Command {
	c := &cobra.Command{Use: "granite", Short: "Observe and optimize CPU contention in AI inference", SilenceUsage: true, SilenceErrors: true}
	c.AddCommand(newDemo(),
		agentCmd,
		controllerCmd,
	)
	return c
}
