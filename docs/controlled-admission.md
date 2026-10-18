# Controlled-instance admission foundation (P2a)

This is an opt-in **admission foundation**, not the final controlled protocol.
Owner new-admission pause is documented separately in [resource-policy.md](resource-policy.md);
it does not drain work or terminate processes. There are no ready tokens,
instance-configuration revisions, controller scopes, operation queues,
asynchronous operation resources or new UI.
Running does not mean model-ready, and stopped does not prove VRAM release.

## Configuration

`instance.Options.controlled` is a JSON boolean, persisted in the existing
options JSON/SQLite store. Missing or `false` retains legacy mode; `null` is
invalid. `environment_policy: "isolated"` by itself does **not** enable controlled
mode. PUT still replaces options; it is not PATCH. Retain `controlled: true` in
replacement bodies, or explicitly disable it while stopped.

Controlled instances require local native `llama_cpp` and the P1 isolated
routing/authentication checks (including loopback binding, no Docker, external
presets, API prefix or external key file). Before preparation, omitted/null
`auto_restart`, `on_demand_start`, and `idle_timeout` receive candidate defaults
`false`, `false`, and `0`, irrespective of global defaults. Explicit `true` or
nonzero idle timeout (including negative numbers) is rejected, not clamped.

A model-less server is a Router and must explicitly configure
`no_models_autoload: true` and `models_max: 1`. Single-model instances do not need
these Router flags. `models_autoload: true` and nonzero `sleep_idle_seconds` are
forbidden in either mode. Controlled instances reject all raw backend argv and
`extra_args` (including unknown backend options captured there), plus explicit
`LLAMA_*` environment overrides other than the P1-validated host/API key.
Presets cannot contain Router/autoload/sleep options, including b11717's
preset-only `load-on-startup` (separate from `models_autoload`) and its internal
`__PRESET_LOAD_ON_STARTUP` identifier. Case and dash/underscore variants are
rejected even with false/null values, during candidate validation before preset
preparation, port changes or stopping the old instance. These deliberately
conservative restrictions avoid guessing flag aliases, argument scope or engine
precedence. Model parameters in presets are otherwise retained, including
literal zero/false; no generic protocol rewrite or engine modification is made.
Administrators remain responsible for the configured executable and local files.

Example Router options (synthetic configuration):

```json
{
  "controlled": true,
  "environment_policy": "isolated",
  "backend_type": "llama_cpp",
  "backend_options": {
    "no_models_autoload": true,
    "models_max": 1
  },
  "preset_ini": "[example]\nmodel = example.gguf\n"
}
```

Enabling **or** disabling controlled mode requires `stopped`; otherwise HTTP 409
`maintenance_required`. The transition itself excludes newly admitted proxy
requests and requires no outstanding use. A running legacy instance cannot be
converted to circumvent its active inference. Legacy autostart/eviction sequences
also reserve use before any side effect, preventing a concurrent mode transition.
Failed persisted controlled
configuration is not registered or automatically started. Valid restored
controlled instances always start in `stopped`, regardless of persisted status.
They are excluded from idle/LRU eviction and automatic restart/startup paths.

## Atomic admission and errors

One gate per instance checks mode, status and lease availability under the same
short-held mutex. Legacy maintenance retains its existing manager/process
serialization (not a new busy policy); its leases are tracked so a controlled
transition cannot overlap it. Inference obtains a shared use permit before request validation
and backend forwarding. The permit remains held until forwarding returns,
including SSE EOF, cancellation, validation errors, backend errors and panic
unwinding. Two inference requests may overlap.

Maintenance is exclusive: Update/Delete/Start/Stop/Restart, direct lifecycle and
options APIs, prepared-option commits, and native Router load/unload all enter
this boundary. A typed admission error maps to HTTP **409**, JSON
`{"error":"busy","details":"..."}`, immediately when controlled use or
maintenance is active. No queue, stop, preset preparation, port allocation or
options replacement occurs on that busy path. Creation also reserves the name
boundary and holds maintenance while publishing/persisting the instance. Deleted
instances are retired so stale references cannot restart them.

