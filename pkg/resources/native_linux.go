//go:build linux

package resources

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func readNative(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if len(b) > 1024*1024 {
		return nil, io.ErrShortBuffer
	}
	return b, err
}

func collectSystem(ctx context.Context) SystemReading {
	return linuxSystem(ctx, readNative, time.Now)
}

func linuxSystem(ctx context.Context, read func(string) ([]byte, error), now func() time.Time) SystemReading {
	r := SystemReading{At: now(), Source: "linux_proc", Reason: "proc_stat_unavailable", MemoryReason: "proc_meminfo_unavailable"}
	if ctx.Err() != nil {
		r.Reason = "collection_canceled"
		return r
	}
	if data, err := read("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) > 4 && f[0] == "cpu" {
				valid := true
				for j := 1; j < len(f) && j <= 8; j++ { // guest times are already included in user/nice.
					n, err := strconv.ParseUint(f[j], 10, 64)
					if err != nil {
						valid = false
						break
					}
					r.Total += n
					if j == 4 || j == 5 {
						r.Idle += n
					}
				}
				r.CPUValid = valid && r.Total > 0 && r.Idle <= r.Total
			} else if len(f) > 0 && strings.HasPrefix(f[0], "cpu") {
				if _, err := strconv.Atoi(strings.TrimPrefix(f[0], "cpu")); err == nil {
					r.Logical++
				}
			}
		}
	}
	if ctx.Err() != nil {
		return r
	}
	if data, err := read("/proc/meminfo"); err == nil {
		values := map[string]uint64{}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[2] == "kB" {
				if n, err := strconv.ParseUint(f[1], 10, 64); err == nil && n <= ^uint64(0)/1024 {
					values[strings.TrimSuffix(f[0], ":")] = n * 1024
				}
			}
		}
		total, ok1 := values["MemTotal"]
		available, ok2 := values["MemAvailable"]
		if ok1 && ok2 && total > 0 && available <= total {
			r.MemoryTotal = total
			r.MemoryAvailable = available
			r.MemoryValid = true
		}
	}
	// Physical cores are not logical CPUs. Missing topology remains null.
	if ctx.Err() == nil {
		if data, err := read("/proc/cpuinfo"); err == nil {
			cores := map[string]bool{}
			complete := true
			for _, section := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
				socket, core := "", ""
				for _, line := range strings.Split(section, "\n") {
					key, val, ok := strings.Cut(line, ":")
					if !ok {
						continue
					}
					switch strings.TrimSpace(key) {
					case "physical id":
						socket = strings.TrimSpace(val)
					case "core id":
						core = strings.TrimSpace(val)
					}
				}
				if socket == "" || core == "" {
					complete = false
					break
				}
				cores[socket+":"+core] = true
			}
			if complete {
				r.Physical = len(cores)
			}
		}
	}
	return r
}
