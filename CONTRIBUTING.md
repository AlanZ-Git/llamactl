# Contributing to Llamactl

Thank you for considering contributing to Llamactl! This document outlines the development setup and contribution process.

## Development Setup

### Prerequisites

- Go 1.26 (`go.mod` specifies `1.26.0`; use a compatible patch release)
- Node.js 22.12+ and npm (honor `webui/package-lock.json`; release workflow uses Node 22)
- A target C compiler with `CGO_ENABLED=1` for `github.com/mattn/go-sqlite3`
- A fixed `llama-server` build only for authorized real-engine tests; unit/contract tests use fixtures

For Windows/Linux builds and independent startup, use the [workstation guide](docs/workstation-deployment.zh-CN.md).
Build WebUI before Go: `webui/webui.go` embeds `dist/*`; a stale dist is not a fresh build, and CGO=0 is not a working SQLite deployment.

### Getting Started

1. **Get the intended source**
   Use the maintainer-provided fork and full revision, or a candidate with a verified file manifest.
   Record `git rev-parse HEAD` and `git status --short`; preserve preexisting edits.
   Upstream latest/images do not establish this fork's controlled/N1 identity.

2. **Install dependencies**
   ```bash
   # Go dependencies
   go mod download
   
   # Frontend dependencies
   cd webui && npm ci && cd ..
   ```

3. **Run for development**

   First set an explicit `LLAMACTL_CONFIG_PATH`, independent absolute data/cache paths, loopback binding and securely
   injected management credentials as in the workstation guide. `go run` is not read-only: it initializes/migrates SQLite
   and may restore ordinary instances. Never point it at daily state for a quick check.
   Build embedded UI first with `cd webui && npm run build && cd ..`.
   ```bash
   # Start backend server
   go run ./cmd/server
   ```
   Server will be available at `http://localhost:8080`
   
   ```bash
   # In a separate terminal, start frontend dev server
   cd webui && npm run dev
   ```
   Development UI will be available at `http://localhost:5173`

4. **Common development commands**
   ```bash
   # Backend
   go test ./... -v                    # Run tests
   go test -race ./... -v              # Run with race detector
   go vet ./...                       # Static checks; format only files you changed
   
   # Frontend (run from webui/ directory)
   npm run test:run                    # Run tests once
   npm run test                        # Run tests in watch mode
   npm run type-check                  # TypeScript type checking
   npm run lint:fix                    # Lint and fix issues
   ```

### Module entry points

| Path | Responsibility |
|---|---|
| `cmd/server/main.go`, `pkg/config/` | Startup, configuration/env precedence and paths |
| `pkg/server/` | HTTP routes, auth, control/N1 handlers, proxy and contract tests |
| `pkg/manager/` | Instance lifecycle, control/tests, N1 reservation/execution/recipes |
| `pkg/instance/`, `pkg/backends/` | Admission, options/presets, process ownership, ready identities, argv |
| `pkg/database/` | SQLite stores and embedded migrations, including N1 004 |
| `pkg/resourcepolicy/`, `pkg/resources/` | Owner pause/release and resource sampler |
| `pkg/models/` | Downloads and N1 inventory/content identities |
| `webui/` | React UI, Vitest tests and embedded build output |
| `tools/evaluator/` | Standard-library N1 CLI; [single-source usage](tools/evaluator/README.md) |

Options/SQLite are live deployment authority; preset.ini is derived. Recipe versions are immutable configuration/evidence,
not a second mutable live store. Files/DB/processes are not one transaction: expose and preserve failure stages.
Keep ordinary instances, controlled admission, and protocol-enabled control/N1 distinct in code and tests.
Whole-tree formatting/lint fixes write files; prefer targeted formatting and avoid unrelated edits.

## Before Submitting a Pull Request

### Required Checks

For code changes, run relevant tests during development and these checks before submission.
Documentation-only edits need affected links/anchors/nav/examples and source correspondence checks, not unrelated
full regressions or real-model runs. Record actual commands/results and unverified boundaries.
On a fresh tree, run the frontend build first, before Go tests/vet/build that import the embedded WebUI package.

1. **Backend**
   ```bash
   go test ./... -v
   go test -race ./... -v
   go vet ./...
   # After the frontend build below; Unix example, output outside the repository
   CGO_ENABLED=1 go build -o ../llamactl-build/llamactl ./cmd/server
   ```

2. **Frontend**
   ```bash
   cd webui
   npm run test:run
   npm run type-check
   npm run build
   ```

Linux race/mock tests and cross-compilation do not certify Windows/SQLite runtime, GPU loading, ready or effective output.
UI changes also need actual browser verification. Real-engine tests, network/production/credential changes and existing-DB
migration require applicable authorization; live CLI evaluation is not a read-only check.

