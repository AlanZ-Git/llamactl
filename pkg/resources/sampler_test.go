package resources

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSamplerInitialDeltaCacheAndStale(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls int
	s := NewSampler(func(context.Context) Reading {
		calls++
		return Reading{System: SystemReading{Source: "fixture", At: now, Total: uint64(calls * 200), Idle: uint64(calls * 100), CPUValid: true, Logical: 4, Physical: 2, MemoryTotal: 1000, MemoryAvailable: 600, MemoryValid: true}}
	}, func() bool { return false })
	s.now = func() time.Time { return now }
	if s.Snapshot().CPU.Usage.Value != nil {
		t.Fatal("initial CPU must be null")
	}
	s.sample(context.Background())
	first := s.Snapshot()
	if first.CPU.Usage.Value != nil || first.CPU.Usage.Reason != "first_sample" {
		t.Fatalf("first delta: %+v", first.CPU.Usage)
	}
	for range 100 {
		_ = s.Snapshot()
	}
	if calls != 1 {
		t.Fatal("HTTP reads must not sample")
	}
	now = now.Add(2 * time.Second)
	s.sample(context.Background())
	got := s.Snapshot()
	if got.CPU.Usage.Value == nil || *got.CPU.Usage.Value != 50 || got.CPU.Usage.SampleWindowMS != 2000 {
		t.Fatalf("CPU: %+v", got.CPU)
	}
	if got.IntervalMS != 2000 || got.TargetIntervalMS != 2000 {
		t.Fatalf("cadence: %+v", got)
	}
	*got.CPU.Usage.Value = 99
	if *s.Snapshot().CPU.Usage.Value != 50 {
		t.Fatal("snapshot aliases cache")
	}
	now = now.Add(6 * time.Second)
	if !s.Snapshot().Stale || !s.Snapshot().CPU.Usage.Stale {
		t.Fatal("age must mark stale")
	}
	b, err := json.Marshal(first)
	if err != nil || len(b) == 0 {
		t.Fatal(err)
	}
}

func TestSamplerProcessGenerationAndUnits(t *testing.T) {
	now := time.Now()
	cpu := uint64(time.Second)
	generation := uint64(1)
	s := NewSampler(func(context.Context) Reading {
		return Reading{System: SystemReading{At: now, Logical: 4}, Processes: []ProcessReading{{Instance: "fixture", Generation: generation, Identity: "held-object", At: now, Source: "fixture", Coverage: "root_and_children", CPUTimeNS: &cpu, WorkingSet: Number(1024), PrivateBytes: Number(512)}}}
	}, func() bool { return true })
	s.now = func() time.Time { return now }
	s.sample(context.Background())
	if s.Snapshot().Processes[0].CPU.Value != nil {
		t.Fatal("first process delta must be null")
	}
	now = now.Add(time.Second)
	cpu += uint64(2 * time.Second)
	s.sample(context.Background())
	got := s.Snapshot()
	if *got.Processes[0].CPU.Value != 50 || got.Processes[0].CPU.Unit != "percent_logical_capacity" {
		t.Fatalf("normalized process CPU: %+v", got.Processes[0])
	}
	if got.TargetIntervalMS != 10000 || !got.Throttled {
		t.Fatal("paused/idle cadence")
	}
	if got.Processes[0].WorkingSet.Unit != "bytes" || *got.Processes[0].PrivateBytes.Value != 512 {
		t.Fatal("memory units")
	}
	generation++
	now = now.Add(time.Second)
	s.sample(context.Background())
	if s.Snapshot().Processes[0].CPU.Value != nil {
		t.Fatal("new generation reused CPU baseline")
	}
	if len(s.previousProcesses) != 1 {
		t.Fatal("old generations retained")
	}
}

func TestSamplerCancellationAndSingleLifecycle(t *testing.T) {
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	s := NewSampler(func(ctx context.Context) Reading {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return Reading{}
	}, nil)
	s.Start()
	s.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sampler not started")
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancellation leaked collector")
	}
	s.Stop()
	s.Start()
	if calls.Load() != 1 {
		t.Fatal("duplicate sampler")
	}
}

func TestGPUParsingAndBoundedFailures(t *testing.T) {
	at := time.Now()
	devices, err := parseGPU([]byte("0, 25, 100, 40, 60\n1, N/A, 200, 50, 150\n"), at)
	if err != nil || len(devices) != 2 {
		t.Fatal(devices, err)
	}
	if *devices[0].MemoryUsed.Value != 40*1024*1024 || devices[1].Utilization.Value != nil || devices[1].Utilization.Reason != "nvidia_smi_not_available" {
		t.Fatalf("GPU parse: %+v", devices)
	}
	for _, text := range []string{"0, NaN, -1, inf, 3\n", "0, 101, 10, 20, 1\n"} {
		devices, err := parseGPU([]byte(text), at)
		if err != nil {
			t.Fatal(err)
		}
		if devices[0].Utilization.Value != nil || devices[0].MemoryUsed.Value != nil {
			t.Fatal("invalid values accepted")
		}
	}
	if _, err := parseGPU([]byte("bad row"), at); err == nil {
		t.Fatal("malformed row accepted")
	}
	for _, tc := range []struct {
		err    error
		reason string
	}{{ErrCommandMissing, "nvidia_smi_missing"}, {context.DeadlineExceeded, "nvidia_smi_timeout"}, {errors.New("private OS details"), "nvidia_smi_failed"}} {
		c := NewCollector(nil)
		c.system = func(context.Context) SystemReading {
			return SystemReading{At: at, MemoryValid: true, MemoryTotal: 100, MemoryAvailable: 40}
		}
		c.command = func(context.Context) ([]byte, error) { return []byte("0, 1, 2, 1, 1"), tc.err }
		reading := c.Collect(context.Background())
		if !reading.System.MemoryValid || reading.GPU.Reason != tc.reason || len(reading.GPU.Devices) != 0 || reading.GPU.SampledAt != nil {
			t.Fatalf("failed GPU poisoned CPU/RAM or freshened GPU: %+v", reading)
		}
	}
}
