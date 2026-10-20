package resources

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxCommandBytes = 16 * 1024

var ErrCommandMissing = errors.New("nvidia-smi missing")
var errOutputLimit = errors.New("nvidia-smi output limit")

// Collector combines independent sources; a GPU failure never discards native
// CPU/RAM or owned process observations. Seams remain private and testable.
type Collector struct {
	system    func(context.Context) SystemReading
	processes func(context.Context) ([]ProcessReading, string)
	command   func(context.Context) ([]byte, error)
	now       func() time.Time
}

func NewCollector(processes func(context.Context) ([]ProcessReading, string)) *Collector {
	return &Collector{system: collectSystem, processes: processes, command: runGPU, now: time.Now}
}

func (c *Collector) Collect(ctx context.Context) Reading {
	r := Reading{System: c.system(ctx)}
	if c.processes != nil {
		r.Processes, r.ProcessCoverage = c.processes(ctx)
	}
	start := c.now()
	r.GPU = GPU{Source: "nvidia-smi", Scope: "whole_device", AttemptedAt: &start, Devices: []Device{}}
	if err := ctx.Err(); err != nil {
		r.GPU.Reason = "collection_canceled"
		return r
	}
	gpuCtx, cancel := context.WithTimeout(ctx, time.Second)
	data, err := c.command(gpuCtx)
	if err == nil {
		err = gpuCtx.Err()
	}
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, ErrCommandMissing):
			r.GPU.Reason = "nvidia_smi_missing"
		case errors.Is(err, context.DeadlineExceeded):
			r.GPU.Reason = "nvidia_smi_timeout"
		case errors.Is(err, context.Canceled):
			r.GPU.Reason = "collection_canceled"
		case errors.Is(err, errOutputLimit):
			r.GPU.Reason = "nvidia_smi_output_limit"
		default:
			r.GPU.Reason = "nvidia_smi_failed"
		}
		return r
	}
	at := c.now()
	devices, err := parseGPU(data, at)
	if err != nil {
		r.GPU.Reason = "nvidia_smi_invalid_output"
		return r
	}
	r.GPU.Devices = devices
	r.GPU.SampleWindowMS = at.Sub(start).Milliseconds()
	for _, device := range devices {
		if device.Utilization.Available || device.MemoryTotal.Available || device.MemoryUsed.Available || device.MemoryFree.Available {
			r.GPU.Available = true
		}
	}
	if r.GPU.Available {
		r.GPU.SampledAt = &at
	} else {
		r.GPU.Reason = "nvidia_smi_metrics_unavailable"
	}
	if len(devices) == 0 {
		r.GPU.Reason = "no_devices_reported"
	}
	return r
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxCommandBytes {
		b.overflow = true
		return 0, errOutputLimit
	}
	return b.Buffer.Write(p)
}

func runGPU(ctx context.Context) ([]byte, error) {
	// Fixed arguments only: no shell, command lines, UUIDs or arbitrary processes.
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,utilization.gpu,memory.total,memory.used,memory.free", "--format=csv,noheader,nounits")
	var out, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &out, &stderr
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if out.overflow || stderr.overflow {
		return nil, errOutputLimit
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, ErrCommandMissing
	}
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func parseGPU(data []byte, at time.Time) ([]Device, error) {
	if len(data) > maxCommandBytes {
		return nil, errOutputLimit
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = 5
	devices := []Device{}
	seen := map[int]bool{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(devices) >= MaxDevices {
			return nil, errors.New("invalid or excessive GPU rows")
		}
		index, err := strconv.Atoi(strings.TrimSpace(row[0]))
		if err != nil || index < 0 || index > 65535 || seen[index] {
			return nil, errors.New("invalid GPU index")
		}
		seen[index] = true
		metric := func(text, unit string, scale, limit float64) Metric {
			text = strings.TrimSpace(text)
			reason := "nvidia_smi_invalid_value"
			if text == "N/A" || text == "[N/A]" || text == "[Not Supported]" {
				reason = "nvidia_smi_not_available"
			}
			v, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > limit {
				return unavailable(unit, "nvidia-smi", "whole_device", reason)
			}
			return measured(v*scale, unit, "nvidia-smi", "whole_device", at)
		}
		d := Device{Index: index, Utilization: metric(row[1], "percent", 1, 100), MemoryTotal: metric(row[2], "bytes", 1024*1024, 1<<40), MemoryUsed: metric(row[3], "bytes", 1024*1024, 1<<40), MemoryFree: metric(row[4], "bytes", 1024*1024, 1<<40)}
		if d.MemoryTotal.Value != nil {
			for _, m := range []*Metric{&d.MemoryUsed, &d.MemoryFree} {
				if m.Value != nil && *m.Value > *d.MemoryTotal.Value {
					*m = unavailable("bytes", "nvidia-smi", "whole_device", "nvidia_smi_inconsistent_memory")
				}
			}
		}
		devices = append(devices, d)
	}
	return devices, nil
}
