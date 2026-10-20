package manager

import (
	"encoding/json"
	"fmt"
	"llamactl/pkg/resources"
	"time"
)

type DeploymentMetric struct {
	Unit     string     `json:"unit"`
	Source   string     `json:"source"`
	Scope    string     `json:"scope"`
	Mean     *float64   `json:"mean"`
	Peak     *float64   `json:"observed_peak"`
	Observed int        `json:"observed"`
	Missing  int        `json:"missing"`
	Stale    int        `json:"stale"`
	Coverage float64    `json:"coverage"`
	FirstAt  *time.Time `json:"first_at"`
	LastAt   *time.Time `json:"last_at"`
	Reason   string     `json:"reason,omitempty"`
}

type DeploymentResources struct {
	Source            string                      `json:"source"`
	BaselineAt        *time.Time                  `json:"baseline_at"`
	Baseline          map[string]resources.Metric `json:"baseline"`
	Metrics           map[string]DeploymentMetric `json:"metrics"`
	DistinctSnapshots int                         `json:"distinct_snapshots"`
	Gap               string                      `json:"gap,omitempty"`
	Contention        string                      `json:"contention"`
}

type deploymentResourceAccumulator struct {
	report       DeploymentResources
	lastSnapshot time.Time
	seen         map[string]time.Time
	start        time.Time
}

func deploymentMetrics(s resources.Snapshot, name string) map[string]resources.Metric {
	out := map[string]resources.Metric{"host.cpu": s.CPU.Usage, "host.ram_used": s.Memory.Used}
	for _, d := range s.GPU.Devices {
		out[fmt.Sprintf("device.%d.utilization", d.Index)] = d.Utilization
		out[fmt.Sprintf("device.%d.memory_used", d.Index)] = d.MemoryUsed
	}
	if len(s.GPU.Devices) == 0 {
		out["device.gpu"] = resources.Metric{Unit: "percent", Source: s.GPU.Source, Scope: s.GPU.Scope, Reason: s.GPU.Reason, Stale: s.GPU.Stale, SampledAt: s.GPU.AttemptedAt}
	}
	for _, p := range s.Processes {
		if p.Instance == name {
			prefix := fmt.Sprintf("owned.generation.%d.", p.Generation)
			out[prefix+"cpu"] = p.CPU
			out[prefix+"working_set"] = p.WorkingSet
			out[prefix+"private_bytes"] = p.PrivateBytes
			out[prefix+"gpu"] = p.GPUUtilization
			out[prefix+"vram"] = p.GPUMemory
		}
	}
	return out
}

func newDeploymentResources(s resources.Snapshot, ok bool, name string, start time.Time) *deploymentResourceAccumulator {
	a := &deploymentResourceAccumulator{start: start, seen: map[string]time.Time{}, report: DeploymentResources{
		Source: "shared_resource_cache", Baseline: map[string]resources.Metric{}, Metrics: map[string]DeploymentMetric{}, Contention: "unknown_no_exclusive_resource_claim",
	}}
	if !ok || s.SampledAt == nil {
		a.report.Gap = "baseline_unavailable"
		return a
	}
	a.report.BaselineAt = s.SampledAt
	a.report.Baseline = deploymentMetrics(s, name)
	if s.Stale {
		a.report.Gap = "baseline_stale"
	}
	cpu := s.CPU.Usage
	if cpu.Available && !cpu.Stale && cpu.Value != nil && *cpu.Value > 50 {
		a.report.Contention = "baseline_host_cpu_above_50_percent_not_attributed"
	}
	for _, d := range s.GPU.Devices {
		if d.Utilization.Available && !d.Utilization.Stale && d.Utilization.Value != nil && *d.Utilization.Value > 20 {
			a.report.Contention = "baseline_device_busy_not_attributed"
		}
	}
	return a
}

func (a *deploymentResourceAccumulator) observe(s resources.Snapshot, ok bool, name string) {
	if !ok || s.SampledAt == nil {
		if a.report.Gap == "" {
			a.report.Gap = "sampler_unavailable"
		}
		return
	}
	if !s.SampledAt.After(a.lastSnapshot) || s.SampledAt.Before(a.start) {
		return
	}
	a.lastSnapshot = *s.SampledAt
	a.report.DistinctSnapshots++
	metrics := deploymentMetrics(s, name)
	// A failed GPU collection (or vanished owned generation) may omit entries
	// altogether. Their absence is a missing attempt, not continuing 100% coverage.
	for key, previous := range a.report.Metrics {
		if _, present := metrics[key]; !present {
			metrics[key] = resources.Metric{Unit: previous.Unit, Source: previous.Source, Scope: previous.Scope, Reason: "metric_absent_in_snapshot"}
		}
	}
	for k, m := range metrics {
		agg := a.report.Metrics[k]
		agg.Unit, agg.Source, agg.Scope = m.Unit, m.Source, m.Scope
		at := m.SampledAt
		if at == nil {
			at = s.SampledAt
		}
		if prev, found := a.seen[k]; found && !at.After(prev) {
			continue
		}
		a.seen[k] = *at
		switch {
		case m.Stale || s.Stale:
			agg.Stale++
			agg.Reason = "stale_observation"
		case !m.Available || m.Value == nil || m.SampledAt == nil || m.SampledAt.Before(a.start):
			agg.Missing++
			agg.Reason = m.Reason
		default:
			if agg.Mean == nil {
				v := *m.Value
				agg.Mean = &v
				peak := v
				agg.Peak = &peak
				agg.FirstAt = m.SampledAt
			} else {
				mean := (*agg.Mean*float64(agg.Observed) + *m.Value) / float64(agg.Observed+1)
				agg.Mean = &mean
				if *m.Value > *agg.Peak {
					peak := *m.Value
					agg.Peak = &peak
				}
			}
			agg.Observed++
			agg.LastAt = m.SampledAt
		}
		agg.Coverage = float64(agg.Observed) / float64(agg.Observed+agg.Missing+agg.Stale)
		a.report.Metrics[k] = agg
	}
}

func (a *deploymentResourceAccumulator) result() DeploymentResources {
	b, _ := json.Marshal(a.report)
	var out DeploymentResources
	_ = json.Unmarshal(b, &out)
	if out.DistinctSnapshots == 0 && out.Gap == "" {
		out.Gap = "no_distinct_sample_during_run"
	}
	observed := 0
	for _, metric := range out.Metrics {
		observed += metric.Observed
	}
	if observed == 0 && out.Gap == "" {
		out.Gap = "no_valid_metrics_during_run"
	}
	return out
}
