//go:build !linux

package telemetry

import "fmt"

type Scheduler struct{}

func Open(string, uint32) (*Scheduler, error) {
	return nil, fmt.Errorf("kernel telemetry requires Linux; use granite demo on this platform")
}
func (*Scheduler) Sample() (float64, uint64, error) { return 0, 0, fmt.Errorf("Linux required") }
func (*Scheduler) Close()                           {}
