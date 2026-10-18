# Owner resource policy (P2b1 foundation)

This is an intermediate **local new-admission latch**, not complete game-priority resource release and not the final P3 operations protocol. No drain, cancellation, force, process-tree termination, ready token, scoped-controller, resource sampler, UI, or request replay is implemented here.

## Scope and behavior

Every production manager initializes one shared policy before constructing/restoring local instances. Local controlled **and legacy** instances use it for creation, options application, explicit/automatic start/restart, inference, mutable raw proxy requests, and native Router load. Direct `NewChecked` embedding without a policy retains legacy compatibility; a running instance has no policy replacement setter. Remote stubs are not subject to local owner stopping/control.

Pause closes new admission immediately, without waiting for an ordinary instance lease, SSE, process mutex or persistence I/O. Already executing substeps may finish. Reservations are rechecked at late start/commit/forward boundaries; an old reservation cannot become valid again after pause/resume. A substep that has crossed its final admission check is already admitted: this is **not cancellation**, and the check is not a transaction with OS spawn, backend forwarding, files or SQLite.

Ordinary STOP/DELETE/unload and read-only status/health/models remain available, subject to existing controlled-instance `busy` rules. The raw/native proxy read-only exemption requires an unambiguous exact GET path (`/health`, `/models`, `/v1/models`, `/props`, `/slots`, `/metrics`) with an empty raw query. Any query-bearing GET is conservatively classified as unknown/work, including `/props?model=synthetic&autoload=true`, `autoload=1`, `/models?reload=1`, and URL-encoded query forms; no query parsing or special parameter allowlist is used. While paused these requests return 409 `ai_paused` with `retryable:false` before backend access, including native list-models requests. When allowed, legacy queries retain unchanged passthrough; controlled instances still reject unsupported query shapes. Query-free exact reads remain available during pause. Native read-only GET and unload never auto-start a stopped process. Pausing does not stop a running process, drain an SSE, or prove that a backend child has exited. The release field is **always `unreleased_unknown` while blocked**, including when the internal activity count is zero. No claim is made about VRAM release or external programs.

Resume only opens admission after its persistence succeeds. It never loads a stopped model itself or replays a request. Delayed automatic restart and boot-start batches carry their original admission epoch and cannot survive an intervening pause. New requests after resume retain the configured legacy on-demand behavior; controlled instances still require explicit start.

## Temporary owner HTTP contract

`GET /api/v1/resource-policy` and `PUT /api/v1/resource-policy` use the existing management authentication infrastructure. **Both require an explicitly configured management key or a valid existing owner/OIDC management session**, even when `require_management_auth=false`. An inference key, including `allow_all`, cannot mutate policy. An automatically generated fallback management key is not sufficient for this endpoint. No second controller service is created.

GET returns the current snapshot. PUT requires exactly:

```json
{
  "allow_ai": false,
  "boot_id": "boot-id-returned-by-GET",
  "expected_revision": 0
}
```

All three fields are mandatory and non-null. `allow_ai` must be a JSON boolean; `expected_revision` a nonnegative uint64 integer; `boot_id` a nonempty string. Duplicate keys, malformed JSON, trailing documents, wrong types, missing/null fields and bodies above 4096 bytes are rejected (400). Unknown fields, including `release_mode`, are rejected (422). There is no force/release mode in this slice.

Successful GET/PUT returns exactly these policy fields (no activity count, model state, paths or credentials):

```json
{
  "allow_ai": false,
  "admission": "blocked",
  "durable": true,
  "release": "unreleased_unknown",
  "revision": 1,
  "boot_id": "opaque-random-boot-identity"
}
```

When allowed, `admission=allowed` and `release=not_requested`. `boot_id` is newly random for each manager initialization; it is not persisted. Revision advances when a fresh write is **accepted**, including persistence failure. It is not an instance-configuration revision. Stale boot/revision (including stale pause) returns 409 with the current snapshot **before any effect**. Maximum uint64 revision cannot advance and returns 409.

PUT returns 200 after persistence succeeds (or an explicitly configured in-memory update); 500 on persistence failure with a **blocked, nondurable snapshot**. A concurrent newer accepted policy write may supersede a write, returning 409/current snapshot. A pause closes admission before waiting on another writer. A pending resume can never reopen admission over a newer pause. During a pending write GET conservatively reports blocked/nondurable. Authentication/parse/uninitialized-policy errors use the ordinary error envelope, not a policy snapshot.

**No idempotency contract: do not blindly retry PUT**, including after a lost response, timeout, 409 or 500. GET, reconcile the returned boot/revision/policy, then deliberately issue a new owner intent if appropriate. P3 will unify operations/idempotency. Refreshing the boot/revision is not authorization to replay old intent.

Rejected inference returns HTTP 503 `ai_paused`; loading/control and mutable non-inference proxy requests return 409 `ai_paused`. These error JSON responses include `retryable:false`. No Retry-After is advertised. Owner reconciliation is required; conventional P2a busy/status behavior remains while allowed.

## Persistence and startup

The policy file is `<DataDir>/resource-policy.json`, with exactly this versioned schema:

```json
{"version":1,"allow_ai":false,"revision":1}
```

A missing file in an existing configured data directory defaults allowed, **durable=false**; merely starting does not create it. An empty DataDir explicitly chooses in-memory embedded/test mode, always nondurable, with no CWD access. Normal server configuration supplies DataDir. Read/permission/parse/version errors and nonregular policy targets (including symlinks) fail closed and are logged; corrupt data is not automatically overwritten. An explicit fresh owner PUT can repair it.

Writes stage a uniquely named same-directory temporary file, write, sync, close and replace. Write/sync/close/replace errors are propagated; the old destination is never deleted to force replacement. Windows sharing/replace failures stay failures. `durable=true` means the loaded or successfully persisted policy matches the runtime policy, not an OS/filesystem power-loss guarantee. File, runtime, SQLite and process state are not a distributed transaction. After a failed write, the persisted state may still differ; restart behavior follows the actual file and must be reconciled.

Blocked startup validates and registers **side-effect-free stopped descriptors**: no preset write, logger file creation or child start. Invalid persisted instance options remain unregistered/fail-closed with an error log. Resume does not reconstruct or start them automatically. Valid descriptors are discoverable and permit explicit options application/start after owner resume; those actions perform normal preset preparation. Existing raw persisted status is not treated as evidence of a live child.

## Locking and tests

Policy reservation precedes ordinary instance admission. The gate mutex only protects policy/revision/epoch and temporary activity reservation; it never surrounds file/DB/process/network I/O or an entire maintenance lease. Late permit checks are short and never wait for another ordinary operation. A separate writer mutex serializes persistence, with revision validation before writing and before reopening. Internal activity is diagnostic bookkeeping, **not release evidence**.

Synthetic tests cover persistence roundtrip/errors/strict parsing, stale boot/revision, concurrent pause vs pending resume, held SSE and maintenance/process locks, core/direct/manager/HTTP admission, SQLite blocked boot, old epochs, real management/inference/session authentication, and failure snapshots. Linux/mock success is not Windows/GPU or daily deployment acceptance. The pre-existing P2a real-engine failure is not changed by this feature.

The checked-in generated Swagger bundle is not regenerated for this intermediate slice; this document and the HTTP contract tests are the authoritative temporary endpoint specification. Existing P2a Swagger edits are preserved.
