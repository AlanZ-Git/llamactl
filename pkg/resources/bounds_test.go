package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResourceBoundsAndNoGPUFreshening(t *testing.T) {
	now := time.Now()
	devices, _ := parseGPU([]byte("0, 20, 100, 50, 50\n"), now)
	r := Reading{GPU: GPU{Source: "nvidia-smi", Scope: "whole_device", Available: true, SampledAt: &now, Devices: devices}}
	for n := 0; n < MaxProcesses+20; n++ {
		r.Processes = append(r.Processes, ProcessReading{Instance: fmt.Sprint(n)})
	}
	s := NewSampler(func(context.Context) Reading { return r }, nil)
	s.now = func() time.Time { return now }
	s.sample(context.Background())
	*devices[0].Utilization.Value = 99
	got := s.Snapshot()
	if len(got.Processes) != MaxProcesses || len(s.previousProcesses) != MaxProcesses || got.ProcessCoverage != "process_limit" {
		t.Fatal("unbounded retention")
	}
	if *got.GPU.Devices[0].Utilization.Value != 20 {
		t.Fatal("collector retained mutable GPU snapshot")
	}
	*got.GPU.Devices[0].MemoryUsed.Value = 7
	*got.GPU.SampledAt = now.Add(time.Hour)
	if *s.Snapshot().GPU.Devices[0].MemoryUsed.Value == 7 || s.Snapshot().GPU.SampledAt.After(now) {
		t.Fatal("client mutated GPU cache")
	}
	now = now.Add(2 * time.Second)
	r = Reading{GPU: GPU{Source: "nvidia-smi", Scope: "whole_device", Reason: "nvidia_smi_timeout"}}
	s.sample(context.Background())
	got = s.Snapshot()
	if got.GPU.SampledAt != nil || len(got.GPU.Devices) != 0 || !got.GPU.Stale {
		t.Fatal("failed GPU reused old data as fresh")
	}
	if len(s.previousProcesses) != 0 {
		t.Fatal("old process generations retained")
	}
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), `"value":null`) {
		t.Fatal("null missing", err)
	}
}

func TestResourceSlowCadenceAndCounterReset(t *testing.T) {
	now := time.Now()
	slow := true
	total, idle := uint64(1000), uint64(500)
	s := NewSampler(func(context.Context) Reading {
		return Reading{System: SystemReading{At: now, CPUValid: true, Total: total, Idle: idle}}
	}, func() bool { return slow })
	s.now = func() time.Time { return now }
	s.sample(context.Background())
	now = now.Add(11 * time.Second)
	if s.Snapshot().Stale {
		t.Fatal("idle cadence falsely stale")
	}
	now = now.Add(10 * time.Second)
	if !s.Snapshot().Stale {
		t.Fatal("idle cadence never stale")
	}
	slow = false
	total = 1
	idle = 0
	s.sample(context.Background())
	got := s.Snapshot()
	if got.IntervalMS != 21000 || got.TargetIntervalMS != 2000 || got.Throttled || got.CPU.Usage.Value != nil {
		t.Fatal("cadence or counter reset wrong")
	}
}

func TestResourceConcurrentCacheReadersAndStop(t *testing.T) {
	entered := make(chan struct{})
	s := NewSampler(func(ctx context.Context) Reading { close(entered); <-ctx.Done(); return Reading{} }, nil)
	s.Start()
	<-entered
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 50 {
				b, err := json.Marshal(s.Snapshot())
				if err != nil || len(b) == 0 {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(s.Stop)
	wg.Wait()
}

func TestGPUOutputLimitAndPartialValues(t *testing.T) {
	var output boundedOutput
	if _, err := output.Write(make([]byte, maxCommandBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{1}); err == nil || !output.overflow || output.Len() != maxCommandBytes {
		t.Fatal("output limit not enforced")
	}
	row := "0, 1, 100, 20, 80\n"
	if _, err := parseGPU([]byte(strings.Repeat(row, MaxDevices+1)), time.Now()); err == nil {
		t.Fatal("duplicate/excess devices")
	}
	var rows strings.Builder
	for n := 0; n <= MaxDevices; n++ {
		fmt.Fprintf(&rows, "%d, 1, 100, 20, 80\n", n)
	}
	if _, err := parseGPU([]byte(rows.String()), time.Now()); err == nil {
		t.Fatal("device limit")
	}
	c := NewCollector(nil)
	c.system = func(context.Context) SystemReading {
		return SystemReading{MemoryValid: true, MemoryTotal: 100, MemoryAvailable: 50}
	}
	c.command = func(context.Context) ([]byte, error) { return []byte("0, N/A, 100, 20, 80\n"), nil }
	r := c.Collect(context.Background())
	if !r.GPU.Available || r.GPU.Devices[0].Utilization.Value != nil || r.GPU.Devices[0].MemoryUsed.Value == nil {
		t.Fatal("partial GPU failure lost usable memory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	c.command = func(context.Context) ([]byte, error) { called = true; return nil, nil }
	r = c.Collect(ctx)
	if called || r.GPU.SampledAt != nil || r.GPU.Reason != "collection_canceled" {
		t.Fatal("canceled collector launched command")
	}
}
