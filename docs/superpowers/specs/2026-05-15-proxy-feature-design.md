# Pull-through proxy — design

Date: 2026-05-15
Status: approved, decomposed into 10 implementation issues
Phase: [ROADMAP](../../ROADMAP.md) Phase 4 (pull-through proxy / cache)

## Goal

Turn ocifactory into a caching mirror in front of upstream public
registries (PyPI, npm, Maven Central). A *proxy namespace* serves
files fetched on-demand from upstream, stores them in the same
OCI-backed shape as a hosted namespace ([storage model](../../architecture/storage-model.md)),
and applies operator-defined governance (allowlist/denylist, delays)
before any upstream call.

Out of scope for v1: virtual repos (overlay of hosted + proxy in one
URL), authenticated upstreams, pull-through for Go module proxy and
apt (these arrive with their own phase-1 hosted handlers first).

## Design pillars

1. **Hosted and proxy are disjoint namespace modes**, not mixed within
   one namespace. A namespace is `mode: hosted` or `mode: proxy`. A
   later `mode: virtual` can compose the two without changing either.
2. **The OCI storage model is the same.** A proxied file lands as a
   file manifest under the same version-manifest anchor as a hosted
   one. The proxy code path is "miss → fetch upstream → write referrer
   → serve" — it never invents a new on-OCI shape.
3. **Per-file is the unit of caching.** Clients fetch one file at a
   time; a half-cached version is half-useful, not broken. Atomic
   "whole version" caching adds transactional complexity without a
   benefit clients can observe.
4. **Indexes pass through live, snapshot for resilience.** When
   upstream is reachable we serve its index verbatim (with our URLs
   rewritten where required). We also periodically snapshot the
   upstream index to OCI so we can keep serving during an outage.
5. **Filters are a chain, run as early as their inputs allow.** Name
   filters (allow/deny) run before any upstream call. Metadata
   filters (publish-time delay) run after upstream metadata fetch but
   before file fetch.

## Namespace shape

`namespace.Spec` gains:

```go
type Spec struct {
    SchemaVersion int    `json:"schema_version,omitempty"`
    Mode          string `json:"mode,omitempty"` // "hosted" | "proxy"; empty == "hosted"
    Policy        Policy `json:"policy,omitzero"`
    Proxy         Proxy  `json:"proxy,omitzero"` // only honored when mode == "proxy"
    Format        map[string]json.RawMessage `json:"format,omitempty"`
}

type Proxy struct {
    Upstream string   `json:"upstream,omitempty"` // canonical URL, e.g. "https://pypi.org"
    Filters  []Filter `json:"filters,omitempty"`
    IndexSnapshot IndexSnapshot `json:"index_snapshot,omitzero"`
}
```

Empty `mode` resolves to `hosted` so existing namespaces load
unchanged. Validation rejects `mode: proxy` without `proxy.upstream`,
and any `proxy` block on a `hosted` namespace.

The schema version stays at 1: the new fields are additive and
backwards-readable by older binaries (per the `Format` rule already in
the spec).

## Architecture

```
                 Per-format handler (python, npm, maven)
                              │
                              ▼
                     ┌─────────────────┐
       index path:   │  serveIndex     │── live HTTP GET to upstream (with rewriting)
                     │                 │── fall back to local snapshot when upstream errors
                     └─────────────────┘
                              │
       file path:    ┌─────────────────┐
                     │  serveFile      │
                     └────────┬────────┘
                              │ 1. registry.ReadFile (cache hit → done)
                              │ 2. filter chain (name → upstream metadata → publish-time)
                              │ 3. fetcher.FetchFile (upstream HTTP GET)
                              │ 4. registry.AddFile (write referrer, then stream to client)
                              ▼
                       pkg/proxy.Fetcher
                              │
                              ▼
                       Upstream registry
```

Three new packages:

- **`pkg/proxy`** — `Fetcher` interface and shared HTTP client utilities
  (timeouts, retries, conditional GET, redirect handling, error
  classification). One in-tree implementation per format lives under
  `pkg/proxy/<format>` (PyPI for python, npm registry for npm, Maven
  Central layout for maven).
- **`pkg/proxy/filter`** — `Filter` interface, chain composition, and
  in-tree filters (`allowlist`, `denylist`, `delay`).
- **`pkg/proxy/snapshot`** — periodic index snapshotter and the read
  path that serves the snapshot during upstream outages.

The per-format handlers gain a small adapter that wires together
their existing `handler.Registry` calls with the new `Fetcher`,
`Filter`, and snapshot pieces. No changes to `handler.Registry` itself.

## Per-file caching: how a request flows

For `GET /<ns>/simple/requests/requests-2.31.0-py3-none-any.whl`:

1. Resolve namespace `<ns>` → confirm `mode: proxy`.
2. Run cheap filters on `requests` (denylist by name). If denied,
   return 404 + log + counter.
3. `registry.ReadFile(...requests-2.31.0-py3-none-any.whl)` →
   a. Hit → stream from OCI to client. Done.
   b. Miss → continue.
