# AGENTS.md

Guidance for AI coding assistants (Claude Code, Codex, Cursor, Aider, etc.) working in this repo. `CLAUDE.md` is a symlink to this file — both names exist so different agent harnesses pick it up automatically.

## What this project is

`ocifactory` is an open-source, lightweight, multi-format artifact registry that uses an **OCI registry as its sole storage backend**. The aim is to be a drop-in replacement for Artifactory / Nexus for users who can't or don't want to run them — small teams, OSS maintainers, regulated shops that already have an OCI registry but no general artifact store, etc.

Design pillars (in priority order):

1. **OCI as the only backend.** No Postgres, no S3, no separate metadata store. Everything — packages, indexes, dist-tags — is content-addressable blobs and tags inside an OCI repo. This is the load-bearing decision; preserve it unless you have a strong reason not to.
2. **Production-ready, not a demo.** Every feature lands with unit tests and (where it crosses a network) integration tests. No half-finished surface area shipped to `main`.
3. **Pluggable architecture.** Anything users will reasonably want to swap — auth, authz, the OCI backend client, observability — goes behind an interface, not a concrete type.
4. **Lean ops.** Single Go binary. Container image. Deploy to GCP Cloud Run or Cloudflare Workers/Containers. No Helm chart sprawl.
5. **Good docs.** README explains the value prop and quickstart. `docs/` covers architecture, each repo type, and operations. Code comments only when "why" is non-obvious.

## Current status

| Area | State |
|---|---|
| `pkg/oci` — OCI-backed registry primitives (Add/Read/List/Delete/AppendRefs) | Done, tested with in-memory fake |
| `pkg/handler/python` — PEP 503 simple index, twine upload, pip download | Done, tested |
| `pkg/handler/maven` — Maven 2 layout (releases, snapshots, metadata, archetype catalog) | Done, tested |
| `pkg/handler/npm` — Routes wired up, all handlers return 501 | **Stub — next up** |
| `pkg/handler` — `Server`, `PassThroughAuth`, `Logger` middlewares | Done |
| `pkg/cred` — Auth credential context | Done (basic auth only) |
| `cmd/ocifactory serve` | Works for `--repo-type=python|maven` |
| Go module proxy support | Not started |
| Debian/apt support | Not started |
| Pull-through proxy / caching | Not started |
| Vulnerability scanning | Not started |
| Authn/Authz beyond pass-through basic auth | Not started |
| Dockerfile / deployment | Not started |
| CI: lint, test, build, image publish | Only `go-test` from `abcxyz/pkg` |

## Architecture (read this before changing things)

```
                     ┌─────────────────────────────────────┐
HTTP request ──►     │  cmd/ocifactory  (CLI entrypoint)    │
                     └────────────┬────────────────────────┘
                                  │
                     ┌────────────▼────────────────────────┐
                     │  pkg/handler                         │
                     │  • Server (port + middleware chain)  │
                     │  • PassThroughAuth, Logger           │
                     └────────────┬────────────────────────┘
                                  │ http.Handler
                     ┌────────────▼────────────────────────┐
                     │  pkg/handler/{python,maven,npm,...}  │
                     │  Each speaks one client protocol     │
                     │  (pip, mvn, npm, go mod, apt, ...)   │
                     └────────────┬────────────────────────┘
                                  │ Registry interface (handler.Registry)
                     ┌────────────▼────────────────────────┐
                     │  pkg/oci.Registry                    │
                     │  AddFile / ReadFile / ListTags /     │
                     │  ListFiles / DeleteRepoFiles /       │
                     │  AppendRefs                          │
                     └────────────┬────────────────────────┘
                                  │ ORAS (oras-go/v2)
                     ┌────────────▼────────────────────────┐
                     │  Any OCI registry                    │
                     │  (GAR, ECR, GHCR, Harbor, zot, ...)  │
                     └─────────────────────────────────────┘
```

### Key types

