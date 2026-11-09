// Package enforce contains the MVP's single reversible intervention.
package enforce

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Weight modifies one explicitly configured cgroup v2. No CPU quota is changed.
// A small relative weight boost is only a fairness guardrail, not a fairness proof.
type Weight struct {
	Path     string
	original string
	applied  string
}

func (w *Weight) Apply() error {
	if w.original != "" {
		return nil
	}
	if filepath.Base(w.Path) != "cpu.weight" {
		return fmt.Errorf("expected a cgroup v2 cpu.weight file")
	}
	b, err := os.ReadFile(w.Path)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 1 || n > 10000 {
		return fmt.Errorf("invalid cpu.weight: %q", b)
	}
	next := min(n*2, 10000)
	if next == n {
		return fmt.Errorf("cpu.weight already at maximum")
	}
	w.original, w.applied = string(b), strconv.Itoa(next)
	if err := os.WriteFile(w.Path, []byte(w.applied), 0); err != nil {
		w.original, w.applied = "", ""
		return err
	}
	return nil
}

func (w *Weight) Rollback() error {
	if w.original == "" {
		return nil
	}
	b, err := os.ReadFile(w.Path)
	if err != nil {
		return err
	}
	// Avoid overwriting an external administrator's change.
	if strings.TrimSpace(string(b)) != w.applied {
		return fmt.Errorf("cpu.weight changed externally; refusing to overwrite")
	}
	if err := os.WriteFile(w.Path, []byte(w.original), 0); err != nil {
		return err
	}
	w.original, w.applied = "", ""
	return nil
}
