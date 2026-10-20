# Read-only resource observations

`GET /api/v1/resources` returns the local manager's latest **shared, bounded**
resource snapshot. It does not sample on demand, start a model, refresh readiness,
change policy, or certify release. Use [owner resource policy](resource-policy.md)
for admission and independently verified process release.

## Authorization and lifecycle

The route uses the existing management-read authorization group, exactly like
instance/configuration reads. With required management authentication (including
when a controlled-protocol instance exists), only management authentication or an
existing management session can read it. A scoped controller or inference key does
not gain machine-wide resource access. Legacy anonymous management-read mode is
preserved when that existing group permits it; it does **not** grant owner-policy
access. There is no resources write route.

The production instance manager owns one sampler, starts it once after instance
restoration, and cancels/joins it on shutdown. All clients share that sampler.
HTTP requests only deep-copy the cached snapshot. Responses have
`Cache-Control: no-store`; an embedding without a sampler receives 503
`resources_unavailable`, not a fabricated zero snapshot.

* Target start-to-start interval: **2 seconds** while a local instance is running.
* **10 seconds** when owner-paused or no local instance is running. Manual pause
  wins over running status when selecting this cadence.
* A single collection context has a **1.5-second** budget. NVIDIA execution has a
  **1-second** budget within it; cancellation/pipe cleanup has a 100 ms wait bound.
  Native local API/file calls check the context between bounded operations; a
  context is not a mechanism for forcibly interrupting an OS syscall.
* Only the latest snapshot and previous CPU-counter baselines are retained. No
  samples are written to a database, disk history, or a time-series platform.
* At most **128 local instance observations**, **128 process members per observed
  tree**, and **16 GPUs** are retained. Process traversal has an additional 25 ms
  cooperative per-tree budget and reports partial coverage. No worker per client.
* Measurement failures have no admission, loading, retry, termination, or policy
  effect. There is no automatic throttling of inference or game detection.

## Envelope and metric contract

```json
{
  "sampled_at": "2026-01-01T00:00:00Z",
  "sample_window_ms": 12,
  "interval_ms": 2000,
  "target_interval_ms": 2000,
  "stale": false,
  "throttled": false,
  "gaming": {
    "value": null,
    "unit": "boolean",
    "source": "not_collected",
    "scope": "host",
    "available": false,
    "reason": "gaming_detection_not_collected",
    "sampled_at": null,
    "sample_window_ms": 0,
    "stale": true
  },
  "cpu": {},
  "memory": {},
  "gpu": {},
  "processes": [],
  "process_coverage": "manager_owned_local_only"
}
```

The abbreviated `cpu`, `memory`, and `gpu` above are detailed below. Every numeric
metric has the same envelope as `gaming`: `value` (number or **null**), `unit`,
`source`, `scope`, `available`, optional `reason`, `sampled_at` (UTC RFC3339 or
null), `sample_window_ms`, and `stale`. Unavailable does not mean zero. The first
host/process CPU delta is null with `first_sample`; CPU requires two observations
of a compatible counter identity. A genuine measured zero remains available.

* Top-level `sampled_at` is the **start of the collection cycle**, not a claim that
  every source succeeded. Top-level `sample_window_ms` is collection elapsed time.
* `interval_ms` is the actual gap between cycle starts (0 on the first cycle,
  before an interval has been observed). `target_interval_ms` is the selected
  cadence, not a fabricated measurement of that gap.
* Each metric carries its own successful observation time. CPU `sample_window_ms`
  is its actual counter-delta interval. For reported gauges it is 0 (not a
  manager-calculated delta); the driver's internal GPU utilization integration
  window is not exposed by this query and must not be inferred as 0 ms.
* A timestamp is stale when absent, in the future, or older than
  `max(5 seconds, 2 × target_interval_ms)`. Age is recomputed on every cache read,
  including for each source. Slow sampling is disclosed separately; it does not
  keep a stopped sampler's data fresh forever. The UI additionally ages cached
  data and marks failed/hidden-page reads untrusted.
* `throttled=true` means **sampling** is slower, with
  `throttle_reason=owner_paused_or_idle`. It does not mean GPU thermal throttling,
  that a game was detected, or that inference was rate-limited. `gaming` is null
  with an explicit not-collected reason.

## CPU and physical RAM

`cpu` contains `usage` (percent of total logical capacity), `logical_count`
(logical online processors), and `physical_count` (physical cores, not sockets).
Missing physical topology is null, never copied from the logical count.

`memory` contains `total`, `available`, and `used`, all **bytes of physical RAM**;
`used = total - available`. These are not process working sets, virtual address
space, pagefile size, or private committed bytes.

* **Windows:** `GetSystemTimes`, `GetActiveProcessorCount`,
  `GetLogicalProcessorInformationEx(RelationProcessorCore)`, and
  `GlobalMemoryStatusEx` through `golang.org/x/sys/windows`/native system DLLs.
  On hosts with more than 64 logical processors, `GetSystemTimes` has a processor
  group limitation; whole-host CPU utilization is null with
  `processor_group_cpu_scope_unsupported` rather than reporting one group as the
  machine. Counts and RAM remain independently available.
* **Linux:** bounded `/proc/stat`, `/proc/meminfo` (`MemAvailable`), and
  `/proc/cpuinfo` core/socket topology. Guest CPU counters are not double-counted.
  Missing topology or unavailable `MemAvailable` is not guessed. Physical RAM
  reports the host `/proc` view, not a container's memory quota.
* Other platforms explicitly report native CPU/RAM unsupported. NVIDIA reporting
  may still work independently if the fixed command is available.

## Owned backend processes

