# Admin service

`ocifactory admin serve` runs the control-plane HTTP service. It is a separate
listener from the data plane (`ocifactory serve`) and should be deployed on a
separate hostname behind platform or network access controls.

> **Security requirement:** the admin service intentionally has no internal
> authentication or authorization middleware. Put it behind Cloud Run
> authenticated invoker, an internal load balancer ACL, an identity-aware proxy,
> or equivalent controls before exposing it to operators.

The admin service owns the **namespace catalogue** — the documents that say
"a namespace named `myteam` exists, and these are the OIDC subjects allowed
to read and write its packages". Every data-plane URL ocifactory serves lives
under a namespace prefix (`/{namespace}/...` for python and npm,
`/{namespace}/maven2/...` for maven), and the data plane fetches each
namespace's spec from the same OCI backend the admin service writes to.

## Start the service

```bash
ocifactory admin serve \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8081 \
  --namespace-prefix=control-plane
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--backend-registry` | Required. OCI registry URL that stores namespace metadata and the namespace index. |
| `--port` | Listener port. Defaults to `8081`; `PORT` is also honored for PaaS deployments. |
| `--namespace-prefix` | Optional OCI repository prefix for namespace metadata and the global namespace index. Defaults to empty. Use a lowercase OCI-safe path segment such as `control-plane`. |
| `--backend-auth-kind` and related `--backend-auth-*` flags | Same backend credential providers as the data-plane service. |
| `--enable-metrics`, `--metrics-path` | Expose and configure Prometheus metrics. |

The service logs a WARN during startup to remind operators that it trusts the
network/platform in front of it.

## HTTP API

All endpoints are JSON-only and versioned under `/admin/v1`.

| Method | Path | Body | Success response |
|---|---|---|---|
| `PUT` | `/admin/v1/namespaces/{name}` | `namespace.Spec` JSON | `201` with a `namespace.Namespace` on create; `200` on update. |
| `GET` | `/admin/v1/namespaces/{name}` | none | `200` with a `namespace.Namespace`. |
| `DELETE` | `/admin/v1/namespaces/{name}` | none | `204` when the namespace exists and is empty. |
| `GET` | `/admin/v1/namespaces` | none | `200 {"namespaces":[...]}`. |

Errors use `{"error":"message"}`. Invalid namespace names and invalid specs
return `400`; missing namespaces return `404`; soft-delete of a non-empty
namespace returns `409`.

### Namespace name validation

Names must satisfy:

- 1–64 characters, lowercase ASCII alphanumerics and `-`.
- No leading or trailing `-`; no leading `_` or `.` (both reserved for
  internal use).
- Not one of a small reserved set: `admin`, `healthz`, `readyz`,
  `metrics`, `simple`, `maven2`, `v2`, `npm`,
  `ocifactory-namespaces`, plus a few `_`-prefixed historical names.
  Reservations are conservative on purpose — better to over-reserve in
  v1 than collide with future routing.

### `Spec` shape

```json
{
  "schema_version": 1,
  "mode": "hosted",
  "policy": {
    "readers": [
      {"issuer": "https://accounts.google.com"}
    ],
    "writers": [
      {"issuer": "https://accounts.google.com",
       "email": "release-bot@myteam.example"}
    ]
  },
  "proxy": { /* only honored when mode == "proxy"; see below */ },
  "format": { /* reserved for future format-specific knobs */ }
}
```

- `mode` selects the namespace's operating mode. `"hosted"` (the
  default — empty resolves to hosted) stores artifacts uploaded by
  clients and serves them back; `"proxy"` mirrors an upstream
  registry on demand. Existing namespaces written before this field
  was introduced load unchanged. On write, an explicit `"hosted"`
  collapses to the empty default to keep the on-disk shape compact.
- `policy.readers` / `policy.writers` are independent
  `SubjectMatcher` lists. **An entirely empty policy is deny-all** —
  a caller is allowed to perform an op only if at least one matcher
  in the corresponding list matches. Granting `write` does not imply
  `read`.
- Every matcher must populate at least one of `issuer`, `sub_match`
  (RE2 regex), `email`, `claims_match` (map of claim → regex), or
  `kind`. Regex patterns are anchored at both ends (`^...$`); if
  your pattern already starts with `^` or ends with `$`, the anchor
  isn't duplicated. See [`auth.md`](auth.md#subjectmatcher-reference)
  for the full reference and worked examples.
