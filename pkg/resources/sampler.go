package resources

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

type Sampler struct {
	mu                  sync.RWMutex
	snapshot            Snapshot
	collect             func(context.Context) Reading
	slow                func() bool
	now                 func() time.Time
	startOnce, stopOnce sync.Once
	cancel              context.CancelFunc
	done                chan struct{}
	lifecycle           sync.Mutex
	stopped             bool
	previous            SystemReading
	previousStart       time.Time
	previousProcesses   map[string]ProcessReading
}

func NewSampler(collect func(context.Context) Reading, slow func() bool) *Sampler {
	s := &Sampler{collect: collect, slow: slow, now: time.Now, done: make(chan struct{}), previousProcesses: make(map[string]ProcessReading)}
	s.snapshot = s.convert(Reading{}, time.Time{}, ActiveInterval)
	s.snapshot.Stale = true
	return s
}

func (s *Sampler) Start() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.stopped {
		return
	}
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		go func() {
			defer close(s.done)
			for ctx.Err() == nil {
				started := s.now()
				interval := s.sample(ctx)
				wait := max(time.Duration(0), interval-s.now().Sub(started))
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	})
}

func (s *Sampler) Stop() {
	s.stopOnce.Do(func() {
		s.lifecycle.Lock()
		s.stopped = true
		cancel := s.cancel
		s.lifecycle.Unlock()
		if cancel != nil {
			cancel()
			<-s.done
		}
	})
}

// Only the sampler goroutine calls sample. Reads never collect or retain history.
func (s *Sampler) sample(ctx context.Context) time.Duration {
	interval := ActiveInterval
	if s.slow != nil && s.slow() {
		interval = IdleInterval
	}
	start := s.now()
	bounded, cancel := context.WithTimeout(ctx, CollectionTimeout)
	r := s.collect(bounded)
	cancel()
	if len(r.Processes) > MaxProcesses {
		r.Processes = r.Processes[:MaxProcesses]
		r.ProcessCoverage = "process_limit"
	}
	if len(r.GPU.Devices) > MaxDevices {
		r.GPU.Devices = r.GPU.Devices[:MaxDevices]
		r.GPU.Reason = "device_limit"
	}
	snap := s.convert(r, start, interval)
	snap.SampleWindowMS = max(int64(0), s.now().Sub(start).Milliseconds())
	if !s.previousStart.IsZero() {
		snap.IntervalMS = max(int64(0), start.Sub(s.previousStart).Milliseconds())
	}
	s.previousStart = start
	s.previous = r.System
	s.previousProcesses = make(map[string]ProcessReading, len(r.Processes))
	for _, p := range r.Processes {
		if p.CPUTimeNS != nil {
			v := *p.CPUTimeNS
			p.CPUTimeNS = &v
		}
		s.previousProcesses[processKey(p)] = p
	}
	s.mu.Lock()
	s.snapshot = copySnapshot(snap, s.now())
	s.mu.Unlock()
	return interval
}

func processKey(p ProcessReading) string {
	return fmt.Sprintf("%s/%d/%s", p.Instance, p.Generation, p.Identity)
}

