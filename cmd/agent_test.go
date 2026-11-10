package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimrodshn/granite/pkg/enforce"
	"github.com/nimrodshn/granite/pkg/model"
	"github.com/nimrodshn/granite/pkg/telemetry"
)

func TestAgentCleanupRestoresWeight(t *testing.T) {
	path := filepath.Join(t.TempDir(), cpuWeightFile)
	if err := os.WriteFile(path, []byte("100"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &runtimeAgent{weight: &enforce.Weight{Path: path}, scheduler: &telemetry.Scheduler{}}
	if err := a.enforceDecision(model.Decision{Action: "apply"}); err != nil {
		t.Fatal(err)
	}
	assertWeight := func(want string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("expected weight %s, got %q: %v", want, b, err)
		}
	}
	assertWeight("100") // Observation mode must not change the host.
	a.args.enable = true
	if err := a.enforceDecision(model.Decision{Action: "apply"}); err != nil {
		t.Fatal(err)
	}
	assertWeight("200")
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	assertWeight("100")
}

func TestAgentHTTPReports(t *testing.T) {
	store := &reportStore{}
	handler := store.handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, agentHealthPath, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health before telemetry: %d", response.Code)
	}
	store.publish(report{Observation: model.Observation{Valid: true, Samples: 42}, Decision: model.Decision{Action: "observe"}})
	for _, tc := range []struct{ path, content string }{
		{agentHealthPath, "ok"},
		{agentStatusPath, `"action":"observe"`},
		{agentMetricsPath, "granite_scheduling_samples 42"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.content) {
			t.Fatalf("%s: status %d, body %s", tc.path, response.Code, response.Body.String())
		}
	}
}
