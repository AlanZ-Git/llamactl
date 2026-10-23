# Single-configuration deployment tests

This is a small extension of the existing instance control API and management
WebUI, not a benchmark queue, a second configuration store, or a parameter search.
The manager sends **real generation requests with fixed synthetic text** to the
selected, already-registered local `llama_cpp` Router/model. Synthetic child
processes used by the automated tests are not real models or GPU measurements.

Machine-readable contract: [control-openapi.yaml](control-openapi.yaml).
Prerequisites, model registration, explicit boot recovery and finite parameter
semantics: [control-api.zh-CN.md](control-api.zh-CN.md). Owner policy and the shared
sampler: [resource-policy.md](resource-policy.md), [resources.md](resources.md).

## WebUI

On the existing **Instances** management page, below the owner/resource panel,
select a protocol-enabled local Router and a logical model. The small deployment
panel supports optional context/GPU-layer overrides (blank preserves; `0` is an
explicit value), 1–3 measured requests, and optional one-request warmup. Read the
loading/cleanup notice before clicking **运行部署测试** (Run deployment test).

- A `202` response is acceptance, not success. The panel polls the accepted ID
  only while visible. Closing the tab does not cancel or replay the server run.
- Cancel is explicit. Owner pause cancels this work even with graceful release;
  owner resume does not replay, reload, or restore any test.
- Definite new-submit rejection leaves editing available. If the response is
  uncertain, the original envelope/key stays in this tab's session storage;
  **以相同 key 重取本次提交** explicitly resubmits that exact envelope, not a new run.
- Missing history after restart is **unknown/not resumable**, never proof that
  nothing executed. Clearing local display requires a warning confirmation for
  unknown/active work and does **not** cancel server work. Check state first.
- JSON/CSV downloads use the same authenticated API requests; credentials are
  never inserted in download URLs.
- The panel does not automatically recover a boot, edit model paths, resume
  owner policy, or replace an existing ready configuration.

## HTTP API and permissions

| Method | Resource | Meaning |
|---|---|---|
| POST | `/api/v1/instances/{name}/tests` | Validate and accept one finite run, or return the original idempotent run |
| GET | `/api/v1/instances/{name}/tests/{id}` | Live/terminal sanitized JSON report |
| GET | same resource with `?format=csv` | Same report in `field,value` CSV rows |
| POST | `/api/v1/instances/{name}/tests/{id}/cancel` | Cancel explicitly; poll for terminal cleanup |

The same management key/admin session or **fixed-instance scoped-controller** as
control is required, even when legacy authentication switches are disabled.
Ordinary inference keys are forbidden. Controllers can query, export and cancel
only their own runs for their fixed instance. Administrators can diagnose/cancel
other scopes. A missing/foreign run uses the same 404 response to avoid disclosure.
Neither model credentials nor ready tokens are client inputs to this API.

Example request (replace boot/revision with a fresh authenticated control read):

```json
{
  "boot_id": "current-boot-from-control",
  "expected_revision": 0,
  "idempotency_key": "unique-bounded-test-1",
  "model": "alpha",
  "fixture_id": "synthetic-text-v1",
  "patch": {"context": 2048, "gpu_layers": 0, "flash_attention": false},
  "measured_generations": 3,
  "warmup": false,
  "max_output_tokens": 256,
  "timeout_seconds": 600,
  "concurrency": 1,
  "seed": 0
}
```

Only `boot_id`, `expected_revision`, `idempotency_key`, `model`, `fixture_id` are
required. Remaining defaults are as above, except missing `patch` preserves all
parameters. Top-level nulls, duplicate keys (including patch keys), unknown fields,
trailing JSON and encoded bodies are rejected. The unencoded envelope is limited
to 64 KiB. The existing finite `ControlPatch` whitelist is reused: missing keeps,
nullable `null` clears the override, and `0`/`false` retain their exact meaning.
No caller prompt, multimodal data, URLs, filenames, shell, raw INI, backend
addresses, automatic A/B, or search are supported.

### Hard bounds

- One active deployment test per manager, no queue; sequential generations only.
- Measured requests: default **3**, range **1–3**. Warmup: default off, optional
  exactly **1** additional request, separately labeled and not measured as a repeat.
