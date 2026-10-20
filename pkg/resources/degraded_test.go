package resources

import (
	"context"
	"testing"
	"time"
)

func TestGPUAllUnavailableIsNotFreshHardware(t *testing.T) {
	c := NewCollector(nil)
	c.system = func(context.Context) SystemReading { return SystemReading{} }
	c.command = func(context.Context) ([]byte, error) { return []byte("0, N/A, N/A, N/A, N/A\n"), nil }
	r := c.Collect(context.Background())
	if r.GPU.Available || r.GPU.SampledAt != nil || r.GPU.AttemptedAt == nil || r.GPU.Reason != "nvidia_smi_metrics_unavailable" {
		t.Fatalf("unavailable fields labeled fresh: %+v", r.GPU)
	}
}

func TestGPUCommandHonorsEarlierCollectionDeadline(t *testing.T) {
	c := NewCollector(nil)
	c.system = func(context.Context) SystemReading {
		return SystemReading{MemoryValid: true, MemoryTotal: 100, MemoryAvailable: 50}
	}
	c.command = func(ctx context.Context) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("unbounded GPU command context")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := c.Collect(ctx)
	if r.GPU.Reason != "nvidia_smi_timeout" || r.GPU.SampledAt != nil || !r.System.MemoryValid {
		t.Fatal("deadline lost independent native observation")
	}
}

func TestCPUCapacityChangeResetsDeltas(t *testing.T) {
	now := time.Now()
	logical := 2
	total := uint64(100)
	cpu := uint64(time.Second)
	s := NewSampler(func(context.Context) Reading {
		return Reading{System: SystemReading{At: now, CPUValid: true, Total: total, Idle: total / 2, Logical: logical}, Processes: []ProcessReading{{Instance: "fixture", At: now, CPUTimeNS: &cpu}}}
	}, nil)
	s.now = func() time.Time { return now }
	s.sample(context.Background())
	now = now.Add(time.Second)
	logical = 4
	total += 100
	cpu += uint64(time.Second)
	s.sample(context.Background())
	r := s.Snapshot()
	if r.CPU.Usage.Value != nil || r.Processes[0].CPU.Value != nil {
		t.Fatal("capacity change used an incompatible CPU baseline")
	}
}
