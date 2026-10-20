# Owner resource policy and resource release

The policy is an owner-only local admission latch with **asynchronous, boot-local
resource release**. It is not a general operations platform, resource sampler,
ready token, configuration revision, request replay mechanism, or GPU metric.

## Admission, pause and resume

Every production manager initializes one shared policy before restoring local
instances. Both controlled and legacy local instances check it for creation,
options application, explicit/automatic start/restart, inference, mutable raw
proxy requests, and native Router loading. Remote stubs are not stopped by this
manager's owner policy. Direct embedding without a policy retains legacy behavior.

`PUT allow_ai=false` closes admission **before** ordinary maintenance, process
locks, another policy writer, or persistence I/O. It immediately creates a
background **graceful** release intent, including if persistence subsequently
fails. The HTTP handler waits for policy persistence, **not** for draining SSE,
loading completion, or process exit. Old permits never become valid again after
pause/resume. Already executing ordinary substeps may finish; file/SQLite writes
are not rolled back or made transactional with process changes.

The graceful operation has one **30-second total deadline**, starting when the
pause or explicit graceful request is accepted. It waits for admitted work to
finish naturally, then requests graceful shutdown of every local owned process
tree concurrently (stdin EOF plus Linux group SIGINT or Windows private-console
Ctrl-C). If draining or tree exit misses the deadline, it stays blocked and reports `unreleased_unknown`.
**It never escalates to force on a deadline.** A backend that ignores graceful
shutdown may keep running until a deliberate owner immediate request.

An explicit **immediate** release preempts graceful release, cancels manager-side
admitted inference/model-loading requests and health/loading waits, and requests
termination of the owned local OS trees. Its total completion budget is
**10 seconds**; an OS API accepting termination is not itself exit evidence.
It does not acquire ordinary instance maintenance leases or wait for an SSE-held
lease. Failures or missing evidence remain visible, not fabricated success.
Ordinary Stop/Delete/unload retain their existing admission and authorization
rules; inference clients have no owner-force authority.

Resume invalidates old release work, persists the allowed policy, and only then
opens admission. It does not autoload, replay, or restore canceled requests.
A new pause also invalidates previous release work. Pending automatic restarts
and startup batches retain their original admission epoch and cannot survive
pause/resume. New requests after resume retain configured legacy on-demand
behavior; controlled instances still require explicit start.

## Owner HTTP contract

All three routes require an **explicitly configured management key or valid
existing owner/OIDC management session**, even if `require_management_auth=false`:

* `GET /api/v1/resource-policy`
* `PUT /api/v1/resource-policy`
* `POST /api/v1/resource-policy/release`

An inference key (including `allow_all`), missing authentication, and an
automatically generated fallback management key do not grant this authority.
No new authentication system or scoped controller is introduced.

PUT requires exactly these non-null fields:

```json
{"allow_ai":false,"boot_id":"boot-id-from-GET","expected_revision":0}
```

`allow_ai` is a JSON boolean; `boot_id` a nonempty string;
`expected_revision` a nonnegative uint64 integer. All fields are required.
Revision advances for every accepted PUT, **including persistence failure**.
Stale boot/revision or an exhausted uint64 revision returns 409/current snapshot
before any effect. A newer accepted writer can supersede an earlier writer,
which returns 409/current snapshot. A pending resume cannot reopen over a newer
pause. While a write is pending, GET conservatively reports blocked/nondurable.

PUT returns 200 after successful persistence (or explicit in-memory operation),
or 500 with a blocked/nondurable snapshot on persistence failure or an unconfirmed
old OS dispatch that prevents safe resume. The latter is checked before writing
allowed. **PUT is not idempotent and must not be blindly retried**, including after a timeout, lost
response, 409, or 500. GET, reconcile, and deliberately issue a fresh owner intent.

POST requires exactly these non-null fields:

```json
{
  "boot_id":"boot-id-from-GET",
  "expected_revision":1,
  "request_id":"owner-release-001",
  "mode":"immediate"
}
```

