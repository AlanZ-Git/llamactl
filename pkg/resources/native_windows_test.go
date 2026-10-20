//go:build windows

package resources

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsNativeResourceObservations(t *testing.T) {
	system := collectSystem(context.Background())
	if system.Logical <= 0 || !system.MemoryValid || system.MemoryTotal == 0 || system.MemoryAvailable > system.MemoryTotal {
		t.Fatal("native system counts or RAM unavailable")
	}
	if system.Logical > 64 && system.CPUValid {
		t.Fatal("one processor group mislabeled as whole host")
	}
	// This is the test's own held process object, not an arbitrary PID lookup.
	process := WindowsProcess(windows.CurrentProcess())
	if process.CPUTimeNS == nil || process.WorkingSet == nil || *process.WorkingSet <= 0 || process.PrivateBytes == nil {
		t.Fatal("held-process native counters unavailable")
	}
	invalid := WindowsProcess(0)
	if invalid.CPUTimeNS != nil || invalid.WorkingSet != nil || invalid.PrivateBytes != nil || invalid.CPUReason != "get_process_times_failed" || invalid.PrivateReason != "get_process_memory_info_failed" {
		t.Fatal("failed native APIs fabricated metrics")
	}
}
