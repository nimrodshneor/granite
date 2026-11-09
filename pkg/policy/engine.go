// Package policy implements a deterministic, bounded trial with hysteresis.
package policy

import (
	"math"
	"time"

	"github.com/nimrodshn/granite/pkg/model"
)

type Config struct {
	TTFTSLO                 float64
	ITLSLO                  float64
	MinDelay                float64
	MaxGPU                  float64
	MinSamples              uint64
	Consecutive             int
	Trial                   time.Duration
	Cooldown                time.Duration
	MinImprovement          float64
	MaxThroughputRegression float64
}

func DefaultConfig() Config {
	return Config{TTFTSLO: .5, ITLSLO: .05, MinDelay: .005, MaxGPU: .85,
		MinSamples: 20, Consecutive: 3, Trial: 30 * time.Second, Cooldown: time.Minute,
		MinImprovement: .05, MaxThroughputRegression: .05}
}

type Engine struct {
	Config   Config
	streak   int
	active   bool
	until    time.Time
	cooldown time.Time
	baseline model.Observation
}

func New(c Config) *Engine { return &Engine{Config: c} }

func valid(o model.Observation) bool {
	for _, v := range []float64{o.TTFTP95Seconds, o.ITLP95Seconds, o.RequestsPerSecond, o.GPUUtilization, o.RunnableDelaySeconds} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return o.Valid && o.GPUUtilization <= 1 && !o.At.IsZero()
}

// Step emits apply/rollback intents. The caller must call Reset after a failed
// application; the engine never performs host mutations itself.
func (e *Engine) Step(o model.Observation) model.Decision {
	c := e.Config
	if !valid(o) || o.Samples < c.MinSamples {
		e.streak = 0
		if e.active {
			return e.rollback(o.At, "missing telemetry; end trial")
		}
		return model.Decision{Action: "observe", Reason: "insufficient telemetry"}
	}
	if e.active {
		if o.RequestsPerSecond < e.baseline.RequestsPerSecond*(1-c.MaxThroughputRegression) ||
			o.TTFTP95Seconds > e.baseline.TTFTP95Seconds*1.1 || o.ITLP95Seconds > e.baseline.ITLP95Seconds*1.1 {
			return e.rollback(o.At, "latency or throughput regressed")
		}
		if !o.At.Before(e.until) {
			reason := "trial expired without sufficient improvement"
			if e.score(o) <= e.score(e.baseline)*(1-c.MinImprovement) {
				reason = "trial improved latency; restore baseline for bounded A/B evaluation"
			}
			return e.rollback(o.At, reason)
		}
		return model.Decision{Action: "hold", Reason: "evaluate bounded trial"}
	}
	if o.At.Before(e.cooldown) {
		return model.Decision{Action: "observe", Reason: "cooldown"}
	}
	if e.score(o) <= 1 || o.RunnableDelaySeconds < c.MinDelay || o.GPUUtilization >= c.MaxGPU {
		e.streak = 0
		return model.Decision{Action: "observe", Reason: "CPU bottleneck evidence not sufficient"}
	}
	e.streak++
	if e.streak < c.Consecutive {
		return model.Decision{Action: "observe", Reason: "waiting for sustained contention"}
	}
	e.active, e.baseline, e.until, e.streak = true, o, o.At.Add(c.Trial), 0
	return model.Decision{Action: "apply", Reason: "sustained SLO violation, runnable delay, and GPU headroom"}
}

func (e *Engine) score(o model.Observation) float64 {
	return math.Max(o.TTFTP95Seconds/e.Config.TTFTSLO, o.ITLP95Seconds/e.Config.ITLSLO)
}

func (e *Engine) rollback(now time.Time, reason string) model.Decision {
	e.Reset(now)
	return model.Decision{Action: "rollback", Reason: reason}
}

func (e *Engine) Reset(now time.Time) {
	e.active, e.streak, e.cooldown = false, 0, now.Add(e.Config.Cooldown)
}