* `mode` is exactly `graceful` or `immediate`.
* `request_id` is 1–128 ASCII letters, digits, `.`, `_`, or `-`.
* Only the current **blocked** policy may request release. Stale boot/revision or
  allowed policy returns 409 without cancellation or termination.
* Accepted new operations and idempotent repeats return **202**, using the same
  policy snapshot envelope as GET/PUT. Release does not increment policy revision.
* Same ID and same normalized fields returns the **original operation's** status,
  timestamps and error, without re-running termination. Same ID with different
  revision/mode conflicts. JSON whitespace/key order are not part of identity.
* CAS is checked first: a repeat from an older policy revision is still stale.
  Repeating an earlier ID in the same current revision can return its superseded
  operation even when another operation is now current; GET shows the current one.
* An active immediate operation cannot be downgraded by a new graceful POST (409).
  A fresh immediate ID can supersede another operation. A new pause is a new
  policy intent and always creates graceful release.
* The ledger holds **1024 explicit IDs per manager boot**, including failed and
  superseded operations. **No expiry and no eviction**. At capacity, new IDs
  receive **503/current snapshot** until a fresh manager boot; existing IDs remain
  readable. Pause/resume does not reset the ledger. Automatically generated pause
  IDs use the reserved form `pause:<revision>` and do not consume explicit slots.
  There is no cross-boot exactly-once guarantee or durable release-operation log.

Both mutation bodies are limited to **4096 original bytes**. Missing/null fields,
wrong types, duplicate keys, invalid JSON, trailing documents and oversized bodies
return 400; unknown fields return 422. PUT still rejects `release_mode` and other
release fields. Authentication/parse/uninitialized-policy failures use the existing
error envelope. Accepted/CAS/capacity/persistence results use policy snapshots.

Example snapshot while draining:

```json
{
  "allow_ai":false,
  "admission":"blocked",
  "durable":true,
  "release":"draining",
  "revision":1,
  "boot_id":"opaque-random-boot-identity",
  "release_operation":{
    "id":"pause:1",
    "mode":"graceful",
    "policy_revision":1,
    "started_at":"2026-01-01T00:00:00Z"
  }
}
```

Allowed means `admission=allowed`, `release=not_requested`, and no operation.
Blocked release states are `draining`, `terminating`, `released`, `failed`, or
`unreleased_unknown`. The optional operation also has `ended_at` (UTC RFC3339,
possibly fractional seconds) and `error_code` when applicable. Stable codes are
`superseded`, `drain_timeout`, `release_timeout`, `termination_failed`,
`port_ownership_unknown`, `external_runtime`, `tree_ownership_unsupported`, and
`release_unavailable`. Raw OS errors, PIDs, paths and credentials are not exposed
in these snapshots. An embedded gate without a manager reports unavailable/
unknown, never successful release.

## OS ownership, evidence and synchronization

Windows uses private non-breakaway **Job objects**, restricted inherited-handle
lists, and `PROC_THREAD_ATTRIBUTE_JOB_LIST` with `CreateProcess`. An internal
self-reexec helper is assigned to a supervisor Job **during creation** and receives
`CREATE_NEW_CONSOLE`. Before launching a backend it verifies that its console
contains only itself. Backend creation inherits that supervisor membership and
explicitly adds a backend-only child Job, also before backend code can run. Held
handle membership is checked before successful publication. There is no
assign-after-Start fallback, manager console attachment/detachment, shared-manager
console broadcast, or PID selection for termination.

The helper entry runs before application main/config/database/network startup,
in both product and test executables. A fixed internal argument alone is not a
launch capability: a validated anonymous mapping, four private event handles,
versioned bounded startup description, pipe stdio, Job membership and private
console are also required. Helper argv contains no backend configuration. The
helper inherits those five control handles, three stdio handles, and a temporary
assign/query-only backend Job capability. It closes that capability before ready;
it cannot terminate the Job. The backend inherits **only the three stdio handles**,
not any Job/control handle or bootstrap environment entry. The supervisor Job
handle never leaves the manager, retaining its last-handle kill-on-close boundary
through startup, normal operation and quarantine.

