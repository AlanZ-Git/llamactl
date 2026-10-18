# Backend environment policy and derived presets

## Scope and defaults

`environment_policy` is an optional **instance option**, persisted in the existing
options JSON and returned by the existing instance APIs:

- Missing, `""`, or `"inherit"`: legacy host-environment inheritance.
- `"isolated"`: opt-in environment handling for a **local, native llama.cpp**
  instance. Other values are errors. Docker (including a global Docker default),
  remote instance stubs, and other backends are unsupported in this mode.

This is not a controlled-instance security role, sandbox, owner-pause switch,
controller, migration, or model-readiness guarantee. Existing instances are not
migrated. llamactl's external management/inference authentication is unchanged.
The configured executable, its arguments, and explicit environment configuration
remain trusted administrator inputs; arbitrary wrappers are not sandboxed.

For example (all values below are synthetic):

```json
{
  "environment_policy": "isolated",
  "auto_restart": false,
  "backend_type": "llama_cpp",
  "environment": {"LLAMA_ARG_THREADS": "4"},
  "backend_options": {
    "host": "127.0.0.1",
    "api_key": "synthetic-backend-token"
  }
}
```

The existing manager allocates a port if none is supplied.

## Child environment

For an isolated instance, ambient host `LLAMA_*` entries are excluded, including
`LLAMA_API_KEY` and all `LLAMA_ARG_*` entries. Comparison is case-insensitive on
Windows and case-sensitive on Unix. PATH, OS, runtime/library, and GPU-related
variables outside that prefix are retained. No parent environment is modified.

Explicit trusted environment layers remain allowed, including explicit LLAMA
variables. Precedence is host < backend configuration < instance configuration.
Explicit empty strings remain empty. The final child environment has no duplicate
keys under the target OS's comparison rules. Within one map, case aliases on
Windows resolve deterministically: the last lexicographically sorted spelling
wins. Prefer one spelling per name. The Windows comparison algorithm has Linux
unit coverage; this is **not Windows execution evidence**.

Inheritance mode retains ambient LLAMA variables and the existing effective
precedence. It does not gain the isolation or routing validation promises below.

## Loopback and backend authentication

Isolated mode requires a literal loopback IP. Omitted host becomes explicit
`127.0.0.1`; IPv6 `::1` is also supported. DNS names (including `localhost`) and
wildcard/non-loopback addresses are rejected. An explicit trusted
`LLAMA_ARG_HOST` must itself be loopback and agree with a supplied typed host;
otherwise preparation fails before stopping the current instance.

One literal backend key can be supplied through typed `backend_options.api_key`
or trusted `LLAMA_API_KEY` environment configuration. Backend/instance environment
maps use the precedence above. If the effective explicit environment key differs
from a nonempty typed key, isolated preparation fails, including an explicitly
empty environment key. Equal keys are allowed. Comma-separated key lists,
whitespace, and control characters are not supported. Key files are never read,
and validation errors do not include key values.

On the **local native llama** proxy and direct health probe, an explicit backend
key replaces caller Authorization. Thus a management/inference credential is not
mistaken for a backend credential. In isolated mode with no backend key, the local
proxy removes caller Authorization. Legacy local mode without an explicit backend
key retains caller Authorization; ambient host keys alone do not enable the new
substitution. Legacy explicitly configured literal keys receive the same local
substitution, but conflicting legacy configurations are not retroactively
validated. Do not combine legacy key files/raw auth flags with this substitution.

Remote proxy requests continue to use the configured **node** key, never the local
backend key. Health probes do not follow redirects (including same-host redirects),
so credentials are not sent to a redirected destination. The reverse proxy does
not itself follow backend redirects either.

Isolated mode rejects the following rather than guessing effective routing/auth:

- Typed key files, external `models_preset` files, TLS key/certificate files, and
  API prefixes. `preset_ini` itself is supported.
- Host, port, API-key/key-file, API-prefix, TLS, or models-preset options in raw
  backend arguments or `extra_args` (including equals and underscore spellings).
- Those routing/authentication settings in `preset_ini` entries. Presets should
  contain model settings, not server credentials or listener configuration.
- Corresponding explicit LLAMA environment routing/authentication variables,
  except the supported `LLAMA_API_KEY` and consistent `LLAMA_ARG_HOST` sources.
  Configure port through the typed option, not `LLAMA_ARG_PORT`.