- `oci.RepoFile{OwningRepo, OwningTag, RefTag, Name, MediaType, Digest}` — addresses one file inside the OCI-backed virtual store.
  - `OwningRepo` is the OCI repository name (e.g. `packages/requests`, `com/foo/bar`).
  - `OwningTag` is the canonical version tag (e.g. `2.31.0`). One OCI manifest per `OwningTag`; layers are the files in that version.
  - `RefTag` is an alias tag like `latest`. Stored prefixed with `ref_` in the backend so `ListTags` can filter them out (callers see clean tag names).
- `handler.Registry` — the interface every per-format handler depends on. Keep it minimal; do not push format-specific concepts into it.
- `cred.Cred` — credentials carried in the request `context.Context`. Today only `Basic`. When adding OAuth/OIDC/JWT, add a new field rather than overloading `Basic`.

### How a per-format handler is structured

Each `pkg/handler/<format>` package owns:
- `RepoType` constant (`"npm"`, `"python"`, …) — used by `serve` to dispatch.
- `ArtifactType` constant — the OCI manifest `artifactType` (e.g. `application/vnd.ocifactory.npm`). Unique per format so users can tell formats apart in their OCI registry.
- `NewHandler(handler.Registry) (*Handler, error)`
- `Mux() http.Handler` — gorilla/mux router for that format's URL scheme.
- Translation logic mapping client protocol calls ↔ `RepoFile` operations.

