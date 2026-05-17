# Proxy filter policy

This doc describes how proxy namespaces decide whether to fetch and
serve a given upstream package version. Filters apply only to **file
downloads** — index requests are always passed through unfiltered.

## Where filters live

Filters are configured on a proxy namespace's `Spec.proxy.filters`:

```json
{
  "mode": "proxy",
  "proxy": {
    "upstream": "https://registry.npmjs.org",
    "filters": [
      {"kind": "allow", "patterns": ["@myorg/*"]},
      {"kind": "deny", "rules": [{"package": "log4j-core", "version": "2.14.*"}]},
      {"kind": "delay", "min_age": "24h"}
    ]
  }
}
```

The chain is an **ordered** list. Each filter is one of three kinds:

| Kind | What it does |
|---|---|
| `allow` | If the ref matches an entry, return **allow** (short-circuit). Otherwise abstain. |
| `deny`  | If the ref matches an entry, return **deny** (short-circuit). Otherwise abstain. |
| `delay` | If the version was published at least `min_age` ago, allow. If younger, deny. If the upload time isn't known yet, fetch upstream metadata and re-run. |

## How the chain runs

For every **file download** request the chain is evaluated:

1. The first filter that returns **allow** or **deny** wins — the
   chain short-circuits and the request is passed through or rejected
   accordingly.
2. A filter that **abstains** (doesn't match) advances to the next
   filter.
3. If every filter in the chain abstains, the request is **allowed**.

Index requests (no specific version pinned — e.g. the PyPI simple
index for a package, the npm package metadata document, the Maven
`maven-metadata.xml`) are **not** evaluated against the filter chain.
Clients see the upstream index as-is; per-version blocking happens at
download time.

This matters because:

- An operator who denies `log4j-core@2.14.*` doesn't need to also
  strip those versions from the index document. Pip / Maven / npm
  will see them listed, attempt to download, and get a 404 — with
  a clear policy reason in the server log.
- The same allowlist entry works for both indexes and downloads,
  even though only downloads run through the chain (the index is
  served upstream unchanged either way).

## Filter shapes

### `allow` and `deny`

Both kinds accept two parallel input shapes, ORed together:

- `patterns`: a list of package-name globs. Each pattern is the
  equivalent of a rule with only `package` set.
- `rules`: a list of `{package, version}` entries. At least one of
  the two fields must be set; the other defaults to "match
  anything".

```json
{"kind": "deny",
 "patterns": ["evil-*"],
 "rules": [
   {"package": "log4j-core", "version": "2.14.*"},
   {"package": "log4j-core", "version": "2.15.*"}
 ]}
```

Globs use `path.Match` semantics: `*` matches a single path segment
(does not cross `/`), `?` matches one char, `[abc]` matches a
character class. Exact-string entries match literally.

A direct call to `Allowlist.Decide` / `Denylist.Decide` with
`Ref.Version == ""` ignores any rule's `version` constraint — the
rule falls back to its `package` check alone. The chain entry point
never invokes filters with an empty `Ref.Version`, so this only
matters to in-process callers exercising filters directly.

### `delay`

A single `min_age` duration string (Go's
[`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) format):

```json
{"kind": "delay", "min_age": "24h"}
```

Decision:

- Upload time unknown → **needs more data** (the caller fetches
  upstream metadata and re-runs).
- Version younger than `min_age` → **deny**.
- Version at least `min_age` old → **allow**.

`min_age` must be positive. Common values: `24h`, `72h`, `7d`-style
durations not supported (use `168h` for a week).

## Composing the chain

The typical shape is `[allow?, deny?, delay]`:

```json
"filters": [
  {"kind": "allow",  "patterns": ["@myorg/*"]},
  {"kind": "deny",   "patterns": ["evil-*"]},
  {"kind": "delay",  "min_age": "24h"}
]
```

What that says, in order:

1. Internal `@myorg/*` packages are always allowed — they're
   trusted publishers, bypass everything below.
2. Anything matching `evil-*` is always denied.
3. Everything else must have been published at least 24h ago.

Order matters. Putting `delay` first would force *every* version
(including internal `@myorg/*` releases) through the age check.

## Deny reasons

`Filters.Decide` returns the deciding `Filter` alongside the
decision. Callers identify the policy via `filter.Kinded`:

```go
d, f, err := fs.Decide(ctx, ref)
if d == filter.DecisionDeny {
    kind := "unknown"
    if k, ok := f.(filter.Kinded); ok {
        kind = k.Kind()
    }
    log.Info("blocked by policy", "kind", kind, "package", ref.Package, "version", ref.Version)
}
```

In-tree filters return `"allow"`, `"deny"`, or `"delay"` as their
kind. Operators see these in deny-reason logs and metrics.

## Validation

Filter configuration is validated when the namespace `Spec` is
written via the admin API:

- An empty `allow` or `deny` (no patterns and no rules) is
  rejected — it would never fire, which is almost always a
  configuration error.
- A rule with neither `package` nor `version` is rejected — it
  would match everything.
- Invalid globs (`path.Match` errors) are rejected.
- `delay.min_age` must be positive.

Invalid filters fail the `PUT /admin/v1/namespaces/{name}` call
with HTTP 400 rather than the first request hitting the bad
config.