- Output cap: default **256**, range **1–256** tokens per request. Prompt is the
  single fixed bounded `synthetic-text-v1` fixture, bound to a SHA-256 in the report.
- Temperature is fixed at 0; seed defaults to 0, permitted range 0–2147483647.
- Each backend request uses `/v1/chat/completions` with the registered logical
  model, one user message containing the fixed text, `stream=true`,
  `stream_options.include_usage=true`, the bounded `max_tokens`, and
  `cache_prompt=false`. The short-text llama.cpp check has a fixed policy of
  `chat_template_kwargs.enable_thinking=false` (explicit false, not omitted or
  null). This is not a caller/UI option or a general model-template adapter.
  The fixture ID and SHA-256 identify **text only**, not a request-policy version;
  the unchanged text digest does not imply identical policy across builds. Bind
  comparisons to the actual candidate source/binary identity as well.
- Total deadline: default/max **600 seconds**, may be shortened to 1–600. The
  execution context reserves `min(5 seconds, budget/2)` for cleanup. Cleanup has
  an independent context bounded by both 5 seconds and the original run deadline;
  cancellation cannot suppress cleanup, nor can cleanup extend the advertised
  budget. Actual elapsed and cleanup duration are recorded, including normal
  scheduling/unwind overhead. No successful deadline promise substitutes for
  confirmed cleanup.
- **No generation is retried automatically.** Failure stops subsequent requests.

Invalid or overbudget requests are rejected before configuration, process or
model side effects. Generation and response byte/frame limits are separate:
malformed, oversized, empty or incomplete streams fail explicitly, rather than
pretending that a role frame or HTTP 200 was a completed answer.

### Idempotency, epochs and cancellation

Identity is `(scope, instance, boot_id, idempotency_key)` with normalized legal
JSON numbers/ordering and preserved optional-field presence. Same identity and
payload returns the original run, even after configuration revision changes;
a different payload is `409 idempotency_conflict`. New submissions then check
CAS and busy state. Old boot writes are `409 epoch_changed`. Do not swap in a new
boot/key to automatically replay an uncertain request.

The bounded current-boot ledger retains terminal reports for 24 hours after
`ended_at`, with capacity matching the existing control operation capacity
(default 1024 test records). It refuses new submissions at capacity rather than
evicting unexpired results. It is not persisted or resumed across manager
restart. `404 test_unknown_not_resumable` also covers expiry and foreign scope;
it does not establish that the test never ran. Manager shutdown cancels and
joins background tests before stopping owned instances.

Cancellation body:

```json
{"boot_id":"current-boot","expected_revision":1,"idempotency_key":"cancel-test-1"}
```

Read the current instance revision first: the test may already have persisted a
new configuration. Cancellation keys are scoped to the run and caller, with at
most 16 distinct cancellation envelopes retained per run. Repeating the same
normalized cancellation returns the current report **before** CAS; mismatched
payload under that key is 409. The first accepted cancellation checks the current
revision. Repeated/terminal cancellation never resumes or replays work. A 200
cancel response can still say `running` during bounded cleanup; wait for non-null
`ended_at` before treating the reservation as released.

## Lifecycle and ownership

The existing `instance.Maintenance` reservation is held across prepare, apply,
start/load/probe, all requests and cleanup. Native controls and normal inference
cannot interleave; they return busy/protocol errors immediately. There are no
HTTP self-calls or reentrant acquisition of that gate. Owner policy has priority
and revokes the run even during load, streaming or cleanup.

- Stopped and explicitly recovered Router: the manager can apply/start/load in
  one run using existing capabilities. A differing patch is persisted with one
  revision increment; invalid patches never stop a process.
- Same model already ready with unchanged effective parameters: probe and mark
  `reused_ready`, keep its ready identity and **do not unload** the preexisting
  model. A differing patch is `ready_configuration_conflict`, not permission to
  stop a ready model for benchmarking.
- Another loaded model, active inference/maintenance or unclear ownership is
  rejected/conflicted; it is never preempted. An asynchronous backend inventory
  conflict may be recorded as a failed run before any apply/load side effect.
- Normally unload only a model whose load was dispatched by this test, including
  a load whose acknowledgment was lost. Keep tested parameters; do not restore
  old options/models or designate a new daily default.
