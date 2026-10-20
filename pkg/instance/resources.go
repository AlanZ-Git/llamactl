package instance

import (
	"context"
	"llamactl/pkg/resources"
	"strings"
	"time"
)

// SampleOwnedResources is a nonblocking observer of the existing lifecycle, not
// an alternate PID registry. A busy/unknown generation never authorizes a read.
func (i *Instance) SampleOwnedResources(ctx context.Context) resources.ProcessReading {
	r := resources.ProcessReading{Instance: i.Name, At: time.Now(), Source: "owned_tree", Coverage: "unavailable", Reason: "no_owned_generation"}
	p := i.process
	if p == nil {
		r.Reason = "remote_instance"
		return r
	}
	if !p.mu.TryRLock() {
		r.Reason = "process_lifecycle_busy"
		return r
	}
	tree, generation, external := p.tree, p.resourceGeneration, p.treeDocker
	p.mu.RUnlock()
	r.Generation = generation
	if tree == nil {
		return r
	}
	if ctx.Err() != nil {
		r.Reason = "collection_canceled"
		return r
	}
	r = tree.sampleResources(ctx)
	r.Instance, r.Generation = i.Name, generation
	if external {
		r.Coverage = "local_launcher_only"
		r.Reason = "external_runtime_not_attributed"
	}
	return r
}

// Sum only an observed set with known membership. Member-set changes reset the
// CPU baseline; working sets may count shared pages more than once.
func sumOwnedReadings(readings []resources.ProcessReading, source, coverage, reason string) resources.ProcessReading {
	out := resources.ProcessReading{At: time.Now(), Source: source, Coverage: coverage, Reason: reason, ObservedProcesses: len(readings)}
	if len(readings) == 0 {
		return out
	}
	var cpu uint64
	var working, private float64
	cpuOK, workingOK, privateOK := true, true, true
	ids := make([]string, 0, len(readings))
	for _, r := range readings {
		ids = append(ids, r.Identity)
		if r.CPUTimeNS == nil || cpu > ^uint64(0)-valueCPU(r) {
			cpuOK = false
			out.CPUReason = r.CPUReason
			if out.CPUReason == "" {
				out.CPUReason = "partial_cpu_observation"
			}
		} else {
			cpu += *r.CPUTimeNS
		}
		if r.WorkingSet == nil {
			workingOK = false
			out.WorkingSetReason = r.WorkingSetReason
			if out.WorkingSetReason == "" {
				out.WorkingSetReason = "partial_working_set_observation"
			}
		} else {
			working += *r.WorkingSet
		}
		if r.PrivateBytes == nil {
			privateOK = false
			out.PrivateReason = r.PrivateReason
			if out.PrivateReason == "" {
				out.PrivateReason = "partial_private_bytes_observation"
			}
		} else {
			private += *r.PrivateBytes
		}
	}
	out.Identity = strings.Join(ids, ",")
	if cpuOK {
		out.CPUTimeNS = &cpu
	}
	if workingOK {
		out.WorkingSet = &working
	}
	if privateOK {
		out.PrivateBytes = &private
	}
	return out
}

func valueCPU(r resources.ProcessReading) uint64 {
	if r.CPUTimeNS == nil {
		return 0
	}
	return *r.CPUTimeNS
}