func (s *Sampler) convert(r Reading, at time.Time, interval time.Duration) Snapshot {
	source := r.System.Source
	if source == "" {
		source = "unavailable"
	}
	reason := r.System.Reason
	if reason == "" {
		reason = "not_sampled"
	}
	snap := Snapshot{TargetIntervalMS: interval.Milliseconds(), Throttled: interval > ActiveInterval,
		Gaming:    unavailable("boolean", "not_collected", "host", "gaming_detection_not_collected"),
		Processes: make([]Process, 0, len(r.Processes)), ProcessCoverage: r.ProcessCoverage, GPU: r.GPU}
	if !at.IsZero() {
		snap.SampledAt = &at
	}
	if snap.Throttled {
		snap.ThrottleReason = "owner_paused_or_idle"
	}
	if snap.ProcessCoverage == "" {
		snap.ProcessCoverage = "manager_owned_local_only"
	}
	if snap.GPU.Source == "" {
		snap.GPU = GPU{Source: "nvidia-smi", Scope: "whole_device", Reason: "not_sampled"}
	}
	if snap.GPU.Devices == nil {
		snap.GPU.Devices = []Device{}
	}
	snap.CPU.Usage = unavailable("percent", source, "host", reason)
	snap.CPU.Logical = unavailable("count", source, "host", reason)
	snap.CPU.Physical = unavailable("count", source, "host", r.System.PhysicalReason)
	if snap.CPU.Physical.Reason == "" {
		snap.CPU.Physical.Reason = "physical_topology_unavailable"
	}
	if r.System.Logical > 0 {
		snap.CPU.Logical = measured(float64(r.System.Logical), "count", source, "host", r.System.At)
	}
	if r.System.Physical > 0 {
		snap.CPU.Physical = measured(float64(r.System.Physical), "count", source, "host", r.System.At)
	}
	if r.System.CPUValid {
		snap.CPU.Usage.Reason = "first_sample"
		prev := s.previous
		if prev.CPUValid {
			snap.CPU.Usage.Reason = "counter_delta_unavailable"
		}
		if prev.CPUValid && prev.Logical != r.System.Logical {
			snap.CPU.Usage.Reason = "logical_capacity_changed"
		} else if prev.CPUValid && r.System.At.After(prev.At) && r.System.Total > prev.Total && r.System.Idle >= prev.Idle && r.System.Idle-prev.Idle <= r.System.Total-prev.Total {
			snap.CPU.Usage = measured(100*(1-float64(r.System.Idle-prev.Idle)/float64(r.System.Total-prev.Total)), "percent", source, "host", r.System.At)
			snap.CPU.Usage.SampleWindowMS = r.System.At.Sub(prev.At).Milliseconds()
		}
	}
	memReason := r.System.MemoryReason
	if memReason == "" {
		memReason = reason
	}
	snap.Memory = Memory{unavailable("bytes", source, "physical_memory", memReason), unavailable("bytes", source, "physical_memory", memReason), unavailable("bytes", source, "physical_memory", memReason)}
	if r.System.MemoryValid && r.System.MemoryAvailable <= r.System.MemoryTotal {
		snap.Memory = Memory{measured(float64(r.System.MemoryTotal), "bytes", source, "physical_memory", r.System.At), measured(float64(r.System.MemoryAvailable), "bytes", source, "physical_memory", r.System.At), measured(float64(r.System.MemoryTotal-r.System.MemoryAvailable), "bytes", source, "physical_memory", r.System.At)}
	}
	for _, p := range r.Processes {
		reason := p.Reason
		if reason == "" {
			reason = "observation_unavailable"
		}
		scope := "manager_owned_process_tree"
		out := Process{Instance: p.Instance, Generation: p.Generation, Coverage: p.Coverage, Reason: p.Reason, ObservedProcesses: p.ObservedProcesses,
			CPU: unavailable("percent_logical_capacity", p.Source, scope, reason), WorkingSet: unavailable("bytes", p.Source, scope, reason), PrivateBytes: unavailable("bytes", p.Source, scope, reason),
			GPUUtilization: unavailable("percent", "not_collected", scope, "device_query_has_no_process_attribution"), GPUMemory: unavailable("bytes", "not_collected", scope, "device_query_has_no_process_attribution")}
		if p.CPUReason != "" {
			out.CPU.Reason = p.CPUReason
		}
		if p.WorkingSetReason != "" {
			out.WorkingSet.Reason = p.WorkingSetReason
		}
		if p.PrivateReason != "" {
			out.PrivateBytes.Reason = p.PrivateReason
		}
		if validNumber(p.WorkingSet) {
			out.WorkingSet = measured(*p.WorkingSet, "bytes", p.Source, scope, p.At)
		}
		if validNumber(p.PrivateBytes) {
			out.PrivateBytes = measured(*p.PrivateBytes, "bytes", p.Source, scope, p.At)
		}
		if p.CPUTimeNS != nil {
			out.CPU.Reason = "first_sample"
			prev, ok := s.previousProcesses[processKey(p)]
			if r.System.Logical <= 0 {
				out.CPU.Reason = "logical_capacity_unavailable"
			} else if ok && s.previous.Logical != r.System.Logical {
				out.CPU.Reason = "logical_capacity_changed"
			} else if ok && prev.CPUTimeNS != nil && *p.CPUTimeNS >= *prev.CPUTimeNS && p.At.After(prev.At) {
				value := 100 * float64(*p.CPUTimeNS-*prev.CPUTimeNS) / float64(p.At.Sub(prev.At)) / float64(r.System.Logical)
				if value <= 100 {
					out.CPU = measured(value, "percent_logical_capacity", p.Source, scope, p.At)
					out.CPU.SampleWindowMS = p.At.Sub(prev.At).Milliseconds()
				} else {
					out.CPU.Reason = "counter_or_capacity_changed"
				}
			}
		}
		snap.Processes = append(snap.Processes, out)
	}
	return snap
}

func validNumber(p *float64) bool {
	return p != nil && *p >= 0 && !math.IsNaN(*p) && !math.IsInf(*p, 0)
}

// Snapshot deep-copies all mutable slices and pointers, then computes age using
// the cadence of this observation (including deliberate slowed sampling).
func (s *Sampler) Snapshot() Snapshot {
	s.mu.RLock()
	snap := s.snapshot
	s.mu.RUnlock()
	return copySnapshot(snap, s.now())
}

func copySnapshot(snap Snapshot, now time.Time) Snapshot {
	snap.Processes = append([]Process{}, snap.Processes...)
	snap.GPU.Devices = append([]Device{}, snap.GPU.Devices...)
	maxAge := max(5*time.Second, 2*time.Duration(snap.TargetIntervalMS)*time.Millisecond)
	stale := func(at *time.Time) bool { return at == nil || now.Sub(*at) > maxAge || now.Before(*at) }
	cloneTime := func(p *time.Time) *time.Time {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	metric := func(m *Metric) {
		if m.Value != nil {
			v := *m.Value
			m.Value = &v
		}
		m.SampledAt = cloneTime(m.SampledAt)
		m.Stale = stale(m.SampledAt)
	}
	snap.SampledAt = cloneTime(snap.SampledAt)
	snap.Stale = stale(snap.SampledAt)
	for _, m := range []*Metric{&snap.Gaming, &snap.CPU.Usage, &snap.CPU.Logical, &snap.CPU.Physical, &snap.Memory.Total, &snap.Memory.Available, &snap.Memory.Used} {
		metric(m)
	}
	snap.GPU.SampledAt = cloneTime(snap.GPU.SampledAt)
	snap.GPU.AttemptedAt = cloneTime(snap.GPU.AttemptedAt)
	snap.GPU.Stale = stale(snap.GPU.SampledAt)
	for j := range snap.GPU.Devices {
		d := &snap.GPU.Devices[j]
		for _, m := range []*Metric{&d.Utilization, &d.MemoryTotal, &d.MemoryUsed, &d.MemoryFree} {
			metric(m)
		}
	}
	for j := range snap.Processes {
		p := &snap.Processes[j]
		for _, m := range []*Metric{&p.CPU, &p.WorkingSet, &p.PrivateBytes, &p.GPUUtilization, &p.GPUMemory} {
			metric(m)
		}
	}
	return snap
}