- If this test started a Router but failed before model load, bounded cleanup
  stops only that test-owned process tree. A preexisting Router is not stopped
  to clean up such a failure. Ordinary start/stop failures remain explicit and
  can require administrator recovery.
- Old permits cannot clean up a new owner epoch. If cleanup cannot be confirmed,
  the report says `failed_not_confirmed_released` or
  `owner_epoch_changed_not_confirmed_released`, with a reason. The owner release
  report remains the authority for host-wide owned-tree release; model unload
  does not mean all GPU memory is free.

Terminal states are `succeeded`, `failed`, `partial`, `cancelled`, with error code
and stage. `partial` means some measured work succeeded or final cleanup failed;
`cancelled` preserves any already-collected partial observations. Terminal means
the test reservation has unwound, **not** that resources were successfully released.

## Measurement and report interpretation

The report identifies the application version/build/commit when available,
source-dirty flag when provided by Go build information, logical engine/model,
configured effective overrides + revision and the tested process/load generation,
fixture ID/digest, generation parameters, timestamps and bounded observations.
Engine/model hashes, runtime binary identity and backend defaults are explicitly
`unknown` when not available; no executable inspection or whole-model hash scan
is done. Omitted override values do not claim measured backend defaults.

For each request:

- TTFT is local manager-client elapsed time at the **first nonempty content
  delta**; role-only, empty content, reasoning, control and usage frames do not
  trigger it. A reasoning-only stream with no nonempty content fails with
  `generation_empty_response` at `[DONE]`, with TTFT still `null`; disabling
  thinking in the request does not relax this parser rule or prove a backend
  honored the policy. It is not an exact tokenizer event or office-client network latency.
- Total duration includes request/stream time. Prompt/completion tokens are exact
  provider usage if supplied, otherwise `null`. No byte-to-token estimate is used.
- Backend prefill/decode milliseconds come only from supplied `timings` fields;
  missing values are `null`. Output/total-time is not presented as decode throughput.
- `warmup`, `initial`, `subsequent` identify request order, not proven cache state.
  `cache_condition` is **unknown**. Requesting `cache_prompt=false` is recorded but
  does not prove cold cache; `reused_ready` is not a cold-load benchmark.
- Only UTF-8 content **length** is retained, not the prompt, response text,
  reasoning, backend error body, private path, credential or ready token.

The report reads the **same P3b shared cache**; it does not launch another sampler
or collector. It retains a baseline and aggregates distinct timestamp observations
during the run. Mean is an arithmetic sample mean, peak an **observed** maximum,
and coverage is valid distinct metric observations divided by distinct metric
attempts (not a fraction of elapsed time). Missing/stale observations do not
become zeros or enter mean/peak. If no distinct sample arrived, no baseline exists,
or all run metrics are unavailable, an explicit gap is shown. A short successful
run can legitimately have no resource sample because the shared idle sampler is
throttled. Metrics never gate control success.

Host/device metrics include other applications. Owned-process metrics are
restricted to this instance and separated by process generation; working-set sums
can double-count shared pages. Contention is only a baseline annotation, not
attribution and not a reason to kill other apps. There is no baseline-subtracted
claim of exact model resource consumption.

CSV contains the same sanitized data as JSON, flattened into deterministic
`field,value` rows, including identity, samples, resource gaps and cleanup. Nulls
remain explicit. Formula-leading cells are escaped for spreadsheet use. Exporting
an active run is a snapshot, not a final result.

## Standard-library client example

`examples/deployment_test.py` (path relative to the repository root) reuses the existing
control client's authentication and redirect protections. It does not issue
credentials, recover a boot, resume owner policy, or retry generations. Supply an
already-authorized credential via your normal secret storage (not in the URL):

```sh
python3 examples/deployment_test.py \
  --base-url http://127.0.0.1:8080 --instance router --model alpha \
  --patch '{"context":2048,"gpu_layers":0,"flash_attention":false}' \
  --measured-generations 3 --format json
```

The example uses `LLAMACTL_CONTROL_KEY`, prints a bounded original submit envelope
for manual reconciliation, polls the accepted ID, and exits nonzero for failed,
partial, cancelled or unknown outcomes. Do not run examples against a real model
without the applicable local resource/test authorization.
