# ocifactory

A lightweight, multi-format artifact registry that uses any OCI registry as its
sole storage backend. Think of it as a small, open-source drop-in for
Artifactory or Nexus — for users who can't or don't want to run them.

> **Status:** experimental / pre-1.0. Python, Maven, and npm are functional and
> covered by real-client integration tests; Go module proxy and apt are
> planned. See [`AGENTS.md`](AGENTS.md) for the full status table.

## Why

If you already run an OCI registry (GAR, ECR, GHCR, Harbor, zot, …) you have
content-addressable, authenticated, replicated blob storage. ocifactory turns
that into a real artifact server speaking the protocols clients already know
(`pip`, `mvn`, `npm`, `go mod`, `apt`), without standing up a second stateful
service.

- **Single stateless binary.** No Postgres, no S3, no separate metadata DB.
- **Storage is your OCI registry.** Bring your own. We only need v2 API.
- **Lean ops.** Single Go binary, distroless container image, deployable to
  Cloud Run or Cloudflare in minutes. Signed releases + SBOMs.
- **Pluggable auth.** OIDC-only frontend authentication (Google, GitHub
  Actions, dex, keycloak, …) and a pluggable per-namespace authorizer for
  read/write policy.

## Supported formats

| Format | Client | Status | Docs |
|---|---|---|---|
| Python (PyPI) | `pip`, `twine` | ✅ Functional | [`docs/repos/python.md`](docs/repos/python.md) |
| Maven         | `mvn`, `gradle` | ✅ Functional | [`docs/repos/maven.md`](docs/repos/maven.md) |
| npm           | `npm`, `yarn`, `pnpm` | ✅ Functional | [`docs/repos/npm.md`](docs/repos/npm.md) |
| Go module proxy | `go mod`     | ⏳ Planned | — |
| Debian / apt    | `apt-get`    | ⏳ Planned | — |
| Pull-through caching of upstreams | — | ⏳ Phase 4 | — |

Per-format operator guides live under [`docs/repos/`](docs/repos/) — URL
layouts, supported client commands, OCI storage shape, knobs, and known
limitations.

## Namespaces

Every URL ocifactory serves lives under a namespace prefix that an operator
creates ahead of time via the [admin API](docs/admin.md):

```
https://ocifactory.your-domain/{namespace}/simple/...        # python
https://ocifactory.your-domain/{namespace}/maven2/...        # maven
https://ocifactory.your-domain/{namespace}/<pkg>             # npm
```

Each namespace carries its own authorization policy
(`readers` / `writers` matchers against the OIDC `iss` + `sub` + claims) and
its own slice of OCI repos, so the same ocifactory instance can host disjoint
teams without leaking artifacts between them. See
[`docs/admin.md`](docs/admin.md) for the API, [`docs/auth.md`](docs/auth.md) for
the policy model.

## Quickstart

```bash
go build ./...

# Point at any OCI registry — local zot, GAR, ECR, GHCR, ...
ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --disable-authn \
  --port=8080
```

Then, in another shell, create a namespace via the admin service and use it:

```bash
# Start the admin service (separate listener, no internal auth — see docs/admin.md).
ocifactory admin serve \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8081 &

# Create a namespace with a permissive policy (dev-only).
curl -X PUT http://localhost:8081/admin/v1/namespaces/team-a \
  -H 'content-type: application/json' \
  -d '{"policy":{"readers":[{"issuer":"anonymous"}],"writers":[{"issuer":"anonymous"}]}}'

# Install through the namespace.
pip install --index-url http://localhost:8080/team-a/simple/ requests
```

Every runtime knob is a CLI flag with a matching env var — no config files.
Common ones: `PORT`, `OCIFACTORY_REPO_TYPE`, `OCIFACTORY_BACKEND_REGISTRY`,
`OCIFACTORY_AUTHN_*`, `OCIFACTORY_BACKEND_AUTH_*`, `OCIFACTORY_LOG_LEVEL`,
`OCIFACTORY_LOG_FORMAT`. See [`docs/auth.md`](docs/auth.md) for the auth
options and [`docs/repos/`](docs/repos/) for per-format guides.

## Architecture

```
client ──► handler/<format> ──► artifact.Store ──► pkg/oci.Registry ──► OCI registry (ORAS)
```

One Go binary. One process. Storage offloaded entirely to your OCI registry.
Each artifact format gets its own `pkg/handler/<format>` package implementing
the relevant protocol; every request resolves its `{namespace}` URL segment
into a per-namespace `ScopedNamespace` view that authorizes the operation
against the namespace's policy and prefixes OCI repos with the namespace.

For the on-OCI shape — version anchors, file manifests, alias manifests, and
why ocifactory uses the OCI 1.1 referrers layout instead of one fat manifest
per version — see
[`docs/architecture/storage-model.md`](docs/architecture/storage-model.md).
See [`AGENTS.md`](AGENTS.md) for the full design notes and
[`docs/ROADMAP.md`](docs/ROADMAP.md) for what's coming.

## Running

### Container image

Signed multi-arch images live at `ghcr.io/yolocs/ocifactory:vX.Y.Z` (and
rolling `:vX.Y` / `:latest` for stable releases). Verify the signature with
cosign — see [`RELEASING.md`](RELEASING.md) for the exact command and identity
regex.

### Binary

Release archives (`linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`) are published on each GitHub Release with sha256 checksums,
CycloneDX SBOMs, and cosign keyless signatures.

## Contributing

- Issues track work — open one before non-trivial PRs and link with `Closes #N`.
- Every feature lands with tests. We use an in-memory OCI fake (`pkg/oci/fake.go`)
  for handler tests; no real registry needed for unit tests.
- See [`AGENTS.md`](AGENTS.md) for code conventions, the per-format handler
  checklist, and guidance on what to ask about before changing.

### Running the tests

```bash
# Full suite. The streaming-upload integration test under pkg/oci spins up
# a real zot registry via testcontainers-go, so a Docker daemon must be
# reachable. CI runs this on every PR. If Docker isn't available the
# integration test self-skips with a clear message; the rest still runs.
go test -race ./...

# Skip the live-zot test for fast local iteration or environments without
# Docker. Everything else runs unchanged.
go test -race -short ./...

# Race detector + coverage, same flags CI uses (sans -short):
go test -count=1 -race -shuffle=on -coverprofile=coverage.out ./...
```

Per-format real-client integration tests (`twine`, `mvn`, `npm`) live behind
the `integration` build tag and run in the `client-integration` CI job:

```bash
go test -tags=integration ./pkg/handler/...
```

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
