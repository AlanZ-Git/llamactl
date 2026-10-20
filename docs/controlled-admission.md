# Controlled-instance admission foundation (P2a)

This is an opt-in **admission foundation**, not the final controlled protocol.
Owner pause and asynchronous graceful/immediate release are documented separately
in [resource-policy.md](resource-policy.md). Owner immediate is a privileged,
revocable process-tree operation, not an ordinary maintenance admission bypass
available to inference clients. There are no ready tokens, instance-configuration
revisions, controller scopes, general operation queues or new UI.
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
unwinding. Two inference requests may overlap. Graceful owner pause leaves existing
streams intact; immediate owner release cancels outbound contexts (including native
loading). An interrupted stream is aborted, not completed with synthetic `[DONE]`;
normal backend SSE bytes and natural completion remain unchanged.

Maintenance is exclusive: Update/Delete/Start/Stop/Restart, direct lifecycle and
options APIs, prepared-option commits, and native Router load/unload all enter
this boundary. A typed admission error maps to HTTP **409**, JSON
`{"error":"busy","details":"..."}`, without queueing when controlled use or
maintenance is active. The HTTP/1 control-body cleanup below may precede delivery
of that already-decided error. No queue, stop, preset preparation, port allocation or
options replacement occurs on that busy path. Creation also reserves the name
boundary and holds maintenance while publishing/persisting the instance. Deleted
instances are retired so stale references cannot restart them.

The checked maintenance capability has private instance identity and active
lifetime validation; internal operations reuse it rather than reacquiring the
gate. A separately scoped operation mutex protects misuse of the capability,
not admission or status queries. File/process/network/DB work never holds the
gate mutex. The separate owner policy closes new admission before this ordinary
gate and does not wait for an SSE-held maintenance/use lease. `PrepareOptions`
callers must always `Close`; direct
successful `Commit` releases its owned lease. Manager-owned candidates retain
the surrounding maintenance lease through restart and persistence.

P1 candidate validation and preset preparation still precede stopping the old
process. Files, SQLite and processes are **not** one transaction. A later commit,
start or persistence failure can leave changed/stopped state and is returned as
an error, not rolled back or reported as successful. This foundation does not
add crash recovery. Ordinary manager shutdown still uses the Stop gate: active
controlled work yields a logged busy error, not an implicit owner-immediate
request. Deliberate owner release is separate: pause starts graceful drain with
no deadline escalation; authenticated immediate cancels inference/loading and
terminates owned local trees without waiting for ordinary maintenance. Release
requires OS/tree and port evidence, not this gate's use count or instance status.
See the owner contract for deadlines, persistence failures and resume linearization.

## Proxy policy

All production proxy handlers eventually use `Instance.ServeHTTP`; rejection
writes the response before the HTTP handler returns (never an empty HTTP 200).
For the maintenance routes below, a local rejection is deferred until the adapter's
permits have unwound, then delivered by the enclosing HTTP boundary.

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

## Bounded HTTP/1 control-rejection settlement

A server-side 409 log does not by itself prove that the client received the JSON.
In particular, Go's automatic response-time request-body drain skips requests with
`Connection: close`. The production router therefore settles **only local
`busy` / `ai_paused` admission rejections** on these maintenance entrypoints:

* native POST `/api/v1/llama-cpp/{name}/models/{model}/load` and `/unload`;
* exact POST `/models/load` and `/models/unload` through the instance raw proxy;
* instance Create, Update, Delete, Start, Stop and Restart management routes.

This is not broad request-draining middleware. Authentication still runs first;
backend responses (including backend 409s), unrelated errors, arbitrary proxy
paths, uploads, inference and HTTP/2 do not opt into settlement. The existing
status, business code, details and `retryable: false` fields are unchanged.
Owner pause still allows unload/stop according to the existing admission rules.

The route boundary defers only the already-decided rejection response. The
handler and its normal defers finish **before any cleanup read**, so no rejected
request's instance lock, maintenance lease or owner permit is retained while
waiting for the client. Cleanup never retries admission or invokes the backend.
Successful requests do not have their writer/body wrapped, buffered or pre-read;
ordinary decoding, original proxy bytes and SSE forwarding/permit lifetime remain
unchanged. Create/Update still decode before admission: this cleanup does not
bound their existing initial decoder work or change invalid-body precedence.

Cleanup budgets and fallback:

* Only known-length, non-chunked bodies of **at most 4 KiB**, with no `Expect`
  header, qualify. This comfortably covers small model-name controls without
  turning cleanup into a large configuration/upload drain. The declared length
  includes all whitespace. It is a cleanup threshold, **not** a new accepted-body
  limit or a change to the controlled proxy's existing 16 MiB validation limit.
* Additional application reads discard at most the declared length (and never
  more than 4 KiB), using a fixed buffer and one absolute **100 ms read deadline**.
  The deadline is not renewed by trickle traffic. There is no background reader,
  goroutine race, unbounded `ReadAll`, or application call to `Body.Close`.
* Complete small bodies are consumed before writing the original error, even for
  `Connection: close`. Successful cleanup clears its read deadline; keep-alive and
  already-pipelined requests may continue under normal server timeout handling.
* Unknown/chunked/oversized/expect-continue bodies and unavailable read deadlines
  cause **zero cleanup reads**. Cancellation, no progress, truncation, disconnect,
  timeout or a failed deadline reset also select the close fallback. Rejection
  remains side-effect free; delivery is not guaranteed for these fallback cases.
* Fallback emits the same JSON with close-delimited HTTP/1 framing, requests
  `Connection: close`, bounds the final flush with a 100 ms write deadline when
  supported, then hijacks and closes the HTTP/1 connection. Merely setting the
  response's close header is insufficient: Go's `finishRequest` can still call
  the original body's `Close` and drain up to another 256 KiB. Explicit connection
  teardown avoids that extra read/close and prevents unsafe connection reuse.

The native Go HTTP/1 writer supports deadlines, flush and hijack; wrappers must
preserve those capabilities directly or through `Unwrap() http.ResponseWriter`
(as the production logging chain does). A custom/opaque writer exposing neither
transport deadlines nor hijack receives a close response without any application
body read/close; its host must provide bounded connection teardown. No helper can
impose transport deadlines through a writer that hides all transport access.
This integration limitation is not a reason to drain without a deadline.

Local regressions cover exact consumption and byte/time boundaries, unsupported
writers, incomplete/trickling/disconnected bodies, real loopback TCP close and
keep-alive/pipelining, active SSE rejection and natural EOF release, owner pause,
auth, decoder leftovers, and unchanged accepted slow/large bodies. These tests
are portable Go fixtures, not proof of Windows socket behavior or real-model
success. No RST initiator is inferred from application logs.

## Local verification boundaries

Focused tests use channel barriers, real manager/SQLite/router/auth chains and
httptest streaming engines, plus a synthetic stdin-exit helper executable for
controlled Start/Restart/Stop. They exercise SSE-held control rejection, read-only
visibility, maintenance versus inference, cancellation/error release, raw proxy
and autoload rejection, mode transitions, restore, candidate defaults and
legacy compatibility. Full Go/race/vet/build results are recorded separately
outside the repository. No model/GPU, deployment, system changes or remote
operations are part of this slice.
