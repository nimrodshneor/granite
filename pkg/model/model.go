// Package model defines engine-independent input to the local policy loop.
package model

import "time"

type Observation struct {
	At                   time.Time `json:"at"`
	TTFTP95Seconds       float64   `json:"ttft_p95_seconds"`
	ITLP95Seconds        float64   `json:"itl_p95_seconds"`
	RequestsPerSecond    float64   `json:"requests_per_second"`
	GPUUtilization       float64   `json:"gpu_utilization"` // fraction, not percent
	RunnableDelaySeconds float64   `json:"mean_runnable_delay_seconds"`
	Samples              uint64    `json:"scheduling_samples"`
	Valid                bool      `json:"valid"`
}

type Decision struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}
