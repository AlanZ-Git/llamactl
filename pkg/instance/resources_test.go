package instance

import (
	"context"
	"testing"
)

func TestOwnedResourceObservationNeverAdoptsStatusPID(t *testing.T) {
	i := &Instance{Name: "synthetic", status: newStatus(Running)}
	i.process = newProcess(i)
	got := i.SampleOwnedResources(context.Background())
	if got.CPUTimeNS != nil || got.WorkingSet != nil || got.Reason != "no_owned_generation" {
		t.Fatalf("status must not authorize PID reads: %+v", got)
	}
	i.process.mu.Lock()
	got = i.SampleOwnedResources(context.Background())
	i.process.mu.Unlock()
	if got.Reason != "process_lifecycle_busy" {
		t.Fatalf("sampler waited on lifecycle: %+v", got)
	}
}
