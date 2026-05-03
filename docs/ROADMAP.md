# Roadmap

The guiding rule: **ship each phase production-ready before starting the next.**
"Production-ready" means: unit tests, integration test against the real client
tool where applicable, docs in `docs/repos/<format>.md` (or equivalent), and at
least one real end-to-end run documented in the PR.

## Phase 1 — Core formats

Round out the format coverage that motivates the project.

- [ ] **npm** — finish the stub handlers in `pkg/handler/npm/`. PEP-equivalent
  endpoints: package metadata, version metadata, tarball download, publish,
  unpublish, dist-tags. Use the `index` repo pattern from `python` for
  package listing. Validate against `npm`, `yarn`, and `pnpm`.
- [ ] **Go module proxy** — implement the `GOPROXY` protocol
  (`/<module>/@v/list`, `/<module>/@v/<version>.info|.mod|.zip`,
  `/<module>/@latest`). Use immutable version tags (`v1.2.3` → OCI tag).
- [ ] **Debian / apt** — serve a flat repo (or full pool/dist layout) backed
  by OCI. `Packages`/`Release`/signing files become OCI manifests; `.deb`
  files become layers.

## Phase 2 — Deployability

Make it trivial to run. Without this, nobody adopts it.

- [ ] Dockerfile (distroless or alpine, < 30 MB image)
- [ ] CI pipeline: lint (`golangci-lint`), unit tests, integration tests, image
  build + push (GHCR) on tag.
- [ ] Cloud Run deployment guide + sample Terraform.
- [ ] Cloudflare Workers/Containers deployment guide.
- [ ] Health check endpoint, Prometheus metrics, structured request logs.
- [ ] Rate limiting hooks (interface, default no-op).

## Phase 3 — Auth/Authz extensibility

The pass-through basic auth model gets us off the ground but won't survive a
real deployment. This phase makes auth pluggable.

- [ ] Define `Authenticator` and `Authorizer` interfaces in `pkg/auth/`.
- [ ] Built-in implementations:
  - Pass-through basic auth (today, kept for back-compat).
  - OIDC / JWT verifier (issuer + JWKS URL config).
  - Static token list (env var or file).
- [ ] Authorization model: scope = `(repo, format, op)` where `op ∈ {read, write, delete}`.
- [ ] Middleware in `pkg/handler` consumes the interfaces; concrete
  implementations register at startup.
- [ ] Documented "how to plug in your own authorizer" recipe.

## Phase 4 — Pull-through proxy / cache

Turn ocifactory into a caching mirror in front of upstream registries.

- [ ] Per-format upstream client (npm registry, PyPI JSON API, Maven Central,
  `proxy.golang.org`, Debian mirrors).
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
