package enforce

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedWeightAndRestore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cpu.weight")
	if err := os.WriteFile(p, []byte("6000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w := &Weight{Path: p}
	if err := w.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.TrimSpace(string(b)) != "10000" {
		t.Fatalf("weight: %s", b)
	}
	if err := w.Rollback(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "6000\n" {
		t.Fatalf("restored: %q", b)
	}
}

func TestExternalChangeIsNotOverwritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cpu.weight")
	if err := os.WriteFile(p, []byte("100"), 0600); err != nil {
		t.Fatal(err)
	}
	w := &Weight{Path: p}
	if err := w.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("300"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Rollback(); err == nil {
		t.Fatal("expected conflict")
	}
}