4. `fetcher.GetVersionMetadata(ctx, "requests", "2.31.0")` →
   gives us the canonical file URL and `upload_time`. Cached short-TTL
   in memory keyed by `(package, version)`.
5. Run metadata-dependent filters (publish-time delay). If denied,
   return 404.
6. `fetcher.FetchFile(ctx, fileURL)` → opens the upstream stream.
   7. Tee through `registry.AddFile` (writes file manifest as a
   referrer to the `2.31.0` version manifest, creating the version
   manifest if absent) while streaming to the client. On AddFile
   error, abort and bubble 502 to client; the next request retries.

Negative cache (in-memory LRU) covers "this fileURL 404s upstream"
for a short TTL (default 60s) so a busted client request doesn't
hammer upstream.

## Index strategy

Two paths, picked per request:

- **Live**: GET upstream's index, rewrite any absolute file URLs to
  point back at this namespace, stream to client. This is the default.
- **Snapshot fallback**: when the live GET errors or times out within
  the client deadline, return the most recent snapshot from OCI.

The snapshotter runs as a background loop per proxy namespace (interval
configurable, default 5m). It writes the raw upstream index body as a
file manifest under a reserved `index-snapshot/<pkg>` repo with a
deterministic tag (`current`) — same storage primitives, no new on-OCI
shape. Filter-denied packages are excluded from the snapshot at write
time so they stay denied even during an outage.

Snapshot is best-effort: if the snapshotter has never run for a
package and upstream is down at first request, we 503. This is
operationally rare and the simpler choice.

## Filter chain

```go
type Filter interface {
    Allow(ctx context.Context, ref Ref) (Decision, error)
}

type Ref struct {
    Package     string
    Version     string    // empty if filter runs before version resolution
    UploadTime  time.Time // zero if not yet known
}

type Decision int
const (
    DecisionAllow Decision = iota
    DecisionDeny
    DecisionNeedsMoreData // skip this filter for now; re-run after metadata fetch
)
```

In-tree filters:
- `allowlist`: deny everything not on the list (name-only match, exact + glob).
- `denylist`: deny everything on the list (name-only match, exact + glob).
- `delay`: deny if `now() - UploadTime < N`. Returns `NeedsMoreData`
  pre-metadata, evaluates after step 4.

Filters compose ordered; first deny wins. All denies are logged with
the filter name and counted via a `proxy_filter_deny_total{filter}`
metric.

## Observability

New metrics (Prometheus):
- `proxy_fetch_total{format, result}` — result ∈ `hit|miss|upstream-error|filter-deny`
- `proxy_fetch_duration_seconds{format, result}`
- `proxy_index_source_total{format, source}` — source ∈ `upstream|snapshot|empty-fallback`
- `proxy_filter_deny_total{filter}`
- `proxy_snapshot_age_seconds{format, package}` (gauge)
- `proxy_negative_cache_size{format}` (gauge)

Structured logs at info on every fetch with `package`, `version`,
`file`, `upstream`, `result`, `duration_ms`.

## Decomposition into issues

| # | Issue | Depends on |
|---|---|---|
| 1 | Namespace spec: `mode`, `proxy` block, validation, admin CRUD, docs | — |
| 2 | `pkg/proxy`: Fetcher interface + shared HTTP client utilities | — |
| 3 | `pkg/proxy/filter`: Filter interface + allowlist/denylist/delay | 1 |
| 4 | `pkg/proxy/snapshot`: index snapshotter and read-fallback | 1, 2 |
| 5 | Negative cache (in-memory LRU) | 2 |
| 6 | `pkg/proxy/python` + integrate into `pkg/handler/python` | 2, 3, 4, 5 |
| 7 | `pkg/proxy/npm` + integrate into `pkg/handler/npm` | 2, 3, 4, 5 |
| 8 | `pkg/proxy/maven` + integrate into `pkg/handler/maven` | 2, 3, 4, 5 |
| 9 | Observability (metrics + structured logs across the proxy stack) | 2 |
| 10 | Integration tests against real upstreams + cross-format `docs/repos/proxy.md` | 6, 7, 8 |

Issues 1, 2, 3, 5, 9 can land in parallel. Issues 4 and the
per-format integrations (6, 7, 8) sequence after their listed
dependencies merge.

## Open questions deferred to implementation

- **Snapshot scope.** Snapshot every package ever fetched, or only
  packages that meet an "active" threshold (fetched in last N days)?
  Start with every-package; revisit if OCI footprint becomes a
  concern.
- **Filter glob syntax.** Glob (`requests*`) is enough for v1; revisit
  if operators want regex or version ranges.
- **Upstream auth.** Out of scope for v1. The Fetcher interface should
  accept an optional credential reference so adding it later doesn't
  reshape the interface.

## Non-goals

- Authenticated upstreams (deferred).
- Pull-through for Go module proxy / apt (these formats need their
  hosted handlers first).
- Virtual repos overlaying hosted + proxy on one URL (separate spec
  once proxy is solid).
- Per-package cache eviction policy. v1 relies on the operator's OCI
  registry GC; we add eviction in Phase 5 (retention policies).
