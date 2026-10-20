// Package resources provides bounded, read-only hardware observations. It never
// grants admission, controls a process, or certifies resource release.
package resources

import "time"

const (
	ActiveInterval    = 2 * time.Second
	IdleInterval      = 10 * time.Second
	CollectionTimeout = 1500 * time.Millisecond
	MaxProcesses      = 128
	MaxDevices        = 16
)

// Metric carries its own observation time. An unavailable value is always null;
// a failed observation has no successful sampled_at to misrepresent as current.
type Metric struct {
	Value          *float64   `json:"value"`
	Unit           string     `json:"unit"`
	Source         string     `json:"source"`
	Scope          string     `json:"scope"`
	Available      bool       `json:"available"`
	Reason         string     `json:"reason,omitempty"`
	SampledAt      *time.Time `json:"sampled_at"`
	SampleWindowMS int64      `json:"sample_window_ms"`
	Stale          bool       `json:"stale"`
}

type CPU struct {
	Usage    Metric `json:"usage"`
	Logical  Metric `json:"logical_count"`
	Physical Metric `json:"physical_count"`
}

type Memory struct {
	Total     Metric `json:"total"`
	Available Metric `json:"available"`
	Used      Metric `json:"used"`
}

type Device struct {
	Index       int    `json:"index"`
	Utilization Metric `json:"utilization"`
	MemoryTotal Metric `json:"memory_total"`
	MemoryUsed  Metric `json:"memory_used"`
	MemoryFree  Metric `json:"memory_free"`
}

type GPU struct {
	Source         string     `json:"source"`
	Scope          string     `json:"scope"`
	Available      bool       `json:"available"`
	Reason         string     `json:"reason,omitempty"`
	SampledAt      *time.Time `json:"sampled_at"`
	AttemptedAt    *time.Time `json:"attempted_at"`
	SampleWindowMS int64      `json:"sample_window_ms"`
	Stale          bool       `json:"stale"`
	Devices        []Device   `json:"devices"`
}

type Process struct {
	Instance          string `json:"instance"`
	Generation        uint64 `json:"generation"`
	Coverage          string `json:"coverage"`
	Reason            string `json:"reason,omitempty"`
	ObservedProcesses int    `json:"observed_processes"`
	CPU               Metric `json:"cpu"`
	WorkingSet        Metric `json:"working_set"`
	PrivateBytes      Metric `json:"private_bytes"`
	GPUUtilization    Metric `json:"gpu_utilization"`
	GPUMemory         Metric `json:"gpu_memory"`
}

type Snapshot struct {
	SampledAt        *time.Time `json:"sampled_at"`
	SampleWindowMS   int64      `json:"sample_window_ms"`
	IntervalMS       int64      `json:"interval_ms"`
	TargetIntervalMS int64      `json:"target_interval_ms"`
	Stale            bool       `json:"stale"`
	Throttled        bool       `json:"throttled"`
	ThrottleReason   string     `json:"throttle_reason,omitempty"`
	Gaming           Metric     `json:"gaming"`
	CPU              CPU        `json:"cpu"`
	Memory           Memory     `json:"memory"`
	GPU              GPU        `json:"gpu"`
	Processes        []Process  `json:"processes"`
	ProcessCoverage  string     `json:"process_coverage"`
}

// Internal readings deliberately contain no command lines, model paths or process
// inventory. Instance supplies observations from its existing held owned tree.
type SystemReading struct {
	At                             time.Time
	Source, Reason, PhysicalReason string
	Total, Idle                    uint64
	CPUValid                       bool
	Logical, Physical              int
	MemoryTotal, MemoryAvailable   uint64
	MemoryValid                    bool
	MemoryReason                   string
}

type ProcessReading struct {
	Instance                                   string
	Generation                                 uint64
	Identity                                   string // boot-local held generation + observed member identities; never exported
	At                                         time.Time
	Source, Coverage, Reason                   string
	ObservedProcesses                          int
	CPUTimeNS                                  *uint64
	WorkingSet, PrivateBytes                   *float64
	CPUReason, WorkingSetReason, PrivateReason string
}

type Reading struct {
	System          SystemReading
	GPU             GPU
	Processes       []ProcessReading
	ProcessCoverage string
}

func Number(v float64) *float64 { return &v }

func unavailable(unit, source, scope, reason string) Metric {
	return Metric{Unit: unit, Source: source, Scope: scope, Reason: reason}
}

func measured(value float64, unit, source, scope string, at time.Time) Metric {
	return Metric{Value: Number(value), Unit: unit, Source: source, Scope: scope, Available: true, SampledAt: &at}
}