When adding a new format, copy the structure from `pkg/handler/python` (it's the most complete reference, including the embedded simple index template and the dual-write trick that maintains an `index` repo for fast `list packages`).

### Index repos pattern

For formats where you need "list all packages I have" (PyPI simple index, npm registry root, etc.), `python` handler maintains a parallel OCI repo named `index` whose tags are package names. This avoids needing a sidecar database — `ListTags("index")` is the package list. Reuse the pattern when implementing npm/Go/apt.

## Build, run, test

```bash
# Build
go build ./...

# Full suite. Includes the streaming-upload integration test in
# pkg/oci which spins up a real zot via testcontainers-go — needs a
# reachable Docker daemon. Self-skips with a logged message if Docker
# isn't there, so this is safe to run anywhere; CI runs it on every PR.
go test -race ./...

# Fast local iteration / Docker-less environments. -short skips the
# live-zot test only; every other test still runs.
go test -race -short ./...

# Same flags CI uses (sans -short).
go test -count=1 -race -shuffle=on -coverprofile=coverage.out ./...

# Vet, format, tidy
go vet ./...
gofmt -s -w .
go mod tidy

# Run locally against a real OCI registry (e.g. local zot or GAR)
go run ./cmd/ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8080
```

## Code conventions

- **Languages:** Go is the only implementation language. Avoid pulling in shell scripts when a Go test will do.
- **Style:** Write idiomatic Go. Follow [Effective Go](https://go.dev/doc/effective_go) and the Google Go style guide — [overview](https://google.github.io/styleguide/go/), [style decisions](https://google.github.io/styleguide/go/decisions), [best practices](https://google.github.io/styleguide/go/best-practices). Prefer explicit and boring over clever. `gofmt -s` and `go vet` must be clean before every commit.
- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. Use `errors.Is` / `errors.As` to check. The `pkg/oci` package already exposes `oci.HasCode(err, statusCode)` for translating ORAS HTTP errors — use it in handlers rather than re-deriving status codes.
- **Logging:** `github.com/abcxyz/pkg/logging`. Read with `logging.FromContext(ctx)`; configure via `OCIFACTORY_LOG_LEVEL`, `OCIFACTORY_LOG_FORMAT`, `OCIFACTORY_LOG_DEBUG`.
- **Routing:** gorilla/mux (already adopted, see commit `26f36de`). Don't reach for stdlib `http.ServeMux` for new format handlers.
- **Public API surface:** anything in `pkg/` is public. Don't expose internals you wouldn't want to support — when in doubt, lowercase it.
- **Configuration:** Operator-tunable knobs (timeouts, thresholds, feature toggles, backend URLs) go through a CLI flag on `cmd/ocifactory serve`, not `os.Getenv` reads scattered inside library code. Library types (e.g. `oci.Registry`) accept the value through a typed option (`WithStreamingPushDisabled(bool)`, `WithArtifactType(string)`, …) so tests can override it without touching the environment and there's exactly one place — the flag definition — that enumerates every knob ocifactory exposes. Environment variables are reserved for what the runtime / harness sets (`OCIFACTORY_LOG_LEVEL`, secret material, GCP `GOOGLE_APPLICATION_CREDENTIALS` and friends), not product behaviour.

### Testing rules

These are non-negotiable. Apply them to every test in the repo:

1. **`t.Parallel()`** at the top of every test function and every subtest. The only exception is when a test mutates process-global state and a comment says so. Tests must be safe to run in parallel.
2. **`t.Context()`** (Go 1.24+) for the test's `context.Context` — it's auto-cancelled on test cleanup. Don't reach for `context.Background()` or hand-roll a cancel.
3. **Table-driven tests** wherever a function has more than one interesting input. Even for small N, the structure makes adding cases trivial:
   ```go
   tests := []struct{
       name string
       // inputs...
       // wants...
   }{ /* ... */ }
   for _, tc := range tests {
       t.Run(tc.name, func(t *testing.T) {
           t.Parallel()
           // ...
       })
   }
   ```
4. **Compare whole values with `cmp.Diff`** (`github.com/google/go-cmp/cmp`):
   ```go
   if diff := cmp.Diff(want, got); diff != "" {
       t.Errorf("Foo() mismatch (-want +got):\n%s", diff)
   }
   ```
   Don't compare field-by-field with `==` or `reflect.DeepEqual` when `cmp.Diff` will work — the diff output is what makes failures debuggable. The argument order is `(want, got)` so the diff legend reads correctly.
5. **Fakes, not mocks.** No `gomock`, no `testify/mock`, no codegen mock libraries. Write a small fake of the interface in a `_test.go` file (or `pkg/<x>/fake.go` if reused across packages). For handler tests, don't mock `handler.Registry` — exercise the real `pkg/oci` code on top of the in-memory backend in `pkg/oci/fake.go`, so the layers are tested together.

## Adding a new repo type — checklist

1. Create `pkg/handler/<format>/` with `handler.go`, the `Mux()`, and translation logic.
2. Define `RepoType` and `ArtifactType` constants.
3. Plumb it into `pkg/commands/serve.go`'s `supportedRepoTypes` and the `switch` in `Run`.
4. Add handler tests using the `oci.fake` backend (cover happy path + 404 + auth errors at minimum).
5. Add an integration test or a documented manual test against a real client (`pip`, `mvn`, `npm install`, `go mod download`, `apt-get`).
6. Document the format under `docs/repos/<format>.md`: URL layout, supported client commands, known limitations.
7. Update the status table in this file.

## Roadmap (one step at a time)

The intent is to ship each phase production-ready before starting the next. See `docs/ROADMAP.md` for the long form.

1. **Phase 1 — Core formats:** npm → Go module proxy → Debian/apt. (Maven and PyPI already done.)
2. **Phase 2 — Deployability:** Dockerfile, container image publishing in CI, Cloud Run / Cloudflare Workers deployment guide, basic Prometheus metrics.
3. **Phase 3 — Auth/Authz extensibility:** OIDC + JWT verification, scoped tokens, pluggable authorizer interface (allow/deny per repo/operation).
4. **Phase 4 — Pull-through proxy:** read-through caching from upstream npm/PyPI/Maven Central/proxy.golang.org/Debian mirrors. Cache TTLs, negative caching, immutable-version pinning.
5. **Phase 5 — Add-ons:** vulnerability scanning (Grype/Trivy integration), retention policies, web UI.

## GitHub workflow

- **Issues are the source of truth for tasks.** Open an issue before non-trivial work; link it from the PR.
- **One feature = one PR.** Keep PRs reviewable. Refactors should be separate PRs from feature work.
- **PR description** should explain motivation + summary of approach + manual test steps. Link the issue with `Closes #N`.
- **CI must be green** before merge. Pre-existing failures aren't a license to add new ones.
- **Dependabot PRs** auto-target main weekly via `.github/dependabot.yml`. Review and merge promptly to stay current.
- **Conventional-ish commit titles** (matching existing history): `Add X`, `Fix Y`, `Switch to Z`, `Bump …`. PR title becomes the squash-merge commit.

## Collaboration model (how the human and AI agent work together)

The human is the product owner / reviewer; the agent does most of the implementation.
**Default loop for every feature:**

1. **A GitHub issue exists** describing the feature. Two paths to get there:
   - **Human files it directly** and sends the agent the issue link, or
   - **Human and agent discuss first** (chat about scope, design, edge cases). Once aligned, the agent drafts the issue and creates it via `gh issue create` for the human's approval, then proceeds. Confirm scope with the human before opening the issue if anything is ambiguous.
2. **Agent runs the full implementation cycle** without further prompting:
   1. Create a feature branch named `<short-topic>` (e.g. `npm-publish`, `go-proxy-list`) from up-to-date `main`.
   2. Implement the feature with tests, commit in logical chunks.
   3. Open a PR with body that references the issue (`Closes #N`), summarizes motivation + approach, and lists manual test steps.
   4. **Monitor CI** (`gh pr checks --watch`). Fix failures and push until green.
   5. **Run a self-review pass** using the `/code-review` plugin command (the `code-review:code-review` skill). It dispatches multiple specialized sub-reviewers in parallel against the PR diff. Address findings before involving the human — the human's review should not catch issues a multi-agent pre-review would have caught.
   6. **Notify the human** with the PR URL once CI is green AND the pre-review pass is addressed.
3. **Human reviews and leaves comments.** Treat human review feedback as authoritative — but ask if a comment seems technically wrong or ambiguous rather than agreeing performatively. (See `superpowers:receiving-code-review`.)
4. **Agent addresses feedback** by pushing additional commits (don't force-push or amend during review). Reply on each thread when fixed. Multiple rounds are expected.
5. **Merge** only when the human says so, or asks the agent to merge. Use squash merge to keep `main` history clean.

**Don't skip the pre-review step.** It exists because human review time is the scarce resource, not agent time.

**When the loop should pause and ask:**
- The issue is ambiguous, or implementation reveals a design question the issue didn't resolve.
- The change touches the `handler.Registry` interface, auth, or anything in [Things to ask before changing](#things-to-ask-before-changing).
- A dependency needs to be added.
- CI failures look pre-existing or environmental rather than caused by the branch.

## Deployment target

- Primary: **GCP Cloud Run** with Artifact Registry (GAR) as the OCI backend. ocifactory authenticates to GAR via the runtime service account.
- Secondary: **Cloudflare Workers/Containers** for edge cache use cases (especially Phase 4 pull-through).
- Storage backend can be any OCI v2 registry — keep that flexibility; do not add GAR-specific code outside an optional plugin.

## Things to ask before changing

- Adding a new top-level dependency (especially anything implying state outside the OCI backend).
- Changing the `handler.Registry` interface — every format handler depends on it.
- Adding a new auth mechanism — discuss the interface shape first.
- Anything that breaks backward compatibility of existing `--repo-type=python|maven` users (assume there's at least one in the wild).

## Out of scope

- Hosting our own OCI registry. We integrate with existing ones; we don't reimplement one.
- Full Artifactory feature parity. We aim for a useful subset, well-executed, not a checkbox match.
- A web UI in Phase 1–3. Defer until the API surface is stable.
