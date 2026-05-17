# Python (PyPI)

ocifactory speaks the [PEP 503](https://peps.python.org/pep-0503/) /
[PEP 691](https://peps.python.org/pep-0691/) "simple repository" protocol
clients like `pip` and `twine` already know. Storage is your OCI
registry — every uploaded wheel / sdist becomes its own file manifest
attached via `subject` to a per-version anchor manifest, with a parallel
`index` repo whose tags enumerate the packages you've published.

> **How it's stored on OCI:** see
> [`docs/architecture/storage-model.md`](../architecture/storage-model.md).
> That doc walks `twine upload dist/*` step-by-step through the
> manifests and tags ocifactory writes for one release.

## Quickstart

Run the server:

```bash
ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

Create a namespace via the admin service (see
[`../admin.md`](../admin.md)). Every PyPI URL lives under
`/{namespace}/...` — there is no implicit root namespace.

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/myteam \
  -H 'content-type: application/json' \
  -d '{"policy":{
        "readers":[{"issuer":"https://accounts.google.com"}],
        "writers":[{"issuer":"https://accounts.google.com",
                    "email":"release-bot@myteam.example"}]}}'
```

Configure `pip`:

```ini
# ~/.pip/pip.conf  (or pip.ini on Windows)
[global]
index-url = https://_oidc:${OCIFACTORY_TOKEN}@ocifactory.your-domain/myteam/simple/
```

Configure `twine`:

```bash
twine upload \
  --repository-url https://ocifactory.your-domain/myteam/ \
  --username _oidc \
  --password "$OCIFACTORY_TOKEN" \
  dist/*
```

`_oidc` is one of three sentinel usernames the auth middleware recognises;
see [`docs/auth.md`](../auth.md#how-clients-send-credentials) for the full
list. The matching `password` is an OIDC ID token, not a static password —
ocifactory has no static-password path on purpose.

## URL layout

Every route lives under `/{namespace}/...` — the namespace is the team /
project / environment slot an operator created via the admin API. Requests
to an unknown namespace return `404 Not Found`; requests whose verified
OIDC subject doesn't match the namespace's `readers` or `writers` policy
return `403 Forbidden`.

| Method | Path (under `/{namespace}/`) | Purpose |
|---|---|---|
| `POST /` (or `PUT /`) | (multipart) | `twine upload` — accepts the legacy PyPI multipart form. |
| `GET /simple/` | — | Root simple index: every package this namespace knows about. |
| `GET /simple/{pkg}/` | — | Per-package simple index: every file for `pkg` in this namespace. |
| `GET\|HEAD /packages/{pkg}/{version}/{filename}` | — | Download a wheel/sdist blob. |

Trailing slashes are tolerated on `/simple` and `/simple/{pkg}` — both
forms are routed to the same handler.

`{pkg}` is normalised on every read and write per
[PEP 503 §normalized-names](https://peps.python.org/pep-0503/#normalized-names):
lowercased, runs of `[-_.]` collapsed to a single `-`. Both
`twine upload Foo_Bar` and `pip install foo-bar` resolve to the same
package.

## Supported client commands

| Command | Status | Reference |
|---|---|---|
| `twine upload` | ✅ Supported | `pkg/handler/python/integration_test.go` — `twine_upload_then_pip_download` |
| `pip download --index-url=…` | ✅ Supported | `pkg/handler/python/integration_test.go` — `twine_upload_then_pip_download` |
| `pip install --index-url=…` (in venv) | ✅ Supported | `pkg/handler/python/integration_test.go` — `pip_install_in_fresh_venv` |
| Normalisation round-trip (`Foo_Bar` ↔ `foo-bar`) | ✅ Supported | `pkg/handler/python/integration_test.go` — `pep503_normalization_roundtrip` |
| `pip search` | ❌ Not supported | XML-RPC; out of scope. |
| Yank API | ❌ Not supported | Tracked separately. |

The integration tests are gated behind `-tags=integration`; CI runs them in
the dedicated client-integration job. Locally:

```bash
go test -tags=integration ./pkg/handler/python/...
```

## Retrying a failed `twine upload`

`twine` issues one POST per file in `dist/`. If the run is interrupted
partway through — TCP reset, CI timeout, killed process — the files
that landed before the failure are durably on the backend and the rest
are not. The release directory on the registry is partially populated.

The safe retry is:

```bash
twine upload --skip-existing dist/*
```

`--skip-existing` makes `twine` treat ocifactory's `409 Conflict` (the
default response for an immutable-add collision with
`--allow-overwrite=false`) as "already there, move on" instead of
erroring. Files that didn't land the first time get pushed cleanly;
files that did land are skipped.

If a file was *almost* uploaded but the digest mismatched, it's not on
the backend at all — the blob upload aborts on digest mismatch and no
manifest is written, so the re-upload pushes it cleanly. Per-file
writes are atomic; there's no partial-byte intermediate state to worry
about.

See [`../operations/partial-uploads.md`](../operations/partial-uploads.md)
for the full picture, including why per-version atomicity isn't a
property either ocifactory or PyPI provides.

## OCI storage layout

Two OCI repositories **per namespace** under `--backend-registry`:

| OCI repo | Canonical tags | What's stored |
|---|---|---|
| `<namespace>/packages/<pkg>` | `<version>` per release | A version anchor manifest tagged with `<version>`, plus one file manifest per uploaded wheel / sdist (subject-linked to the version anchor and addressable via the OCI 1.1 referrers API). Each file manifest also carries a deterministic `_f_<sha256>` tag so reads resolve in one round-trip. |
| `<namespace>/index` | `<pkg>` per package | A single sentinel layer (`name=present`, body=`"1"`). The body is unused; `ListTags("<namespace>/index")` is the package list for this namespace. |

A third per-namespace repo, `<namespace>/ocifactory-packages`, is
maintained by the data-plane namespace wrapper itself — its tags
enumerate every OwningRepo the wrapper has recorded a write for, and
it backs the admin service's "namespace is empty" check on soft
delete. Operators don't write to it directly.

`<pkg>` is always the PEP 503 normalised name. The base
`artifactType` is `application/vnd.ocifactory.python` and ocifactory
appends `.version`, `.file`, and `.alias` suffixes to distinguish the
three manifest kinds — operators inspecting the registry see e.g.
`application/vnd.ocifactory.python.file` and can tell at a glance.

For the manifest graph (version anchor / file manifests / alias
manifests), the read path that turns a file lookup into a single tag
resolve, and a worked example tracing `twine upload dist/*` through to
the per-file `AddFile` calls it produces, see
[`docs/architecture/storage-model.md`](../architecture/storage-model.md).

The `index` repo write is one sentinel **per package**, not per version.
The first upload of a given package writes the sentinel; later uploads
short-circuit on `ListTags`. This keeps the immutable-add contract — see
[Operator knobs](#operator-knobs) — compatible with publishing many
versions of the same package.

## PEP compliance

| PEP | Status | Notes |
|---|---|---|
| [PEP 503](https://peps.python.org/pep-0503/) — simple repository (HTML) | ✅ Supported | Names normalised; HTML simple index served by default. |
| [PEP 691](https://peps.python.org/pep-0691/) — JSON simple index | ✅ Supported | Served when the request `Accept`s `application/vnd.pypi.simple.v1+json` with q-value at least as high as any HTML alternative. Default (no `Accept` header, or unparseable) is HTML so legacy clients keep working. |
| [PEP 658](https://peps.python.org/pep-0658/) — `data-dist-info-metadata` | ❌ Not supported | Tracked by [#60](https://github.com/yolocs/ocifactory/issues/60). |
| `data-requires-python` | ❌ Not supported | Tracked by [#60](https://github.com/yolocs/ocifactory/issues/60). |
| [PEP 700](https://peps.python.org/pep-0700/) — `size`, `upload-time`, `versions`, `tracks` | ❌ Not supported | Out of scope; would require additional metadata in the manifest annotations. The PEP 691 response advertises `api-version: "1.0"` to signal we don't emit these fields. |
| `/json/<pkg>/` legacy JSON | ❌ Not supported | Different schema from PEP 691; not implemented. |
| Yank API | ❌ Not supported | Out of scope today. |
| XML-RPC mirror endpoints (`changelog`, `list_packages`) | ❌ Not supported | Deprecated upstream; out of scope. |

The HTML index emits `<a href="…#sha256=…">` per file so `pip` can
verify the download against the OCI backend's digest without an extra
round-trip. The JSON index emits the same digest under
`files[].hashes.sha256`.

## Authentication and authorization

Every PyPI route is gated by `pkg/auth.Middleware` — there are no
public-by-default endpoints in the Python format. The middleware
chain runs **before** any route handler, so an unauthenticated request
gets a `401 Unauthorized` before touching the OCI backend.

Configure the authenticator via `--authn-*` (or `--disable-authn` for
local dev). See [`docs/auth.md`](../auth.md) for the full table.

Authorization is **per-namespace**: every read and write is checked
against the namespace's `Policy` (the `readers` / `writers` matchers
operators wrote when they created the namespace via the admin API).
Authenticated callers whose `(issuer, sub, email, claims)` don't match
any matcher get a `403 Forbidden`. Granularity is coarse — `OpRead`
covers `/simple/...` listings and `/packages/.../<file>` blob fetches;
`OpWrite` covers `POST /` (twine upload). There is no per-package
granularity today: every reader in a namespace can read every package
in it, every writer can write to any package in it. Out-of-tree
authorizers (OPA / Cedar / Casbin) plug in via
`namespace.WithAuthzFactory` if you need finer control.

See [`docs/auth.md`](../auth.md#namespace-authorization) for the
policy model and matcher reference.

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--python-max-upload-bytes` | `1073741824` (1 GiB) | Caps the total request body the upload endpoint accepts. Defends against an authenticated client streaming arbitrary bytes. Set to `0` to disable; tighten for cost-sensitive deployments. |
| `--simple-index-cache-ttl` | `60s` | Per-replica per-package memoisation of `/simple/<pkg>/`. Successful uploads invalidate the affected entry locally; in multi-replica deployments other replicas may take up to one TTL to see a new release. Set to `0` to disable. |
| `--allow-overwrite` | `false` | **Leave this `false` for Python.** twine semantics are immutable-add: the same `(package, version, filename)` should never be re-published. The default rejects re-uploads with `409 Conflict`, matching PyPI's auditability story. |

Common server-wide flags (`--port`, `--backend-registry`, `--enable-metrics`,
the `--authn-*` and `--backend-auth-*` families) are documented in
[`docs/auth.md`](../auth.md) and [`docs/observability.md`](../observability.md).

## Migration: pre-existing data in the OCI backend

Two migrations may apply.

### 1. Pre-existing un-normalized package names

The handler normalises package names at every entry point: writes
canonicalise `Foo_Bar` → `foo-bar` before pushing, and reads
canonicalise the requested name before looking up. **Reads of the
un-normalized name are not aliased back to the canonical name.**

Concretely: if you populated `<namespace>/packages/Foo_Bar:1.0` in your
OCI backend before upgrading to a normalising ocifactory, `pip install
Foo_Bar` will look for `<namespace>/packages/foo-bar:1.0` and 404. Two
ways forward:

1. **Re-upload** the affected versions (`twine upload`). The handler
   normalises on write, so the new manifest lands at
   `<namespace>/packages/foo-bar:1.0`. The old `Foo_Bar` repo is
   harmless but unreachable; clean it up via your OCI backend's
   tooling.
2. **Rename in place** at the OCI layer. Out of ocifactory's scope —
   doable with `oras cp` against your backend, but verify the resulting
   manifest's `artifactType` annotation still reads
   `application/vnd.ocifactory.python.version` /
   `.file` / `.alias`.

For new deployments this never matters: every write canonicalises.

### 2. Data published before the namespace prefix

Earlier ocifactory versions served PyPI URLs at the registry root
(`/simple/`, `/packages/...`). Today every URL lives under a
namespace. If you have `packages/<pkg>` repos in your OCI backend left
over from that era, point ocifactory at them by `oras cp`-ing them
under a namespace prefix (`<namespace>/packages/<pkg>`) and creating
the matching namespace via the admin API. The on-manifest shape (file
manifests + version anchors + alias manifests) is unchanged across
this migration — only the repo path moved.

## Proxy mode (pull-through PyPI)

Set the namespace's `mode` to `"proxy"` and supply a `proxy.upstream`
URL when creating it via the admin API:

```bash
curl -X PUT \
  -H 'Content-Type: application/json' \
  --data '{
    "mode": "proxy",
    "proxy": {
      "upstream": "https://pypi.org",
      "filters": [
        {"kind": "denylist", "rules": [{"package": "evil-*"}]}
      ]
    },
    "policy": {
      "readers": [{"issuer": "..."}],
      "writers": [{"issuer": "..."}]
    }
  }' \
  http://admin.ocifactory.internal/admin/v1/namespaces/pypi-cache
```

Point `pip` at the namespace exactly like a hosted one:

```ini
# ~/.pip/pip.conf
[global]
index-url = https://ocifactory.example.com/pypi-cache/simple/
```

What changes on the wire:

- **`/simple/<pkg>/`** (per-package index): served from an in-process
  L1 cache (10 s TTL by default) → an OCI-backed pull-through cache
  under `<ns>/ocifactory-proxy-cache/index/<pkg>` (60 s TTL) → upstream
  PyPI. Absolute `files.pythonhosted.org` URLs in the response are
  rewritten to `/<ns>/packages/<pkg>/<version>/<filename>` so file
  downloads route back through ocifactory.
- **`/simple/`** (top-level): same caching shape. No URL rewriting
  needed (anchors are package-relative).
- **`/packages/<pkg>/<version>/<filename>`**: registry hit → done.
  Miss → cheap filter chain (name allowlist/denylist) → PyPI JSON
  metadata fetch → metadata-dependent filters (publish-time delay)
  → upstream file fetch → tee through `AddFile` into OCI while
  streaming to the client. The second request for the same file
  serves entirely from the OCI cache.
- **Uploads (`POST /`)** return `405 Method Not Allowed`. Twine
  cannot publish into a proxy namespace.

Degraded operation:

- Upstream returns 5xx on an index request and the OCI cache has a
  stale entry → serve stale, log a warning.
- Upstream returns 5xx and no cache entry exists → synthesize a
  minimal index from local `ListFiles("packages/"+pkg)`, so previously
  cached files remain installable.
- Upstream and local synthesis both empty → 503 Service Unavailable.
- Upstream returns 404 on a file → 404 to the client. A short in-memory
  negative cache (60 s default) suppresses repeat probes for the same
  bad URL.

Concurrent first-misses for the same `(pkg, version, filename)` collapse
onto one upstream fetch via singleflight, so a thundering herd of
`pip install requests` at deploy time pays one upstream RTT per file
per process — not N.

The filter chain (issue [#110](https://github.com/yolocs/ocifactory/issues/110))
runs allowlist / denylist filters before any upstream call and the
publish-time `delay` filter after PyPI metadata fetch. A denied file
returns 404; a denied index entry is filtered out of the response. See
[`docs/proxy/filter-policy.md`](../proxy/filter-policy.md).

## Limitations

- **No PEP 658 `data-dist-info-metadata`.** `pip` falls back to
  downloading the full wheel for dependency resolution; on slow links or
  large wheels this is noticeably slower than against PyPI proper.
  Tracked by [#60](https://github.com/yolocs/ocifactory/issues/60).
- **No `data-requires-python`.** Resolvers can't pre-filter wheels by
  Python version from the index. Same tracking issue as above.
- **No `/json/<pkg>/` endpoint.** A handful of older tools used the
  pre-PEP-691 JSON shape; modern clients negotiate via `Accept` and
  get the PEP 691 response.
- **No yank API.** A bad release has to be pulled by deleting the
  `packages/<pkg>:<version>` tag in your OCI backend.
- **No PEP 658 `data-dist-info-metadata` synthesis** on hosted namespaces. Proxy
  namespaces preserve the upstream attribute when PyPI sets it.
- **No XML-RPC mirror endpoints.** Deprecated upstream; not implemented.