### API Documentation

Keep both API layers synchronized:

- Ordinary API: relevant `pkg/server/` handler comments and `docs/docs.go`, `docs/swagger.json`, `docs/swagger.yaml`.
- Controlled/N1: `docs/control-api.zh-CN.md`, `docs/control-openapi.yaml`, client/affected feature docs and
  `pkg/server/*openapi_test.go` plus applicable auth/validation/lifecycle tests.

Cover missing/null/0/false, unknown fields, permissions, boot/revision, ready/use identities, owner pause, non-replay,
and independent execution/cleanup results. Do not turn failed cleanup into execution success.
If regeneration is needed, use the Swag version pinned in `go.mod`, not latest:

```bash
# Install swag if needed
go install github.com/swaggo/swag/cmd/swag@v1.16.5

# Update comments in the relevant pkg/server/handlers_*.go file
# Then regenerate docs
swag init -g cmd/server/main.go
```

Review generated diffs; Swag does not generate `control-openapi.yaml`.
Focused synthetic checks include `go test ./pkg/server -run 'OpenAPI'` and
`python3 -m unittest discover -s tools/evaluator/tests`; select the affected checks, not live CLI calls.

## Pull Request Guidelines

### Pull Request Titles
Use this format for pull request titles:
- `feat:` for new features
- `fix:` for bug fixes  
- `docs:` for documentation changes
- `test:` for test additions or modifications
- `refactor:` for code refactoring

### Submission Process
1. Follow the agreed fork flow: `main` tracks upstream; feature work integrates through `dev` toward verified `stable`; no direct development on `main`/`stable`
2. Make changes following the coding standards
3. Run all required checks listed above
4. Update documentation if necessary
5. Commit/push/PR only with applicable authorization (development is not remote publication permission). Submit with:
   - Clear description of changes
   - Reference to any related issues
   - Screenshots for UI changes

## Code Style and Testing

### Testing Strategy
- Backend tests use Go's built-in testing framework
- Frontend tests use Vitest and React Testing Library
- Run tests frequently during development
- Add tests for new features and bug fixes

### Go
- Follow standard Go formatting (`go fmt`)
- Use meaningful variable and function names  
- Add comments for exported functions and types
- Handle errors appropriately

### TypeScript/React
- Use TypeScript strictly (avoid `any` when possible)
- Follow React hooks best practices
- Use meaningful component and variable names
- Prefer functional components over class components

## Documentation Development

This project uses MkDocs for documentation. When working on documentation:

### Setup Documentation Environment

```bash
# Disposable venv, not shared/system Python
python3 -m venv /tmp/llamactl-docs-venv
/tmp/llamactl-docs-venv/bin/python -m pip install -r docs-requirements.txt
```

### Development Workflow

```bash
# Serve documentation locally for development
/tmp/llamactl-docs-venv/bin/mkdocs serve
```
The documentation will be available at http://localhost:8000

```bash
# Build static documentation site
/tmp/llamactl-docs-venv/bin/mkdocs build --site-dir /tmp/llamactl-docs-site
```
The site is in the specified temporary directory. Review warnings separately; non-strict build success does not validate
all links/platform instructions (CI uses `--strict`). Windows uses the independent venv's `Scripts/python.exe`/`Scripts/mkdocs.exe`.

### Documentation Structure

- `docs/` - Documentation content (Markdown files)
- `mkdocs.yml` - MkDocs configuration
- `docs-requirements.txt` - Python dependencies for documentation

### Adding New Documentation

When adding new documentation:

1. Create Markdown files in the appropriate `docs/` subdirectory
2. Update the navigation in `mkdocs.yml`
3. Validate links/anchors and examples; build with existing hooks `readme_sync.py` and `fix_line_endings.py`
4. Verify an explicit public candidate allowlist without private dependencies; publish only when authorized

Four product entries: README/index, workstation deployment, existing client/API/feature docs, and this file.
Do not duplicate CLI flags: `docs/evaluator-client.md` includes `tools/evaluator/README.md`.
Keep real host/account/device coordinates, credentials, raw requests, private assets and logs out of public files.
Local construction/progress notes are not prerequisites for building or using an exported repository.

### Documentation Deployment

An upstream-oriented Pages workflow exists; it does not mean this fork's docs/binaries have been published.
Check intended remote and full allowlist/history before an authorized push; do not deploy as part of a local doc edit.

## Getting Help

- Check existing [issues](https://github.com/lordmathis/llamactl/issues)
- Review the [README.md](README.md) for usage documentation  
- Look at existing code for patterns and conventions

Thank you for contributing to Llamactl!