Windows graceful still closes stdin, but also sends **one real CTRL_C_EVENT** via
that helper's isolated console. Ctrl-C is explicitly enabled before backend
creation. Only after the backend exists does the helper enable its own ignore
attribute; that per-process change does not affect the existing backend or its
later descendants. No CTRL_BREAK/default-termination substitute is used.
One tree-generation latch covers different release IDs and ordinary Stop; an
uncertain request is not blindly resent, because a backend's second Ctrl-C may
force-exit. An arbitrary backend is still responsible for implementing graceful
handling and for preserving or coordinating its descendants' console lifecycle.

The manager obtains the backend's process handle directly from its held helper
handle, not by opening a possibly recycled PID. Immediate retains
`TerminateJobObject` on the captured scopes. The helper is asked to quit only after
the backend Job has zero active processes **and an empty Job process list**, and
the held backend root is wait-signaled. This does not assume that the supervisor
Job has exactly one process; console infrastructure is not a backend descendant.
Final completion requires both Job scopes empty, both held root/helper process
objects wait-signaled, and their exit-code observations. Accounting removal or a
GetExitCodeProcess value alone is not process-exit proof. No process name/PID is
used to exclude a member or to select a termination target. A nonzero helper exit
is also retained as a lifecycle error, not hidden behind a zero backend exit.

Helper readiness has a 3-second budget; its own startup watchdog is 5 seconds.
Failed startup requests termination of the exact, unpublished Job scopes and waits up to
2 seconds. If exit cannot be proved, the failed generation remains owned and
quarantined, blocking replacement Start/retirement; owner immediate can still stop
it. Startup pipes are closed on failure, and Job/process/IPC handles are retained
until actual exit evidence, rather than dropping a live ownership boundary.

The original fixed Windows b11717 qualification observed natural SSE completion
without backend exit on EOF: its 30-second timeout remained blocked/unknown. The
subsequent native synthetic RED also proved missing Ctrl-C receipt, with the real
handler fixture self-check passing. Those failures remain evidence for the old
EOF-only implementation. The new console implementation requires independent
Windows qualification and fixed-engine verification; neither Linux checks nor
later explicit-force cleanup certify graceful backend completion. The first
console-helper candidate subsequently produced one real Ctrl-C and three normal
completion receipts but failed helper/Job retirement; a Stop test's later force
fallback was not natural-exit proof. That native failure is retained separately.
The scoped-Job and held-process-wait correction still requires native qualification.

Linux starts a new inherited process group and makes the manager process a child
subreaper (no system configuration change). The sole tree waiter uses group-scoped
`wait4`/`waitid` to reap root and adopted descendants, not `/proc`/PID enumeration.
Reaping, ownership verification and group signaling are synchronized; local group
creation is also serialized against the final reap/probe so an old group cannot
be mistaken for a newly spawned generation. Native foreground backends and their
descendants must preserve the inherited group; daemonizing/detaching backends are
not supported ownership targets. Linux groups are not a hostile-code sandbox.
Other OSes retain ordinary lifecycle behavior but owner release reports unknown
when tree ownership is unsupported. Docker/external runtimes are also unknown:
the manager still stops its owned local launcher tree, but does not infer ownership
or release of daemon-managed containers.

Policy admission uses only a short mutex. A separate revocable effect boundary
orders OS spawn/publication and each actual shutdown syscall with resume. Resume
cannot open admission until an already-authorized signal has returned, but it
never waits there for process exit. A stale ticket cannot issue a later signal.
Generation-specific tree/pipe/context references prevent a delayed worker from
signaling a replacement process. Pause never waits on this effect boundary.
Process/health/exit waits use deadlines outside the admission mutex and outside
the shutdown-syscall boundary. Deletion cannot retire an instance with a live
owned tree merely because its status was changed to stopped/failed.

