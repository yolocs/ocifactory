# ocifactory

A lightweight, multi-format artifact registry that uses any OCI registry as its
sole storage backend. Think of it as a small, open-source drop-in for
Artifactory or Nexus — for users who can't or don't want to run them.

> **Status:** experimental / pre-1.0. Maven and PyPI are functional; npm is in
> progress; Go module proxy and apt are planned. See [`AGENTS.md`](AGENTS.md)
> for the full status table.

## Why

If you already run an OCI registry (GAR, ECR, GHCR, Harbor, zot, …) you have
content-addressable, authenticated, replicated blob storage. ocifactory turns
that into a real artifact server speaking the protocols clients already know
(`pip`, `mvn`, `npm`, `go mod`, `apt`), without standing up a second stateful
service.

- **Single stateless binary.** No Postgres, no S3, no separate metadata DB.
- **Storage is your OCI registry.** Bring your own. We only need v2 API.
- **Lean ops.** Designed to deploy on Cloud Run or Cloudflare in minutes.
- **Pluggable auth.** Pass-through basic auth today; OIDC/JWT and a custom
  authorizer interface on the roadmap.

## Supported formats

| Format | Client | Status | Docs |
|---|---|---|---|
| Maven | `mvn`, `gradle` | ✅ Functional | [`docs/repos/maven.md`](docs/repos/maven.md) |
| PyPI  | `pip`, `twine`  | ✅ Functional | [`docs/repos/python.md`](docs/repos/python.md) |
| npm   | `npm`, `yarn`, `pnpm` | 🚧 In progress (routes wired, handlers pending) | — |
| Go module proxy | `go mod`        | ⏳ Planned | — |
| Debian / apt    | `apt-get`       | ⏳ Planned | — |
| Pull-through caching of upstreams | — | ⏳ Phase 4 | — |

Per-format operator guides live under [`docs/repos/`](docs/repos/) — URL
layouts, supported client commands, OCI storage shape, knobs, and known
limitations.

## Quickstart

```bash
go build ./...

# Point at any OCI registry — local zot, GAR, ECR, GHCR, ...
go run ./cmd/ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8080

# In another shell
pip install --index-url http://localhost:8080/simple/ requests
```

Every runtime knob is a CLI flag with a matching env var — no config files.
Common ones: `PORT`, `OCIFACTORY_REPO_TYPE`, `OCIFACTORY_BACKEND_REGISTRY`,
`OCIFACTORY_AUTHN_*`, `OCIFACTORY_BACKEND_AUTH_*`, `OCIFACTORY_LOG_LEVEL`,
`OCIFACTORY_LOG_FORMAT`. See [`docs/auth.md`](docs/auth.md) for the auth
options and [`docs/repos/`](docs/repos/) for per-format guides.

## Architecture

```
client ──► handler/<format> ──► pkg/oci.Registry ──► OCI registry (ORAS)
```

One Go binary. One process. Storage offloaded entirely to your OCI registry.
Each artifact format gets its own `pkg/handler/<format>` package implementing
the relevant protocol. See [`AGENTS.md`](AGENTS.md) for the full design notes
and [`docs/ROADMAP.md`](docs/ROADMAP.md) for what's coming.

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

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
