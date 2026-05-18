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
| `pkg/oci` — OCI-backed registry primitives (Add/Read/List/Delete/AppendRefs), streaming-push integration test against a real zot | Done, tested with in-memory fake + live-zot |
| `pkg/oci` — deterministic `_f_<sha256>` file tags so file reads are one round-trip | Done |
| `pkg/oci` — blob-download redirect to backend presigned URLs (GAR/ECR/ACR/GHCR/Docker Hub) | Done |
| `pkg/handler/python` — PEP 503 simple index, twine upload, pip download, per-package simple-index cache | Done, tested (incl. real-client integration tests). Operator docs: [`docs/repos/python.md`](docs/repos/python.md). |
| `pkg/proxy/python` + python proxy mode (registry hit → filter → PyPI JSON metadata → file fetch → tee to OCI; pull-through indexes with stale-OK + synthesis fallback; uploads → 405) | Done, tested. Wired into `pkg/handler/python` for namespaces with `mode: proxy`. Operator docs: [`docs/repos/python.md`](docs/repos/python.md#proxy-mode-pull-through-pypi). |
| `pkg/handler/maven` — Maven 2 layout (releases, snapshots, metadata, archetype catalog), checksum verification, snapshot-always-overwrite | Done, tested (incl. real-client integration tests). Operator docs: [`docs/repos/maven.md`](docs/repos/maven.md). |
| `pkg/proxy/maven` + maven proxy mode (registry hit → filter → Maven metadata fetch → file fetch → tee to OCI; live metadata passthrough; writes → 405) | Done, tested. Wired into `pkg/handler/maven` for namespaces with `mode: proxy`. Operator docs: [`docs/repos/maven.md`](docs/repos/maven.md#proxy-mode-pull-through-maven). |
| `pkg/handler/npm` — npm registry HTTP protocol (`npm publish`, `npm install`, `npm dist-tag add\|ls`), scoped + unscoped packages | Done, tested (incl. real-client integration tests). Operator docs: [`docs/repos/npm.md`](docs/repos/npm.md). |
| `pkg/proxy/npm` + npm proxy mode (registry hit → filter → packument fetch/rewrite/cache → tarball fetch → tee to OCI; stale-OK + synthesis fallback; writes → 405) | Done, tested. Wired into `pkg/handler/npm` for namespaces with `mode: proxy`. Operator docs: [`docs/repos/npm.md`](docs/repos/npm.md#proxy-mode-pull-through-npm). |
| `pkg/handler` — `Server`, `Logger`, `MetricsMiddleware`, `ObservabilityHandler` (intercepts `/healthz` / `/readyz` / `/metrics` before format mux) | Done |
| `pkg/metrics` — pluggable Recorder (Prometheus default, no-op for tests) | Done |
| `pkg/auth` — Pluggable frontend authentication (Authenticator, AuthContext, Chain, OIDC) | Done, tested. OIDC-only — static passwords are out-of-tree by design. Configured via `OCIFACTORY_AUTHN_*` flags / env vars. |
| `pkg/auth` — Pluggable `Authorizer` interface with `OpRead` / `OpWrite` ops; `AllowAll` / `DenyAll` built-ins for tests / fallbacks | Done |
| `pkg/auth/backend` — Pluggable backend credential `Provider` interface and in-tree adapters (`anonymous`, `gcpadc`, `staticenv`, `dockerconfig`) | Done, tested. Wired into `oci.Registry` via `WithBackendAuth`. Configured via `OCIFACTORY_BACKEND_AUTH_*` flags / env vars. |
| `pkg/namespace` — Namespace data model, OCI-backed `Store`, JSON `Spec` with `SchemaVersion` | Done, tested. Operator docs: [`docs/admin.md`](docs/admin.md). |
| `pkg/namespace` — Per-namespace `Policy` (readers/writers `SubjectMatcher`s on issuer / sub regex / email / claims), `PolicyAuthorizer`, data-plane `Registry` wrapper with policy cache + per-namespace package index | Done, tested. Authorizer is pluggable via `AuthzFactory`. |
| `ocifactory admin serve` — control-plane namespace CRUD API (`PUT/GET/DELETE /admin/v1/namespaces/{name}`, list, soft-delete) | Done, tested. Operator docs: [`docs/admin.md`](docs/admin.md). |
| `pkg/handler/echo` — No-op auth target for the GitHub OIDC CI job | Done. Not a real artifact format; no OCI backend, no `handler.Registry`. Exists to give CI a concrete request to make against a real OIDC issuer. |
| `cmd/ocifactory serve` | Works for `--repo-type=python\|maven\|npm\|echo` (echo runs without `--backend-registry`). Every URL is namespace-prefixed: `/{namespace}/...` for python and npm; `/{namespace}/maven2/...` for maven. |
| `cmd/ocifactory admin serve` | Works against any OCI backend; runs on a separate listener (`--port`, default `8081`). |
| Dockerfile + multi-arch image publishing | Done. `Dockerfile.goreleaser` is distroless/nonroot; images at `ghcr.io/yolocs/ocifactory:vX.Y.Z` (+ `:vX.Y` / `:latest` for stable). Built via Goreleaser; release procedure in [`RELEASING.md`](RELEASING.md). |
| Release pipeline | Done. Goreleaser builds binaries (linux/darwin × amd64/arm64), archives, sha256 sums, CycloneDX SBOMs (syft), cosign keyless signatures on every archive + image. |
| `internal/version` — build-time version stamping via `-ldflags="-X .../internal/version.Version=..."`, fallbacks to `runtime/debug.ReadBuildInfo()` for dev builds | Done. `--version` surfaces it; `/readyz` includes it in the JSON body. |
| Go module proxy support | Not started |
| Debian/apt support | Not started |
| Pull-through proxy / caching | Python, npm, and Maven done; Go/apt and cross-format hardening remain Phase 4 work. |
| Vulnerability scanning | Not started |
| Authorization extensibility — multiple backends (OPA / Cedar / Casbin) | Pluggable via `namespace.AuthzFactory`; only the matcher-based built-in ships in-tree. |
| Cloud Run / Cloudflare deployment guides | Not started |
| Structured request logging | Not started (debug-level request log via `pkg/handler.Loggeer` is present) |
| Rate limiting | Not started |
| CI: lint, test, build, image publish | `go-test` from `abcxyz/pkg`; `oidc-e2e` job mints a real GitHub OIDC token and exercises the auth chain against `--repo-type=echo`; `client-integration` runs separate Python / Maven / npm steps for the `-tags=integration` real-client tests (`twine`, `mvn`, `npm`). A separate `live-upstream` workflow runs separate Python / Maven / npm proxy steps against real PyPI / Maven Central / npm on every PR (intentionally non-hermetic; upstream outages will turn it red). Image publish runs on the release workflow, not per-PR. |

## Architecture (read this before changing things)

```
                     ┌──────────────────────────────────────────┐
HTTP request ──►     │  cmd/ocifactory  (CLI entrypoint)         │
                     │  Subcommands: serve / admin serve         │
                     └───────────────┬──────────────────────────┘
                                     │
                     ┌───────────────▼──────────────────────────┐
                     │  pkg/handler                              │
                     │  • Server (port + middleware chain)       │
                     │  • Logger, MetricsMiddleware              │
                     │  • ObservabilityHandler                   │
                     │    (/healthz, /readyz, /metrics)          │
                     └───────────────┬──────────────────────────┘
                                     │ http.Handler
                     ┌───────────────▼──────────────────────────┐
                     │  pkg/handler/{python,maven,npm,...}       │
                     │  Each speaks one client protocol          │
                     │  (pip, mvn, npm, go mod, apt, ...)        │
                     │  Routes mounted under /{namespace}/...    │
                     │  auth.Middleware (pkg/auth) chained on    │
                     │    the format's root or sub-router        │
                     └───────────────┬──────────────────────────┘
                                     │ handler.Registry interface
                     ┌───────────────▼──────────────────────────┐
                     │  pkg/namespace.ScopedRegistry             │
                     │  • authorizes (read/write) against the    │
                     │    namespace's compiled Policy            │
                     │  • prefixes OwningRepo with <namespace>/  │
                     │  • records writes into the per-namespace  │
                     │    package index (ocifactory-packages)    │
                     └───────────────┬──────────────────────────┘
                                     │ handler.Registry interface
                     ┌───────────────▼──────────────────────────┐
                     │  pkg/oci.Registry                         │
                     │  AddFile / ReadFile / ListTags /          │
                     │  ListFiles / DeleteRepoFiles /            │
                     │  DeleteTagFiles / AppendRefs /            │
                     │  BlobRedirectURL / Ping                   │
                     └───────────────┬──────────────────────────┘
                                     │ ORAS (oras-go/v2)
                     ┌───────────────▼──────────────────────────┐
                     │  Any OCI registry                         │
                     │  (GAR, ECR, GHCR, Harbor, zot, ...)       │
                     └──────────────────────────────────────────┘
```

The on-OCI shape `pkg/oci` writes — version anchors, file manifests,
alias manifests, the deterministic `_f_*` file tag, and why it's not a
fat per-version manifest — is documented in
[`docs/architecture/storage-model.md`](docs/architecture/storage-model.md).
Read that doc before touching the `pkg/oci` write paths.

### Key types

- `oci.RepoFile{OwningRepo, OwningTag, RefTag, Name, MediaType, Digest, Size, AllowOverwrite}` — addresses one file inside the OCI-backed virtual store.
  - `OwningRepo` is the OCI repository name **relative to the namespace** at the handler boundary (e.g. `packages/requests`, `com/foo/bar`). The `namespace.ScopedRegistry` wrapper prefixes it with `<namespace>/` before forwarding to `pkg/oci`, so on the real backend the repo is `<namespace>/packages/requests`.
  - `OwningTag` is the canonical version tag (e.g. `2.31.0`). It identifies the **version manifest** — a constant-size anchor in the [OCI 1.1 referrers layout](docs/architecture/storage-model.md). Files for that version are *not* layers on this manifest; each file is its own **file manifest** with `subject = versionDesc`, addressed via the referrers API and tagged with a deterministic `_f_<sha256>` tag so reads resolve in one round-trip.
  - `RefTag` is an alias tag like `latest`. It identifies an **alias manifest** with `subject = versionDesc` and the canonical version recorded in the `ocifactory.alias.target` annotation.
  - `AllowOverwrite` is a per-file flag handlers set when the file is intentionally mutable (Maven snapshot artifacts, snapshot `maven-metadata.xml`). It overrides the registry-level `--allow-overwrite` default for that one call.
  - The three manifest kinds are distinguished by `artifactType` suffix — `.version`, `.file`, `.alias` — appended to the operator-configured base type (e.g. `application/vnd.ocifactory.python.version`).
- `handler.Registry` — the interface every per-format handler depends on. Keep it minimal; do not push format-specific concepts into it. Implementations are `*oci.Registry` (raw) and `*namespace.ScopedRegistry` (namespaced + authorized, used in production).
- `namespace.Registry` / `namespace.ScopedRegistry` — the data-plane namespace wrapper. `Registry.For(ns)` returns a cheap per-request `ScopedRegistry` that handlers use as their `handler.Registry`. Authorizer instances are cached per namespace (`DefaultPolicyCacheTTL = 60s`) and invalidated automatically when admin-side `Store.Put` / `Store.Delete` fires the mutation hook.
- `namespace.Policy` — the JSON-serialised authz block on a namespace `Spec`. Empty Policy is deny-all. `Readers` and `Writers` are independent `SubjectMatcher` lists; matchers ANDed across fields (`issuer`, `sub_match` regex, `email`, `claims_match`, `kind`).
- `auth.AuthContext{Issuer, ID, Email, Claims}` — the verified caller identity the OIDC authenticator installs into the request context. The `namespace.PolicyAuthorizer` consumes it; out-of-tree authorizers (OPA / Cedar / Casbin) plug in via `namespace.WithAuthzFactory`.
- `auth.Authorizer` / `auth.Op` (`OpRead`, `OpWrite`) — the coarse pluggable authz surface every namespace policy compiles to.

### How a per-format handler is structured

Each `pkg/handler/<format>` package owns:
- `RepoType` constant (`"npm"`, `"python"`, …) — used by `serve` to dispatch.
- `ArtifactType` constant — the OCI manifest `artifactType` base (e.g. `application/vnd.ocifactory.npm`). `pkg/oci` appends `.version` / `.file` / `.alias` to it so the three manifest kinds are distinguishable in the OCI backend.
- `NewHandler(*namespace.Registry, opts ...Option) (*Handler, error)` — takes the namespace wrapper, not a raw `*oci.Registry`, so every request goes through the authorizer.
- `Mux() http.Handler` — gorilla/mux router for that format's URL scheme. Mount every route on a `router.PathPrefix("/{namespace}").Subrouter()` (or `"/{namespace}/<format-prefix>"`) so the handler can read the namespace from `mux.Vars(req)` and call `r.For(ns)` to get a per-request `*namespace.ScopedRegistry`.
- Translation logic mapping client protocol calls ↔ `RepoFile` operations through the scoped registry.

When adding a new format, copy the structure from `pkg/handler/python` (it's the most complete reference, including the embedded simple index template, the per-package simple-index cache, and the dual-write trick that maintains an `index` repo for fast "list packages").

### Namespace URL layout

Every format mounts under `/{namespace}/...`. Some formats add a fixed
sub-prefix after the namespace to make the URL self-describing when a
single hostname serves multiple formats per namespace:

| Format | URL shape |
|---|---|
| python | `/{namespace}/simple/<pkg>/`, `/{namespace}/packages/<pkg>/<version>/<filename>`, `/{namespace}/` for twine upload |
| maven  | `/{namespace}/maven2/<groupId>/<artifactId>/<version>/<filename>` (and the `maven-metadata.xml` / `archetype-catalog.xml` routes) |
| npm    | `/{namespace}/<pkg>`, `/{namespace}/{@scope/name}`, `/{namespace}/-/package/<pkg>/dist-tags/<tag>`, `/{namespace}/-/ping` |

Namespaces have to be created via the [admin API](docs/admin.md) before any
request can land on them — there is no auto-vivification. `ScopedRegistry`
returns `namespace.ErrNotFound` (mapped to 404 by `handler.WriteNamespaceError`)
when the namespace's metadata document is missing.

### Index repos pattern

For formats where you need "list all packages I have" (PyPI simple index, npm registry root, etc.), `python` handler maintains a parallel OCI repo named `index` (relative to the namespace, so `<namespace>/index` on the backend) whose tags are package names. This avoids needing a sidecar database — `ListTags("index")` is the package list. Reuse the pattern when implementing npm/Go/apt.

Separately, `namespace.Registry` itself maintains a per-namespace **package index repo** at `<namespace>/ocifactory-packages` (configurable via `WithPackageIndexSuffix`). It's an in-process LRU-deduped record of every owning-repo the wrapper has written to, used by `admin serve`'s soft-delete to refuse deleting a non-empty namespace. Format handlers don't write to it directly — every `ScopedRegistry.AddFile` call updates it best-effort.

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

# Run locally against a real OCI registry (e.g. local zot or GAR).
# --disable-authn is required for the dev path — Validate refuses to
# start with neither --authn-kind nor --disable-authn set.
go run ./cmd/ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --disable-authn \
  --port=8080

# Then create a namespace via the admin service so the data plane has
# something to serve under.
go run ./cmd/ocifactory admin serve \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8081
```

## Code conventions

- **Languages:** Go is the only implementation language. Avoid pulling in shell scripts when a Go test will do.
- **Style:** Write idiomatic Go. Follow [Effective Go](https://go.dev/doc/effective_go) and the Google Go style guide — [overview](https://google.github.io/styleguide/go/), [style decisions](https://google.github.io/styleguide/go/decisions), [best practices](https://google.github.io/styleguide/go/best-practices). Prefer explicit and boring over clever. `gofmt -s` and `go vet` must be clean before every commit.
- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. Use `errors.Is` / `errors.As` to check. The `pkg/oci` package already exposes `oci.HasCode(err, statusCode)` for translating ORAS HTTP errors — use it in handlers rather than re-deriving status codes.
- **Logging:** `github.com/yolocs/ocifactory/pkg/logging`. Read with `logging.FromContext(ctx)`; configure via `OCIFACTORY_LOG_LEVEL`, `OCIFACTORY_LOG_FORMAT`, `OCIFACTORY_LOG_DEBUG`.
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

### CI integration workflow rules

- Keep GitHub Actions integration jobs split by artifact format when a job can fail for Python, Maven, npm, or future repo types independently. `client-integration` should have one step per real-client test with an exact `-run` filter, and `live-upstream` should have one step per live proxy upstream test with the format-specific build tag. This makes CI failures point at the broken artifact format immediately instead of hiding it inside one combined `go test` invocation.

## Adding a new repo type — checklist

1. Create `pkg/handler/<format>/` with `handler.go`, the `Mux()`, and translation logic.
2. Define `RepoType` and `ArtifactType` constants.
3. **Mount every route under a `/{namespace}` sub-router** so the handler can read the namespace from `mux.Vars(req)` and call `registry.For(ns)` to get a per-request `*namespace.ScopedRegistry`. Optionally prefix routes with a per-format segment (`/{namespace}/maven2/...`, `/{namespace}/simple/...`) when the URL would otherwise be ambiguous against another format on the same hostname.
4. **Accept `WithAuthMiddleware(func(http.Handler) http.Handler)` as an Option** and chain the middleware on whichever routes need authentication. The convention today (python, maven, npm) is `router.Use(mux.MiddlewareFunc(h.authMW))` on the root router (python, maven) or on the `/{namespace}` sub-router (npm) so every route is gated. Public-by-default formats (Go module proxy listings, future read-without-token endpoints) would chain on a sub-router and leave reads ungated; outlier endpoints with their own auth contract sit on a sub-router that doesn't chain it at all. **Do not add an open default**: in `pkg/commands/serve.go`, always pass `WithAuthMiddleware(authMW)` when constructing the handler. The handler-side option is permissive (omitting it leaves routes ungated, which is what tests want), so the gate against silent-no-auth lives in serve.go — verify it's wired before merging.
5. **Take a `*namespace.Registry` in `NewHandler`**, not a `*oci.Registry`. Every backend op flows through the scoped view so the namespace's compiled `Policy` runs on every read and write. Map `namespace.ErrNotFound`, `namespace.ErrInvalidName`, `namespace.ErrInvalidOwningRepo`, and `auth.ErrUnauthorized` to HTTP via `handler.WriteNamespaceError` — there's a shared helper because every format needs the same translation.
6. Plumb it into `pkg/commands/serve.go`'s `supportedRepoTypes` and the `switch` in `Run`. Build the `*namespace.Registry` next to the format-specific `*oci.Registry`, pass it to the handler constructor along with `WithAuthMiddleware(authMW)`.
7. Add handler tests using the `oci.fake` backend (cover happy path + 404 + auth errors at minimum). Add a `pkg/handler/<format>/auth_test.go` that mirrors `pkg/handler/python/auth_test.go`: a deny-all middleware reaches every route, omitting the option leaves routes ungated, and the middleware chains before the route handler runs. Add a `pkg/handler/<format>/namespace_test.go` mirroring `pkg/handler/python/namespace_test.go`: unknown namespace → 404, deny-all policy → 403, cross-namespace request can't reach another namespace's data.
8. Add an integration test or a documented manual test against a real client (`pip`, `mvn`, `npm install`, `go mod download`, `apt-get`). The harness in `pkg/handler/integrationtest/` seeds a `default` namespace via `Store.Put` before the subprocess starts; copy that pattern.
9. Document the format under `docs/repos/<format>.md`: URL layout (including the `/{namespace}/` prefix), supported client commands, known limitations. Copy [`docs/repos/_template.md`](docs/repos/_template.md) — it has the headings and depth `python.md` / `maven.md` / `npm.md` use, plus inline notes on what to call out front-and-center (e.g. operator footguns that break the second invocation of the most common client command).
10. Update the status table in this file.

## Roadmap (one step at a time)

The intent is to ship each phase production-ready before starting the next. See `docs/ROADMAP.md` for the long form.

1. **Phase 1 — Core formats:** Go module proxy → Debian/apt. (Python, Maven, npm done.)
2. **Phase 2 — Deployability:** Cloud Run + Cloudflare deployment guides, structured request logging, rate limiting. (Distroless image, multi-arch publishing, Goreleaser pipeline, Prometheus metrics, signed releases + SBOMs done.)
3. **Phase 3 — Auth/Authz extensibility:** pluggable authn (OIDC) and per-namespace authz are shipped. Remaining: more authorizer plugins (OPA / Cedar / Casbin examples) and scoped token issuance.
4. **Phase 4 — Pull-through proxy:** read-through caching from upstream npm/PyPI/Maven Central/proxy.golang.org/Debian mirrors. Cache TTLs, negative caching, immutable-version pinning.
5. **Phase 5 — Add-ons:** vulnerability scanning (Grype/Trivy integration), retention policies, web UI.

## GitHub workflow

- **Issues are the source of truth for tasks.** Open an issue before non-trivial work; link it from the PR.
- **Always pull latest first.** Before starting any issue, feature, fix, review follow-up, or branch/worktree creation, update the primary checkout with `git pull --ff-only` from `main`. If it cannot fast-forward cleanly, stop and resolve that state before doing task work.
- **Delete merged worktrees.** Once a PR is merged, remove its feature worktree from the primary checkout with `git worktree remove ../ocifactory-<short-topic>` and delete the local branch with `git branch -D <short-topic>`. Do this before starting unrelated work so stale worktrees do not accumulate.
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
   1. **Create a dedicated git worktree** for the change. Every feature, fix, or refactor lands in its own worktree on its own branch — never reuse an existing checkout, and never stack unrelated work on the same branch. Branch name is `<short-topic>` (e.g. `npm-publish`, `go-proxy-list`); worktree path is `../ocifactory-<short-topic>` (sibling of the primary checkout). Create both in one shot from an up-to-date `main`:
      ```bash
      git fetch origin main
      git worktree add -b <short-topic> ../ocifactory-<short-topic> origin/main
      cd ../ocifactory-<short-topic>
      ```
      Do all subsequent work — edits, commits, `go test`, `gh pr create`, CI-fix pushes — from inside that worktree. When the PR is merged (or abandoned), clean up with `git worktree remove ../ocifactory-<short-topic>` and `git branch -D <short-topic>` from the primary checkout. Rationale: keeps the main checkout free for parallel reviews / hotfixes, makes "which change am I touching?" unambiguous, and prevents accidental cross-contamination between in-flight branches.
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
- Changing the `namespace.Spec` / `namespace.Policy` JSON shape — bump `CurrentSchemaVersion` and add a read-side migration so older bodies stay loadable; never silently drop fields.
- Adding a new auth mechanism or `Op` value — discuss the interface shape first. Authenticator implementations must return one of the sentinel errors (`ErrNoCredential`, `ErrInvalidToken`, `ErrIssuerUnavailable`) so the middleware maps to the right HTTP status; authorizers must wrap `auth.ErrUnauthorized` on deny.
- Anything that breaks backward compatibility of existing `--repo-type=python|maven|npm` users — incl. the URL shape, the on-OCI layout, or the admin API.

## Out of scope

- Hosting our own OCI registry. We integrate with existing ones; we don't reimplement one.
- Full Artifactory feature parity. We aim for a useful subset, well-executed, not a checkbox match.
- A web UI in Phase 1–3. Defer until the API surface is stable.