`processes[]` contains `instance`, boot-local `generation`, `coverage`, `reason`,
`observed_processes`, and the metrics `cpu`, `working_set`, `private_bytes`,
`gpu_utilization`, `gpu_memory`. Remote stubs are excluded. A stopped descriptor
without a held generation reports null with `no_owned_generation`, not invented
zero usage. The manager's own process is not attributed to a model.

Only the existing owned local tree authorizes observations. Status, saved PIDs,
process names, port owners, command lines, or a machine-wide process inventory
are not a source of ownership. Contended lifecycle locks are not waited on:
`process_lifecycle_busy` / `tree_lifecycle_busy` produces a null observation.

* **Windows:** duplicate the held backend Job and root process handles while the
  tree is known. Root measurements use the held object. A bounded backend Job
  membership list supplies possible children; every opened child handle must
  independently prove membership in that same held Job before metrics are read.
  PID reuse outside the Job therefore cannot authorize a read. Creation time and
  member identities reset CPU baselines when the observed set changes. The
  private console supervisor is excluded by using the backend-only Job.
  `GetProcessTimes` reports kernel+user CPU time;
  `GetProcessMemoryInfo(PROCESS_MEMORY_COUNTERS_EX)` supplies working set and
  `PrivateUsage` (**private commit**, not physical RAM). Access/query failures
  remain source-specific nulls, not a claim of unsupported Windows metrics.
* **Linux:** the existing sole tree waiter pins the inherited group/root lifetime
  during a short, bounded traversal. Open proc-directory FDs anchor process
  identities; a child's group membership is checked within that directory before
  accepting its counters. Discovery follows only the held root's task children
  and their observed children, never all `/proc` PIDs. Stat CPU time uses Linux
  USER_HZ ticks and RSS uses the native page size. Once the root has been reaped,
  descendant enumeration is unavailable (`root_reaped_descendants_not_enumerable`)
  rather than reopening a possibly recycled root PID. Linux private/commit is
  null with `linux_private_commit_not_available`; RSS is not relabeled as commit.
* The sample is **not an atomic census**. Coverage is
  `root_and_observed_children` or `root_and_observed_job_members`, with explicit
  non-atomic/partial/access/limit reasons. A disappearing, inaccessible, or
  unobserved member is not silently counted as zero. A successful aggregate
  describes only the observed members. Aggregate working sets/RSS may double
  count shared pages. Member-set/generation changes reset process CPU deltas.
* Process CPU unit is `percent_logical_capacity`, dividing the observed tree's
  CPU-time delta by wall time and logical processor count. It is **not** a
  single-core percentage that can legitimately exceed 100%. Invalid counter
  deltas are null. Process memory units are bytes.
* Docker/external runtimes have `coverage=local_launcher_only` and
  `external_runtime_not_attributed`. The local launcher tree is observable;
  daemon-managed containers or unrelated processes are not adopted.

## NVIDIA device metrics and failure handling

The only external command is `exec.CommandContext`, without a shell, using fixed
arguments:

```text
nvidia-smi --query-gpu=index,utilization.gpu,memory.total,memory.used,memory.free --format=csv,noheader,nounits
```

`gpu` contains `source=nvidia-smi`, `scope=whole_device`, `available`, `reason`,
`sampled_at`, `attempted_at`, query elapsed `sample_window_ms`, `stale`, and
`devices[]`. Each device has only a numeric `index` and metrics `utilization`,
`memory_total`, `memory_used`, `memory_free`. MiB from NVIDIA is converted to bytes.
GPU utilization and VRAM are **whole-card** observations: they include unmanaged
external programs and are not exact per-model attribution. Release evidence and
GPU memory reclamation remain separate facts.

Output and stderr are each capped at 16 KiB. Parsing is capped at 16 unique device
rows; malformed shapes/indices, duplicate or excessive rows are rejected. `N/A`
and reported unsupported fields become `nvidia_smi_not_available`; invalid,
negative, nonfinite, over-100 utilization, and inconsistent memory values become
null. A single unavailable field does not erase other valid device fields. If
all fields are unavailable, `gpu.available=false` with
`nvidia_smi_metrics_unavailable` and no successful sample timestamp; only the
attempt timestamp is retained.

Missing executable, timeout, cancellation, command failure, output limit or
malformed output yield an empty device set and a specific GPU reason, preserving
native CPU/RAM/process observations. A failed query has **no successful GPU
sample timestamp**. Previous GPU values are never copied into a new successful
looking timestamp. Raw stderr/OS errors, UUIDs, model paths and command lines are
not returned.

This collector deliberately does not call a per-process GPU API. Those metrics
are null with `source=not_collected` and
`device_query_has_no_process_attribution`. This describes this integration's
actual evidence limit, **not** a blanket assertion that WDDM or every Windows
GPU/process API is unsupported. No per-model value is derived by subtracting
whole-card counters. No NVML service, CUDA toolkit, driver, registry change or
new dependency is required.

## WebUI and verification boundary

The dashboard separates owner admission, durability and release progress from
these hardware values. It shows source/scope, null reasons, timestamps, cadence,
stale/error state, and an optional owned-process detail section without graphs.
Polling stops when the document is hidden and on unmount. Owner mutation handling
is specified in [resource-policy.md](resource-policy.md#webui-owner-controls).

Unit/contract tests use fake collectors/clocks/command results, native synthetic
owned-process trees, and the actual HTTP router. An opt-in, time-bounded browser
fixture uses synthetic credentials and a stopped descriptor without loading a
model. Compile checks and Linux/mock tests do not certify native Windows APIs,
actual NVIDIA/WDDM behavior, browser rendering, target-machine overhead, GPU
release, real-model inference, or deployment. Those require separate authorized
verification.
