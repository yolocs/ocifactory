# npm

ocifactory speaks the [npm registry HTTP protocol](https://github.com/npm/registry/blob/main/docs/REGISTRY-API.md)
that `npm`, `yarn`, and `pnpm` already know. Storage is your OCI
registry — every published `(package, version)` writes a per-version
anchor manifest plus two file manifests (the tarball and
`package.json` metadata blob) attached via `subject`, with dist-tags
landing as alias manifests on the same OCI repo. A parallel `index`
repo's tags enumerate the packages you've published.

> **How it's stored on OCI:** see
> [`docs/architecture/storage-model.md`](../architecture/storage-model.md).
> That doc walks the manifest graph (version anchor / file manifests /
> alias manifests) end-to-end; the worked examples are for python and
> maven, but npm uses the exact same shape.

## Quickstart

Run the server:

```bash
ocifactory serve \
  --repo-type=npm \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://token.actions.githubusercontent.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

Configure `~/.npmrc`:

```ini
registry=https://ocifactory.your-domain/myteam/
//ocifactory.your-domain/myteam/:_authToken=${OCIFACTORY_TOKEN}
```

`OCIFACTORY_TOKEN` is an OIDC ID token; ocifactory has no
static-password path on purpose. See [`docs/auth.md`](../auth.md) for
how the token gets verified.

Then publish and install:

```bash
npm publish
npm install some-package
```

## URL layout

Every route lives under `/{namespace}/` so the same ocifactory
instance can host disjoint npm teams. `{namespace}` is what an
operator chose via the admin namespace API; the per-team `~/.npmrc`
points npm at `https://ocifactory.your-domain/{namespace}/` and every
client request lands there.

| Method | Path (under `/{namespace}/`) | Purpose |
|---|---|---|
| `GET\|HEAD` | `/{name}` | Packument: full package document with all versions and dist-tags. |
| `GET\|HEAD` | `/@{scope}/{name}` | Same, scoped form. |
| `GET\|HEAD` | `/{name}/-/{name}-{version}.tgz` | Tarball download. |
| `GET\|HEAD` | `/@{scope}/{name}/-/{name}-{version}.tgz` | Same, scoped form. |
| `PUT` | `/{name}` | Publish: CouchDB-shaped body with `_attachments`, `versions`, and `dist-tags`. |
| `PUT` | `/@{scope}/{name}` | Same, scoped form. |
| `PUT\|POST` | `/-/package/{name}/dist-tags/{tag}` | `npm dist-tag add`. Body is a JSON-encoded version string. |
| `GET\|HEAD` | `/-/package/{name}/dist-tags` | `npm dist-tag ls`. |
| `DELETE` | `/-/package/{name}/dist-tags/{tag}` | **Returns 501 in v1** — see [Limitations](#limitations). |
| `GET\|HEAD` | `/-/ping` and `/` | `npm ping` and registry-root probe. |

The scoped form (`@scope/name`) is sent verbatim by modern npm
clients — slashes are NOT URL-encoded, and the regex in
`pkg/handler/npm/handler.go` accepts both shapes.

### Package name validation

Names are validated against npm's documented rules: lowercase
alphanumeric plus `.`, `_`, `-`, no leading `.` or `_`, max 214 chars
including the `@scope/` prefix, no tilde (`~`). Anything else
returns `400 Bad Request` at the handler boundary before reaching the
OCI backend.

## Supported client commands

| Command | Status | Reference |
|---|---|---|
| `npm publish` (unscoped) | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_then_install_unscoped` |
| `npm publish` (`@scope/name`) | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_then_install_scoped` |
| `npm install <pkg>` | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_then_install_unscoped` |
| `npm install <pkg>@<version>` | ✅ Supported | Same. |
| `npm install <pkg>@<dist-tag>` | ✅ Supported | `pkg/handler/npm/integration_test.go` — `dist_tag_add_resolves` |
| `npm dist-tag add` | ✅ Supported | Same. |
| `npm dist-tag ls` | ✅ Supported | Tested in `pkg/handler/npm/handler_test.go` (`TestDistTagList`). |
| `npm dist-tag rm` | ❌ Not supported (v1) | Returns 501. |
| `npm unpublish` | ❌ Not supported (v1) | Returns 404. CouchDB `_rev` semantics are gnarly; deferred. |
| `npm login` | ❌ Not supported (v1) | Operators put a token in `.npmrc` directly. |
| `npm search` | ❌ Not supported (v1) | Tracked separately. |
| `npm whoami` | ❌ Not supported (v1) | Returns 404. |
| Abbreviated packument (`Accept: application/vnd.npm.install-v1+json`) | ❌ Not supported (v1) | The full packument we return satisfies modern npm clients; abbreviated form is an optimisation we may add later. |

The integration tests are gated behind `-tags=integration`; CI runs
them in the dedicated client-integration job. Locally:

```bash
go test -tags=integration ./pkg/handler/npm/...
```

## OCI storage layout

Two OCI repositories per namespace under `--backend-registry`:

| OCI repo | Canonical tags | What's stored |
|---|---|---|
| `packages/u/<name>` | `<version>` per release | A version anchor manifest tagged with `<version>`, plus two file manifests (tarball `<name>-<version>.tgz` and `package.json`) subject-linked to the anchor and addressable via the OCI 1.1 referrers API. Used for unscoped packages. |
| `packages/s/<scope>/<name>` | `<version>` per release | Same shape; used for scoped (`@scope/name`) packages. |
| `index` | `<encoded-name>` per package | A single sentinel layer (`name=present`, body=`"1"`). `ListTags("index")` is the package list. |

Each file manifest also carries a deterministic `_f_<sha256>` tag so
tarball downloads resolve in one round-trip.

Dist-tag aliases land as alias manifests on the same `packages/...`
repo, tagged with the dist-tag name: `npm dist-tag add foo@1.0.0 latest`
creates a `latest` alias subject-linked to the `1.0.0` version anchor,
with `ocifactory.alias.target = "1.0.0"` recorded as an annotation —
exactly the same shape python's `latest`/`stable` aliases use.

The base `artifactType` is `application/vnd.ocifactory.npm` and
ocifactory appends `.version`, `.file`, and `.alias` suffixes to
distinguish the three manifest kinds — operators inspecting the
registry see e.g. `application/vnd.ocifactory.npm.alias` on the
`latest` dist-tag.

For the full manifest graph and worked examples, see
[`docs/architecture/storage-model.md`](../architecture/storage-model.md).

### Name encoding

OCI repository segments must match `[a-z0-9]+(?:[._-][a-z0-9]+)*` —
npm names already do, so unscoped names pass through as-is. Scoped
names contain `@` and `/`, neither of which is valid in a segment,
so they're laid out across two segments: `@scope/name` becomes the
repo `packages/s/scope/name`. The `s/` and `u/` sub-prefixes
guarantee an unscoped name can never collide with the scope half of
a scoped name.

The `index` repo's tags follow a different scheme because OCI tags
allow uppercase letters and underscores. Names there are encoded
with the `_HH` percent-style scheme that
`pkg/namespace.encodeTag` uses, so `@scope/foo` becomes
`_40scope_2Ffoo` in the index repo. The encoding is lossless and the
helpers `encodePackageNameTag` / `decodePackageNameTag` in
`pkg/handler/npm/names.go` round-trip exactly.

## Publish flow

`npm publish` POSTs a CouchDB-flavoured JSON document containing:

- `name`: the package name (must match the URL).
- `versions`: a map from version to a per-version metadata object.
- `dist-tags`: optional map from dist-tag name to target version.
- `_attachments`: map from filename to `{ content_type, data, length }`
  where `data` is the base64-encoded tarball.

The handler:

1. Caps the body via `MaxBytesReader` so an oversize publish never
   pays for the JSON decode.
2. Decodes the JSON and validates the URL name against the body.
3. For each version: decodes the attachment, recomputes sha1
   (`dist.shasum`) and sha512 (`dist.integrity`, SRI-prefixed) and
   rejects any mismatch with `400`.
4. Writes the tarball and the version metadata as two layers under
   the same OCI manifest.
5. Writes each dist-tag as an OCI alias on the version's manifest.
6. Writes the per-package index sentinel if it's the first version.
7. Returns `201 {"ok": true, "id": "<name>", "rev": "<server-token>"}`.

## Authentication

Every npm route is gated by `pkg/auth.Middleware` — there are no
public-by-default endpoints in the npm format. The middleware chain
runs **before** any route handler, so an unauthenticated request gets
a `401 Unauthorized` before touching the OCI backend.

Configure the authenticator via `--authn-*` (or `--disable-authn` for
local dev). See [`docs/auth.md`](../auth.md) for the full table.

Authorization (per-package, per-operation policy) is **not**
implemented yet — every authenticated caller in a namespace's
reader / writer matchers can read and write every package in that
namespace.

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--npm-max-upload-bytes` | `1073741824` (1 GiB) | Caps the total request body the publish endpoint accepts. npm publish bodies are base64-encoded JSON, so the wire size is ~1.34× the tarball. Defends against an authenticated client streaming arbitrary bytes. Set to `0` to disable; tighten for cost-sensitive deployments. |
| `--allow-overwrite` | `false` | **Leave this `false` for npm.** npm publish semantics are immutable-add: the same `(package, version)` should never be re-published. The default rejects re-publishes with `409 Conflict`, matching npmjs.org's behaviour. |
| `--disable-streaming-push` | `false` | Force buffered + monolithic uploads through the OCI backend instead of chunked PATCH. Set only if your backend has broken chunked-PATCH support. |
| `--disable-blob-redirect` | `false` | Disable `307` redirects to backend-issued presigned URLs on tarball downloads. Set when exposing backend URLs to clients is unacceptable (egress restrictions, DLP, audit). |

Common server-wide flags (`--port`, `--backend-registry`,
`--enable-metrics`, the `--authn-*` and `--backend-auth-*` families)
are documented in [`docs/auth.md`](../auth.md) and
[`docs/observability.md`](../observability.md).

## Tarball URL rewriting

The `dist.tarball` field inside each version metadata blob is
rewritten on read to point at this server, regardless of what the
publisher originally sent (npm clients default to encoding
`https://registry.npmjs.org/...`). The Host header drives the
rewrite, so deployments behind a reverse proxy that forwards the
client-visible Host need no special configuration.

## Limitations

- **No yank / unpublish.** `DELETE /{name}/-/<file>/-rev/<rev>` and
  `DELETE /{name}/-rev/<rev>` both return `404`. CouchDB `_rev`
  semantics are non-trivial and the operational story (what happens
  to in-flight resolves of yanked versions?) needs more design. File
  an issue if you need this.
- **No `npm login` flow.** Operators put an OIDC token in `.npmrc`
  themselves:

  ```
  //ocifactory.your-domain/myteam/:_authToken=<token>
  ```

  A future device-flow bootstrap may be added.
- **No `npm search`.** The index repo gives us a flat package list,
  but scoring and ranked retrieval are non-trivial. Tracked
  separately.
- **No abbreviated packument metadata** (`Accept:
  application/vnd.npm.install-v1+json`). The full packument we serve
  satisfies modern npm clients; abbreviated form is an optimisation
  for large packuments.
- **No `npm whoami` / token introspection.** Returns `404`.
- **No `npm dist-tag rm`.** Returns `501`. Re-point unwanted dist-tags
  to a known version with `npm dist-tag add <pkg>@<version> <tag>`.
- **No pull-through caching of upstream npm.** Tracked under Phase 4
  of [`docs/ROADMAP.md`](../ROADMAP.md).
