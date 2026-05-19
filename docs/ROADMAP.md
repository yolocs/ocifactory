# Roadmap

The guiding rule: **ship each phase production-ready before starting the next.**
"Production-ready" means: unit tests, integration test against the real client
tool where applicable, docs in `docs/repos/<format>.md` (or equivalent), and at
least one real end-to-end run documented in the PR.

## Phase 1 — Core formats

Round out the format coverage that motivates the project.

- [x] **npm** — `npm publish` (scoped + unscoped), `npm install` (with
  dist-tag and version specifiers), `npm dist-tag add` / `ls`. Tarball
  URLs rewritten per request so the `dist.tarball` field always points
  at ocifactory. Validated against `npm`, `yarn`, and `pnpm`. Operator
  docs: [`repos/npm.md`](repos/npm.md).
- [ ] **Go module proxy** — implement the `GOPROXY` protocol
  (`/<module>/@v/list`, `/<module>/@v/<version>.info|.mod|.zip`,
  `/<module>/@latest`). Use immutable version tags (`v1.2.3` → OCI tag).
- [ ] **Debian / apt** — serve a flat repo (or full pool/dist layout) backed
  by OCI. `Packages`/`Release`/signing files become OCI manifests; `.deb`
  files become layers.

## Phase 2 — Deployability

Make it trivial to run. Without this, nobody adopts it.

- [x] **Dockerfile and multi-arch container image.** Distroless `nonroot`
  base, < 30 MB image. Published to `ghcr.io/yolocs/ocifactory:vX.Y.Z`
  (+ `:vX.Y` / `:latest` for stable releases). See
  [`../Dockerfile.goreleaser`](../Dockerfile.goreleaser).
- [x] **Release pipeline.** Manually-dispatched GitHub Actions workflow
  drives Goreleaser. Per release: binaries for `linux/amd64`,
  `linux/arm64`, `darwin/amd64`, `darwin/arm64`; CycloneDX SBOMs;
  cosign keyless signatures on every archive and image. See
  [`../RELEASING.md`](../RELEASING.md).
- [x] **CI pipeline.** `go-test` (full suite incl. live-zot integration
  test) on every PR; `oidc-e2e` job mints a real GitHub Actions OIDC
  token and exercises the auth chain; `client-integration` job runs
  the `-tags=integration` real-client tests for python, maven, and
  npm.
- [x] **Health check, readiness, metrics.** `/healthz`, `/readyz`
  (caches a 1s backend probe; returns build info), `/metrics`
  (Prometheus exposition with HTTP- and OCI-backend-layer
  instrumentation). See [`observability.md`](observability.md).
- [x] **Build-time version stamping.** `--version` surfaces
  `ocifactory vX.Y.Z (commit <sha>, <os>/<arch>)`; same info appears
  in the `/readyz` JSON body so multi-instance deployments are
  diagnosable per-pod. See `internal/version`.
- [ ] Cloud Run deployment guide + sample Terraform.
- [ ] Cloudflare Workers/Containers deployment guide.
- [ ] Structured request logs (debug-level request log via
  `pkg/handler.Loggeer` exists; structured-with-fields request log
  doesn't).
- [ ] Rate limiting hooks (interface, default no-op).

## Phase 3 — Auth/Authz extensibility

The pluggable auth model is partially landed and powers production
deployments today. Remaining work is more authorizer backends and
scoped-token issuance.

- [x] **`Authenticator` interface in `pkg/auth/`.** OIDC implementation
  in `pkg/auth/oidc/`. `pkg/auth/chain/` composes multiple authenticators
  so an instance can accept tokens from several issuers (Google service
  accounts + GitHub Actions + …).
- [x] **`Authorizer` interface in `pkg/auth/`.** `OpRead` / `OpWrite`
  ops. Default in-tree implementation is `namespace.PolicyAuthorizer`
  — a matcher-based authorizer compiled from each namespace's `Policy`
  block (`readers` / `writers` `SubjectMatcher` lists). Out-of-tree
  authorizers (OPA / Cedar / Casbin) plug in via
  `artifact.WithAuthzFactory`.
- [x] **Pluggable backend credential `Provider`.** In-tree adapters:
  `anonymous`, `gcpadc` (ADC), `staticenv` (env vars, re-read every
  call), `dockerconfig` (honours credential helpers). See
  [`auth.md`](auth.md#backend-how-ocifactory-talks-to-the-oci-registry).
- [x] **Namespace control plane.** `ocifactory admin serve` exposes
  `PUT/GET/DELETE /admin/v1/namespaces/{name}` against an OCI-backed
  store. See [`admin.md`](admin.md).
- [x] **Per-namespace authorization model.** `(namespace, op)` where
  `op ∈ {read, write}`. Every data-plane request matches against the
  namespace's compiled `Policy`; authorizer cache invalidates on admin
  Put/Delete. Documented in [`auth.md`](auth.md#namespace-authorization).
- [x] **OIDC-only frontend authentication.** Static passwords are
  intentionally out-of-tree — out-of-tree authenticators implement
  `auth.Authenticator` and slot in via a custom main. See
  [`auth.md`](auth.md).
- [ ] **Scoped tokens.** A token-mint endpoint that takes a verified
  OIDC token and issues a shorter-lived `aud=ocifactory` token bound
  to a single namespace and op. Open question on the wire shape.
- [ ] **Example out-of-tree authorizer.** A small reference for OPA or
  Cedar so operators who already run one don't need to reverse-engineer
  the interface.
- [ ] **Documented "how to plug in your own authorizer" recipe.**
  Today it's "implement `auth.Authorizer`, pass it via
  `artifact.WithAuthzFactory`"; the surface is small enough that a
  recipe in [`auth.md`](auth.md) is sufficient.

## Phase 4 — Pull-through proxy / cache

Turn ocifactory into a caching mirror in front of upstream registries.

- [ ] Per-format upstream client (npm registry, PyPI JSON API, and Maven
  Central are done; `proxy.golang.org` and Debian mirrors remain).
- [ ] Cache semantics:
  - Immutable artifacts (specific versions): cache forever.
  - Mutable indexes (npm package doc, PyPI simple index): TTL.
  - Negative cache for 404s (short TTL).
- [ ] Cache eviction strategy (LRU on backing OCI repo? rely on registry GC?).
- [ ] "Virtual repos" combining proxied upstream + local hosted in one URL.

## Phase 5 — Add-ons

Anything we'd bolt on once the core is solid.

- [ ] Vulnerability scanning (Grype/Trivy on push or on demand).
- [ ] Retention policies (keep last N versions, delete > 90 days, etc.).
- [ ] Read-only web UI for browsing repos.
- [ ] Replication / mirror-this-ocifactory-to-that-ocifactory.

## Non-goals

- Reimplementing the OCI distribution spec ourselves. We integrate.
- Feature parity with Artifactory. We aim for an opinionated useful subset.
- General-purpose blob storage abstraction over non-OCI backends. The OCI
  constraint is a feature, not a limitation to route around.
- Static passwords / shared HTTP basic auth as a first-class auth mode.
  An OIDC issuer is reachable from every realistic 2026 deployment;
  out-of-tree authenticators cover the edge cases.
