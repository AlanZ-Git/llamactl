# N1 stdlib evaluator / direct service client

No dependencies, SSH, daemon, engine download or automatic inference retries.
Server SQLite is execution authority; checkpoints are private, atomic mode-0600 caches.
All examples below are synthetic. Never put real keys, business messages, outputs or
private runtime logs in this directory or Git.

## Runnable isolated real HTTP fixture

From the repository root, using the approved Go toolchain and caches:

```sh
go test ./pkg/server -run '^TestN1PythonHTTPFixture$' -count=1 -v -timeout=120s
python3 -m unittest discover -s tools/evaluator/tests
```

The Go test reuses `newN1Fixture` and the existing synthetic Router process. It creates
an isolated SQLite DB, a tiny synthetic GGUF, random loopback ports and a test credential;
it never starts the product against daily state or loads a real model. It invokes
`synthetic_http.py <fixture-origin> <private-temp-directory> <synthetic-scoped-key> <synthetic-evaluator-key>` itself and waits for exit.
The script exercises **Client discover/run/report/use/infer/release against real server
routes**, including repeat report, checkpoint reconciliation, ACKed body preservation
and response boot/revision/recipe/process/load/prompt identity checks. The direct-use
Client is separate from the evaluator instance; no evaluator service is required.
A `python3` unavailable skip is not successful Python integration verification.

## Direct client

The manager must already be configured, authorized and recovered. Choose a specific
published recipe ID/version from descriptions; names are not stable identities.
Minimum direct-client key: `controller=false`, `permission_mode=per_instance`, only the
chosen instance's inference permission (`can_start=false`, `can_evict=false`), plus N1
`discover`, `service_use`, `inference` on that instance. Envelope lookup uses `GET inventory`
(`boot_id/revision`), not legacy controller status. The HTTP fixture asserts both legacy
control and operations remain denied to this key. Evaluation additionally needs `evaluate`;
recipe writing, publication and observation remain separately granted capabilities.

```sh
python3 tools/evaluator/evaluator.py --url http://127.0.0.1:8080 \
  --instance router --key-file /private/n1.key discover
python3 tools/evaluator/evaluator.py --url http://127.0.0.1:8080 \
  --instance router --key-file /private/n1.key use RECIPE_ID 1 --checkpoint /private/use.json
python3 tools/evaluator/evaluator.py --url http://127.0.0.1:8080 \
  --instance router --key-file /private/n1.key infer USE_ID /private/messages.json
python3 tools/evaluator/evaluator.py --url http://127.0.0.1:8080 \
  --instance router --key-file /private/n1.key release USE_ID
```

`messages.json`: `[{"role":"user","content":"synthetic test"}]`.
The client obtains the current use identity, sends exact prompt version/digest and
ready/use headers, verifies response identity, and does **not** retry inference.
Repeating `use` with the same checkpoint queries the original switch rather than
creating another use. An expired/terminal use is not resurrected. Use a new explicitly
chosen checkpoint only for a genuinely new authorized use.

## Bounded evaluation

`synthetic_http.py` contains a complete executable plan example, constructed from
real inventory verification. For a prepared plan, keep the same shape:

- `execution`: candidates with exact artifact ID/digest, complete deployment,
  request parameters/system prefix/template version; wall/attempt/cleanup seconds,
  requests/tokens/bytes budget, parameter ranges, fixture version, synthetic purpose,
  real `notice_ref` and elapsed `not_before` for an actually authorized run.
- `fixtures`: candidate ID to fixture ID, messages and explicit `synthetic_contains`
  oracle. The directional stop oracle uses declared coverage, bounded request budget,
  or configured patience/epsilon; it makes no business/performance recommendation.

```sh
python3 tools/evaluator/evaluator.py plan /private/plan.json
python3 tools/evaluator/evaluator.py --key-file /private/n1.key \
  run /private/plan.json --checkpoint /private/evaluation.json
python3 tools/evaluator/evaluator.py --key-file /private/n1.key status RUN_ID
python3 tools/evaluator/evaluator.py --key-file /private/n1.key cancel RUN_ID
python3 tools/evaluator/evaluator.py --key-file /private/n1.key \
  report --checkpoint /private/evaluation.json --upload
```

`plan` only prints **prepared**; it sends no notice. A transport exception is ambiguous:
resume with the same plan/checkpoint. The client queries the accepted boot/key and
attempt sequence first. It never changes the accepted boot or automatically replays
inference. Server receipt identity/digest is checked before keeping or reusing local
output. Body is saved **before ACK**. Lost output remains unavailable, not reconstructed;
only locally present, digest-verified and scored output can be shortlisted. Reports are
external synthetic-only findings, never business PASS or capacity evidence.

Server defaults: bounded request body 1MiB; output 1MiB/body and 16MiB/run; default
in-memory delivery TTL 5 minutes. Retained outputs require explicit `retain_outputs`.
Owner pause, capability revocation, hard deadlines and cleanup barriers remain server
authority if the Python process disconnects. No automatic owner/boot recovery.

## Real-run preflight helper (do not run against real load without authorization)

Prepare a **private run package**, with actual authorized run identity, observation URL
and authentic notice evidence. Example numbers below are **engineering proposals only**,
not user-set limits or measurements. Demand must be recalculated for the selected model,
context, batch, KV types and GPU layers before a real test:

```json
{
  "run_id": "explicit-current-run",
  "observation_url": "http://127.0.0.1:8080/api/v1/instances/router/observation",
  "thresholds": {"cpu_percent": 80, "ram_percent": 85, "gpu_percent": 80, "vram_percent": 85},
  "demand": {"ram_bytes": 1073741824, "vram_bytes": 1073741824,
    "rationale": "Example estimates only; replace with this model/parameter demand and margin"},
  "authorized": false,
  "notice_delivered": false,
  "owner_paused": false,
  "safety_failed": false
}
```

```sh
python3 tools/evaluator/preflight.py --package /private/real-run.json \
  --key-file /private/n1.key --samples 4 --interval 2.1
```

The helper collects a short bounded window from the **existing sampler**, exposes fresh
distinct timestamps, per-metric maximum gaps and coverage, and persists warning latch and
operator confirmation to this private package after each sample. One sample or repeated
cache timestamps cannot authorize the normal low-load path. Missing GPU/critical metrics
are unknown, never zero. Thresholds are labelled `engineering_proposals_not_user_limits`.

High occupancy, insufficient headroom or unknown metrics initially leaves the run prepared.
The operator reports values/impact and obtains a genuine informed current-run confirmation
through the existing user channel. Supply that receipt as a private file:

```json
{"run_id":"explicit-current-run","choice":"retain_coload","informed":true,
 "receipt":"<actual user confirmation reference>"}
```

`choice` may instead be `clear_resources` (the user clears resources themselves).
Pass `--confirmation /private/confirmation.json`. Once confirmed for this run, further
high/unknown samples are **recording, not another approval gate**; persisted confirmation
survives helper restart. Waiting or spontaneous occupancy decrease does not clear a prior
warning. Normal low load uses existing authorization and notice, with no extra approval.
No helper can override owner pause, identity/isolation, budgets or actual safety failures.
It never closes external processes and never fabricates delivered notice or user consent.

Keep baseline, known/unknown external co-load, process resource observations and actual
OOM/timeouts/results in the private run evidence. This helper is not a capacity validator.
Windows reparse/holding-handle compatibility, real engines/models and GPU behavior remain
separate target-runtime verification; Linux synthetic GREEN does not establish them.