The checked maintenance capability has private instance identity and active
lifetime validation; internal operations reuse it rather than reacquiring the
gate. A separately scoped operation mutex protects misuse of the capability,
not admission or status queries. File/process/network/DB work never holds the
gate mutex. This leaves room for a later priority-block latch, but does **not**
provide owner pause today. `PrepareOptions` callers must always `Close`; direct
successful `Commit` releases its owned lease. Manager-owned candidates retain
the surrounding maintenance lease through restart and persistence.

P1 candidate validation and preset preparation still precede stopping the old
process. Files, SQLite and processes are **not** one transaction. A later commit,
start or persistence failure can leave changed/stopped state and is returned as
an error, not rolled back or reported as successful. This foundation does not
add crash recovery or guaranteed engine cancellation. Manager shutdown also uses
the ordinary Stop gate: active controlled work yields a logged busy error, not
forced termination or a promise that every child exited. Drain work before
shutdown; force/owner-stop semantics are deferred.

## Proxy policy

All production proxy handlers eventually use `Instance.ServeHTTP`; rejection
writes the response before returning an error (never an empty HTTP 200).

* Controlled requests never auto-start. Stopped instances return HTTP **503**
  `instance_not_running` with use-running-only semantics, even for privileged
  credentials. Explicit Start remains available under maintenance admission.
* Exact POST `/models/load` and `/models/unload` are maintenance, including via
  the native model handlers and raw instance proxy. The JSON body accepts only
  a nonempty string `model`. The existing model path parameter is not an
  implicit substitute for this body.
* Shared inference accepts the fixed completion/chat-completion, embedding,
  rerank, infill, tokenize/detokenize and apply-template paths, including the
  corresponding supported `/v1` paths. `/v1/*` is not a blanket permission.
* GET `/health`, `/models`, `/v1/models`, `/props`, `/slots`, `/metrics` may be
  observed while maintenance runs, provided the process is still running.
  These read-only requests do not reserve inference use or auto-start a process;
  backend errors during a stop remain possible. Management instance GET/list is
  independently observable. The backend WebUI root is not allowlisted.
* Unknown paths, methods, encoded-path ambiguity, query parameters, compressed
  bodies and other mutable raw operations (props, slot restore, etc.) are
  rejected with HTTP **400** `unsupported_controlled_request`.
* POST bodies must be one JSON object, at most 16 MiB **in the original body**
  (including whitespace). Duplicate keys at any depth and nested autoload/Router
  overrides are rejected, even `false` or `null`. Raw proxies forward the original
  JSON bytes after validation. The OpenAI `/v1` aggregator first resolves the
  instance and checks permission, then validates the original bytes with the same
  validator before re-encoding with the backend model name. It holds shared use
  through rewriting and forwarding, so mode changes cannot race normalization;
  final `ServeHTTP` admission/validation remains in place. Valid model-prefix
  replacement and ordinary zero/false parameters are retained. Legacy JSON
  normalization and request formats are not newly restricted by this policy.
* The OpenAI aggregator still reads and parses the complete body to determine
  model routing before applying controlled validation. The 16 MiB rejection is
  a controlled protocol limit, **not** a bound on initial allocation or a complete
  allocation-DoS defense. Invalid JSON that cannot be routed retains the existing
  `invalid_request` response.

Authentication remains the existing native management/inference policy. This
is not a new authorization scope: native Router handlers still use their
existing permissions. Do not expose the backend directly as a substitute for
manager admission. Engine flags and behaviors require fixed-build integration
validation before deployment; mock/local Go tests do not certify Windows/GPU,
actual model readiness, autoload suppression in a particular engine build,
or completion of engine work after a disconnected HTTP request.

## Local verification boundaries

Focused tests use channel barriers, real manager/SQLite/router/auth chains and
httptest streaming engines, plus a synthetic stdin-exit helper executable for
controlled Start/Restart/Stop. They exercise SSE-held control rejection, read-only
visibility, maintenance versus inference, cancellation/error release, raw proxy
and autoload rejection, mode transitions, restore, candidate defaults and
legacy compatibility. Full Go/race/vet/build results are recorded separately
outside the repository. No model/GPU, deployment, system changes or remote
operations are part of this slice.