The Windows helper handshake bounds synchronous dispatch observation to 2 seconds,
clamped to the owner operation's remaining deadline. An unclaimed request is atomically canceled. A claimed request requires the actual
`GenerateConsoleCtrlEvent` return state or dispatcher-exit evidence, not just an
IPC enqueue/ack. On uncertainty the manager may stop **only that exact helper** and
wait up to 1 second for exit, also clamped to the remaining deadline; it does not
kill the backend or close a live Job as a graceful fallback. If completion/cancellation/exit is still unproved, an internal
pending-effect guard outlives the bounded call, without holding the policy mutex
or effects token. Explicit immediate and a new pause remain usable.

Resume checks that guard **before persisting allowed** and again before reopening.
An unresolved guard returns the existing PUT failure response (500 with a blocked,
nondurable snapshot and the accepted new revision), without writing allowed or
opening admission. No endpoint, JSON field, or automatic resume is added. The
owner must GET/reconcile and deliberately issue a fresh resume after completion
or dispatcher exit is known. As with other PUT failures, do not blindly replay an
old revision. A superseding pause retains the existing revision/persistence rules.
During a pending resume write, an ordinary Windows Stop's control effect is rejected
rather than introducing a new uncertain dispatch between the guard check and
reopen; it does not hold the effects token around persistence. This guard is
boot-local and is not a replay mechanism for old shutdown work.

`released` requires tree-exit evidence **and** port-listener evidence for the
captured generation/current descriptor. On Windows, `GetExtendedTcpTable` is
queried for both IPv4 and IPv6; any LISTEN row on the port is conservatively
rejected, regardless of local address or owner. A closed connection's TIME_WAIT
row is not a listener and is not evidence that an exited Job is still alive.
Windows therefore does not equate a failed temporary rebind with a live listener.
Missing, failed, malformed or canceled table observations stay unknown; a failed
Dial or empty activity count is never substituted for OS evidence. Non-Windows
platforms retain the existing bind check. The TCP table is read only for evidence,
never to select PIDs to terminate.

An unexpected listener, uncertain OS evidence, or termination failure is
unknown/failed, never permission to kill an external port owner. Port evidence is
at verification time: it is neither a reservation against another program binding
later nor a promise that immediate rebinding will succeed while TIME_WAIT or an
OS reservation remains. A zero activity count only allows graceful termination
to begin; it is **not** release evidence. These are manager-owned process-tree
facts, not whole-machine GPU/VRAM measurements or a promise of instantaneous
driver memory reclamation.

## Proxy behavior and interruption

Local blocked inference returns 503 `ai_paused`; loading/control and mutable
non-inference proxy requests return 409 `ai_paused`, with `retryable:false` and
no Retry-After. Owner reconciliation is required.

Exact query-free GET `/health`, `/models`, `/v1/models`, `/props`, `/slots`, and
`/metrics` remain read-only exemptions. Any query-bearing GET is conservatively
work, including encoded Router/autoload parameters. Native read-only GET and
unload never auto-start a stopped process. Controlled request restrictions and
the bounded HTTP/1 control-rejection settlement remain unchanged.

Naturally completed SSE is forwarded unchanged, including a backend's `[DONE]`.
Owner immediate cancels outbound request contexts. Before response headers this
can produce an ordinary proxy error; after streaming has begun Go's reverse proxy
aborts the HTTP response/stream (HTTP/1 clients generally observe truncated framing
or unexpected EOF; HTTP/2 uses transport stream interruption). The manager does
**not** append `[DONE]`, fabricate a successful completion, or add a new SSE event
protocol. Loading calls are canceled too. HTTP cancellation alone does not prove
engine work stopped; separate tree-exit verification is required.

## Persistence and blocked boot

The policy file remains `<DataDir>/resource-policy.json` with exactly:

```json
{"version":1,"allow_ai":false,"revision":1}
```

Boot identity, release operations and idempotency records are not persisted.
Missing policy in an existing configured directory defaults allowed/nondurable;
startup does not create the file. Empty DataDir is explicit nondurable embedded
mode, with no CWD access. Read/permission/parse/version errors and nonregular
policy targets (including symlinks) fail closed and are logged. Corrupt data is
not automatically overwritten; a fresh deliberate owner PUT may repair it.

Writes stage a uniquely named same-directory file, write/sync/close and replace.
Errors propagate; the old target is never deleted to force replacement. Windows
sharing/replacement failures remain failures. Durable means persisted policy
matches runtime policy, not an OS power-loss guarantee or transaction with SQLite
and processes. Failed pause still launches release and stays blocked/nondurable;
the actual old file determines policy after restart and must be reconciled.

Blocked boot restores validated, side-effect-free **stopped descriptors**, with no
preset/logger creation or child start. Invalid instance options stay unregistered.
It reports `unreleased_unknown`, without an operation, and never resumes an old
release. Persisted status is not live-process evidence. Resume alone neither
reconstructs nor starts models; explicit start/options application performs normal
preparation. The manager never targets previous-boot/unowned processes by name/PID.

## WebUI owner controls

The dashboard exposes a prominent **暂停 AI / 恢复 AI** control and a separate
**立即释放…** action with explicit confirmation and cancel. Pause sends only
`allow_ai`, `boot_id`, and `expected_revision` to PUT: the backend's ordinary
pause already starts graceful release. The UI does not invent a PUT request ID
or send an extra release POST for pause. Immediate POST uses a fresh bounded
random `request_id`, `mode=immediate`, and the last trusted GET precondition.
Resume changes admission only; it never starts a model or replays canceled work.

Admission (allowed/blocked), persistence (durable/nondurable), and actual release
(not requested/draining/terminating/released/unknown/failed) are displayed
separately. HTTP 202 is not interpreted as released, and process release does not
promise immediate driver VRAM reclamation. Hardware observations are shown in a
separate [read-only resources](resources.md) section.

Every mutation disables duplicate clicks and is followed by GET. Conflict,
failed response, timeout or network-unknown results are **not retried**. The error
remains visible after reconciliation, even if the current state happens to match
the intent. A failed or stale read cannot authorize another write. Only a fresh,
visible-page, successfully parsed GET with an exactly representable CAS revision
can authorize a new deliberate action. The UI rejects revisions beyond JavaScript
safe integer precision rather than round a uint64. A confirmation tied to an old
boot/revision must be reopened after policy changes.

Policy reads poll every two seconds only while visible; requests are bounded,
old responses are ignored, and hiding/unmount cancels reads and invalidates write
authority. Writes are bounded client-side too; aborting a write is not proof that
the server did not apply it. A later visible GET is required to reconcile. A
separate owner-only 401 is shown as a capability denial without dispatching the
global logout event: anonymous/auth-disabled compatibility reads must not create
an automatic login/logout loop. Existing authentication storage and management
sessions are reused; no new controller or authentication system is introduced.

## Verification boundary

Synthetic tests exercise strict HTTP/auth/session behavior, real loopback SSE and
native loading cancellation, CAS/idempotency/capacity, persistence failure and
blocked restart, held maintenance, drain and termination timeouts without force,
resume/effect races, new generations, concurrent local instances, remote exclusion,
startup/termination failures, parent/child/grandchild exit and an external sentinel.
Native Windows Job-association regression source is included for separate execution.
Linux tests do **not** certify Windows APIs/sockets, fixed-engine Router behavior,
real-model quality/performance, GPU release, or deployment. Those require separate
controlled target-environment verification.

The checked-in generated Swagger bundle is not regenerated for this extension;
this document and HTTP contract tests specify these endpoints. Existing Swagger
edits for the controlled-admission foundation are preserved.
