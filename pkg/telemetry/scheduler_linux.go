package telemetry

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

type counters struct{ DelayNS, Samples uint64 }

type Scheduler struct {
	collection *ebpf.Collection
	links      []link.Link
	previous   counters
}

func Open(object string, pid uint32) (*Scheduler, error) {
	if pid == 0 {
		return nil, fmt.Errorf("target host TGID must be positive")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}
	spec, err := ebpf.LoadCollectionSpec(object)
	if err != nil {
		return nil, err
	}
	c, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("load scheduler eBPF: %w", err)
	}
	s := &Scheduler{collection: c}
	if c.Maps["target"] == nil || c.Maps["stats"] == nil {
		s.Close()
		return nil, fmt.Errorf("scheduler maps missing")
	}
	if err := c.Maps["target"].Put(uint32(0), pid); err != nil {
		s.Close()
		return nil, err
	}
	for name, tracepoint := range map[string]string{"wakeup": "sched_wakeup", "wakeup_new": "sched_wakeup_new", "switch_task": "sched_switch", "exit_task": "sched_process_exit"} {
		p := c.Programs[name]
		if p == nil {
			s.Close()
			return nil, fmt.Errorf("scheduler program %s missing", name)
		}
		l, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: tracepoint, Program: p})
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("attach %s: %w", tracepoint, err)
		}
		s.links = append(s.links, l)
	}
	return s, nil
}

// Sample returns the interval mean across all threads of the configured TGID.
// Per-CPU cumulative counters avoid read-and-clear races with kernel updates.
func (s *Scheduler) Sample() (float64, uint64, error) {
	var values []counters
	if err := s.collection.Maps["stats"].Lookup(uint32(0), &values); err != nil {
		return 0, 0, err
	}
	var total counters
	for _, v := range values {
		total.DelayNS += v.DelayNS
		total.Samples += v.Samples
	}
	delay, count := total.DelayNS-s.previous.DelayNS, total.Samples-s.previous.Samples
	s.previous = total
	if count == 0 {
		return 0, 0, nil
	}
	return float64(delay) / float64(count) / 1e9, count, nil
}

func (s *Scheduler) Close() {
	for _, l := range s.links {
		_ = l.Close()
	}
	if s.collection != nil {
		s.collection.Close()
	}
}
