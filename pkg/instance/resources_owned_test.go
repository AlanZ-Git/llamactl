//go:build linux || windows

package instance

import (
	"context"
	"llamactl/pkg/resources"
	"testing"
	"time"
)

func TestOwnedResourceTreeCountsAndRetirement(t *testing.T) {
	inst, _, _ := treeFixture(t, "stubborn")
	var got resources.ProcessReading
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = inst.SampleOwnedResources(context.Background())
		if got.ObservedProcesses >= 3 && got.CPUTimeNS != nil && got.WorkingSet != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.ObservedProcesses != 3 || got.CPUTimeNS == nil || got.WorkingSet == nil || *got.WorkingSet <= 0 || got.Generation == 0 {
		t.Fatalf("owned root/child/grandchild not observed: %+v", got)
	}
	inst.process.mu.RLock()
	tree := inst.process.tree
	inst.process.mu.RUnlock()
	if err := tree.signal(true); err != nil {
		t.Fatal(err)
	}
	waitTreeGone(t, tree)
	got = inst.SampleOwnedResources(context.Background())
	if got.CPUTimeNS != nil || got.WorkingSet != nil || got.ObservedProcesses != 0 {
		t.Fatalf("exited generation read recycled PID: %+v", got)
	}
}