- `proxy.upstream` is the canonical URL of the upstream registry the
  namespace mirrors (e.g. `https://pypi.org`,
  `https://registry.npmjs.org`, `https://repo1.maven.org/maven2`).
  Required when `mode` is `"proxy"`; must parse as an absolute
  `http`/`https` URL. A `proxy` block on a hosted namespace is
  rejected at validation.
- `proxy.filters` is the ordered filter chain applied before any
  upstream call (allowlist / denylist / publish-time delay). The
  concrete shape lands in a follow-up issue; today ocifactory accepts
  and roundtrips arbitrary filter JSON so an operator's spec written
  for a newer ocifactory survives older binaries.
- `format` is reserved for future format-specific knobs; ocifactory
  preserves it across roundtrips so a newer ocifactory's keys aren't
  silently dropped by an older one.

### Worked example — create a namespace

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/myteam \
  -H 'content-type: application/json' \
  -d '{"policy":{
        "readers":[{"issuer":"https://token.actions.githubusercontent.com",
                    "sub_match":"repo:myorg/.+"}],
        "writers":[{"issuer":"https://token.actions.githubusercontent.com",
                    "sub_match":"repo:myorg/myapp:ref:refs/heads/main"}]}}'
```

Response: `201 Created` with the persisted document (including the
`schema_version: 1` ocifactory stamped on write).

### Worked example — create a proxy namespace

A proxy namespace mirrors an upstream registry on demand. The data
plane will not auto-discover this — every URL still lives under
`/{namespace}/...`, but reads to it fetch from `proxy.upstream`
through the per-format proxy code path (which lands in follow-up
issues).

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/pypi \
  -H 'content-type: application/json' \
  -d '{
        "mode": "proxy",
        "proxy": {"upstream": "https://pypi.org"},
        "policy": {
          "readers":[{"issuer":"https://token.actions.githubusercontent.com",
                      "sub_match":"repo:myorg/.+"}]
        }
      }'
```

Response: `201 Created`. Validation rejects `mode: "proxy"` without a
parseable `proxy.upstream`, and rejects a `proxy` block on a hosted
namespace (empty `mode` or explicit `"hosted"`).

### Policy propagation

Policy changes via this API take effect on the very next data-plane
request. The admin `Store` fires a mutation hook on `Put` / `Delete`
that the data plane's `namespace.Registry` consumes to invalidate its
cached authorizer for the affected namespace. Without that hook,
changes would only land after the cache TTL (`DefaultPolicyCacheTTL = 60s`)
expired.

## Soft delete

`DELETE` is intentionally conservative in this phase. ocifactory checks the
namespace package index and refuses to delete a namespace that still contains
packages. Remove package data through a future cascade-delete flow before
deleting those namespaces.

## Health, readiness, and metrics

The admin service reuses the same observability endpoints as the data plane:

- `/healthz` returns liveness.
- `/readyz` calls `Store.List` against the OCI-backed namespace index under the
  standard readiness timeout.
- `/metrics` exposes Prometheus metrics when metrics are enabled.

## Namespace metadata storage

Namespace metadata is stored with OCI artifact type
`application/vnd.ocifactory.namespace`, independent of the data-plane format
served by a process. If you experimented with namespace metadata from the short
window before this admin service existed, recreate those namespaces through the
admin API so Python, Maven, npm, and future repo types all read the same
control-plane documents.

Concretely, for a namespace named `myteam` with `--namespace-prefix=""`
(the default):

| OCI repo | Tag | Body |
|---|---|---|
| `myteam` | `_metadata` | The JSON-serialised `Spec` written by the admin `PUT`. |
| `ocifactory-namespaces` | `myteam` | A single sentinel layer — the tag's existence is the catalogue entry. |

A non-empty `--namespace-prefix` (e.g. `control-plane`) shifts both
repos under that prefix (`control-plane/myteam`, `control-plane/ocifactory-namespaces`).
Operators sharing one OCI registry between an ocifactory deployment and
unrelated artifacts use the prefix to keep the namespaces out of the way.

## `schema_version`

Persisted specs carry an integer `schema_version` field. ocifactory stamps it
on every write — operators do not set it manually and clients can omit it from
PUT bodies. The contract is:

- The current binary writes `schema_version: 1`.
- Bodies without the field (written by older builds, before this field
  existed) are treated as version 1 on read; no backfill is required.
- A body claiming a `schema_version` higher than this binary understands is
  rejected with `HTTP 400` rather than silently loaded with fields dropped.
  Upgrade ocifactory before working with those namespaces.

The field exists so future, backwards-incompatible changes to the on-disk
shape have an in-band way to migrate without operator-coordinated downtime.
