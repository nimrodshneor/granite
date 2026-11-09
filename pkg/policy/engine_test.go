package policy

import (
	"math"
	"testing"
	"time"

	"github.com/nimrodshn/granite/pkg/model"
)

func observation() model.Observation {
	return model.Observation{At: time.Unix(1000, 0), TTFTP95Seconds: .8, ITLP95Seconds: .08,
		RequestsPerSecond: 10, GPUUtilization: .5, RunnableDelaySeconds: .01, Samples: 100, Valid: true}
}

func TestEvidenceGates(t *testing.T) {
	for name, change := range map[string]func(*model.Observation){
		"GPU saturated": func(o *model.Observation) { o.GPUUtilization = .95 },
		"no delay":      func(o *model.Observation) { o.RunnableDelaySeconds = .001 },
		"healthy SLO":   func(o *model.Observation) { o.TTFTP95Seconds, o.ITLP95Seconds = .3, .03 },
		"missing GPU":   func(o *model.Observation) { o.Valid = false },
		"low samples":   func(o *model.Observation) { o.Samples = 2 },
		"NaN":           func(o *model.Observation) { o.GPUUtilization = math.NaN() },
	} {
		t.Run(name, func(t *testing.T) {
			e, o := New(DefaultConfig()), observation()
			change(&o)
			for i := 0; i < 5; i++ {
				if d := e.Step(o); d.Action != "observe" {
					t.Fatalf("unexpected decision: %+v", d)
				}
			}
		})
	}
}

func TestTrialRollbackAndCooldown(t *testing.T) {
	for _, reason := range []string{"regression", "expiry", "missing telemetry", "throughput"} {
		t.Run(reason, func(t *testing.T) {
			e, o := New(DefaultConfig()), observation()
			for i := 0; i < 3; i++ {
				d := e.Step(o)
				if (i == 2) != (d.Action == "apply") {
					t.Fatalf("wrong hysteresis: %+v", d)
				}
				o.At = o.At.Add(5 * time.Second)
			}
			switch reason {
			case "regression":
				o.TTFTP95Seconds = 2
			case "expiry":
				o.At = o.At.Add(time.Minute)
			case "missing telemetry":
				o.Valid = false
			case "throughput":
				o.RequestsPerSecond = 8
			}
			if d := e.Step(o); d.Action != "rollback" {
				t.Fatalf("expected rollback: %+v", d)
			}
			o.Valid, o.TTFTP95Seconds, o.RequestsPerSecond = true, .8, 10
			for i := 0; i < 5; i++ {
				if d := e.Step(o); d.Action != "observe" {
					t.Fatalf("cooldown: %+v", d)
				}
			}
		})
	}
}
