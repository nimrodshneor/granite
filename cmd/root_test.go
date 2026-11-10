package cmd

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCommandsReturnValidationErrors(t *testing.T) {
	originalAgent, originalController := agentCmdLineArgs, cmdLineArgs
	t.Cleanup(func() { agentCmdLineArgs, cmdLineArgs = originalAgent, originalController })
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"agent", []string{"agent", "--pid=0"}, "positive --pid"},
		{"controller", []string{"controller", "--interval=0s"}, "--interval must be at least 1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			c.SetArgs(tc.args)
			if err := c.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCommandsReturnNormallyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{"agent", "controller"} {
		t.Run(name, func(t *testing.T) {
			c := New()
			// The command instances are package globals and may retain a context
			// from an earlier execution in this process.
			child, _, err := c.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			originalContext := child.Context()
			t.Cleanup(func() { child.SetContext(originalContext) })
			child.SetContext(ctx)
			c.SetArgs([]string{name})
			if err := c.ExecuteContext(ctx); err != nil {
				t.Fatalf("cancelled command: %v", err)
			}
		})
	}
}