- Invalid explicit environment names or embedded NUL values.

Unknown *model* options continue through the existing `extra_args` representation;
this package does not implement a full llama.cpp argument/INI schema or certify
that a particular llama-server release accepts every model setting. Literal keys
still use the existing options/config storage and may appear in process argv when
supplied as typed options. No new secret storage or scoped credentials are added.

## Preset preparation and failures

Options are authoritative. For local instances, `preset_ini` is synchronized on
checked construction, option changes, persisted restoration, and every start
(including automatic restarts). Missing, null, or empty `preset_ini` means clear;
these forms intentionally have the same clearing semantics, not patch semantics.

Preparation clones the candidate, validates it, checks the destination type, then
writes a unique **same-directory temporary file**, checks write/flush/close, and
retains it for the stopped-stage commit. Clearing an existing file also probes the
parent directory during preparation. Non-regular preset destinations, including
symlinks and directory collisions, are rejected. Cleanup is limited to that
operation's own temporary file; there is no directory-wide cleanup.

For a running update, the old process, options, old preset and old port reservation
are retained through preparation. A candidate port may be reserved temporarily;
it is released on pre-commit failure. Only after preparation succeeds does the
manager stop the old instance, commit the replacement/clear, install options, and
release the old port. Direct `SetOptions` returns an error and cannot commit local
options while running/restarting/shutting down. Manual restart also preflights
before stopping. Production creation/load paths use `NewChecked`; the legacy
`New` convenience constructor panics on a preparation error and is not used by
production callers.

Replacement uses `os.Rename`, not direct truncation of `preset.ini`, and never
falls back to deleting the old file to force replacement. Linux local tests cover
replacement and failure. Windows file sharing/rename behavior may reject a
replacement; that error is surfaced. This is not a claim of Windows-tested atomic
replacement or power-loss durability (there is no directory fsync transaction).
Filesystem races, ACLs, and sharing locks can still cause commit-stage failure.

This is **not a file + database + process transaction**:

- Invalid configuration or preparation failure leaves the old running instance
  and its reservation/options/preset alone.
- A later commit failure is explicit; the old process may already be stopped.
- A later start failure is explicit; new options/preset/port may already be
  committed, while the database may still contain the previous configuration.
- A later database failure is explicit; a successfully started process is not
  automatically rolled back. Reconcile using the reported stage and live state.
- Creation database failure removes the new registry entry and port reservation,
  but a committed derived preset may remain. It does not report success.
- Persisted reconstruction/preparation failures are propagated to the loader;
  failed instances are not registered and cannot autostart. The existing manager
  constructor logs aggregate load errors while continuing with other successfully
  prepared instances; it does not turn them into an overall startup success claim.

## Explicit zero/false and scope

This package does **not** convert the approximately 200 typed llama fields to
pointers. Many existing typed numeric/bool fields still cannot distinguish zero
or false from omission. Use the existing representations deliberately:

- `preset_ini` preserves model-scoped text verbatim, including explicit `0` and
  `false`, section boundaries, and unknown model entries.
- `extra_args` preserves string values such as `{"seed":"0"}` in final argv.
  An empty value means a presence flag, e.g. `{"no-warmup":""}`. For booleans,
  use a backend-supported negative flag rather than assuming `--flag false` works.
- Do not supply contradictory typed/global model flags alongside those values.
  This package does not merge or resolve arbitrary model-parameter conflicts.

The regression fixture covers JSON → real SQLite → reload → derived INI and
captured final argv from a synthetic test subprocess. It checks `auto_restart:
false`, `idle_timeout: 0`, model preset `temp = 0` / `flash-attn = false`, CLI
`--seed 0`, `--no-warmup`, Unicode/space paths, and absence of conflicting global
model flags. It proves transport/persistence/command construction, **not llama
parser acceptance, model readiness, real inference, Windows, GPU, or performance**.

## API schema maintenance

The existing Swagger artifacts (`docs.go`, `swagger.json`, `swagger.yaml`) expose
`environment_policy` as a string in `instance.Options`; existing endpoints are
unchanged. For this package the three property entries were synchronized directly
because running the established Swag generator was denied by the execution policy.
No generator/dependency installation or frontend schema generation was performed.
