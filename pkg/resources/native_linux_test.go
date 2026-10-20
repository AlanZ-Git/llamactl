//go:build linux

package resources

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLinuxSystemUnitsTopologyAndIndependentFailures(t *testing.T) {
	files := map[string]string{
		"/proc/stat":    "cpu  10 20 30 40 50 60 70 80 999 999\ncpu0 1\ncpu1 1\n",
		"/proc/meminfo": "MemTotal: 100 kB\nMemAvailable: 40 kB\nSwapTotal: 1000 kB\n",
		"/proc/cpuinfo": "processor: 0\nphysical id: 0\ncore id: 0\n\nprocessor: 1\nphysical id: 0\ncore id: 0\n",
	}
	read := func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, errors.New("missing")
		}
		return []byte(value), nil
	}
	r := linuxSystem(context.Background(), read, time.Now)
	if !r.CPUValid || r.Total != 360 || r.Idle != 90 || r.Logical != 2 || r.Physical != 1 {
		t.Fatalf("CPU topology/counters: %+v", r)
	}
	if !r.MemoryValid || r.MemoryTotal != 102400 || r.MemoryAvailable != 40960 {
		t.Fatalf("physical memory units: %+v", r)
	}
	delete(files, "/proc/stat")
	files["/proc/cpuinfo"] = "processor: 0\n"
	r = linuxSystem(context.Background(), read, time.Now)
	if r.CPUValid || r.Physical != 0 || !r.MemoryValid {
		t.Fatal("independent failures lost memory or invented physical count")
	}
	files["/proc/meminfo"] = "MemTotal: 10 kB\nMemAvailable: 20 kB\n"
	r = linuxSystem(context.Background(), read, time.Now)
	if r.MemoryValid {
		t.Fatal("invalid memory accepted")
	}
}
