# npm

The npm handler speaks the npm registry API used by `npm publish`,
`npm install`, and `npm dist-tag`. Package versions, materialized
packuments, and the package index are stored as OCI manifests and tags
under the selected namespace; no side database is used.

## Quickstart

Start ocifactory with the npm format:

```bash
ocifactory serve \
  --repo-type=npm \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

Configure npm to use a namespace-prefixed registry URL. Every npm route
is private in v1, so put a bearer token in `.npmrc` using whatever
out-of-band OIDC flow your deployment uses:

```bash
npm config set registry https://ocifactory.example.com/myteam/
npm config set //ocifactory.example.com/myteam/:_authToken "$OIDC_TOKEN"

npm publish
npm install left-pad
npm install @scope/pkg
npm dist-tag add left-pad@1.0.0 next
npm install left-pad@next
```

For local development with `--disable-authn`, npm can use any non-empty
token value because the server authenticates all requests as the
`anonymous` development subject.

## URL layout

All paths live under `/{namespace}/`.

| Method | Path | Purpose |
|---|---|---|
| `GET`, `HEAD` | `/{namespace}/{name}` | Full packument for an unscoped package. |
| `GET`, `HEAD` | `/{namespace}/@{scope}/{name}` | Full packument for a scoped package. |
| `GET`, `HEAD` | `/{namespace}/{name}/-/{name}-{version}.tgz` | Tarball download. |
| `GET`, `HEAD` | `/{namespace}/@{scope}/{name}/-/{name}-{version}.tgz` | Scoped tarball download; the filename uses the package basename. |
| `PUT` | `/{namespace}/{name}` | Publish a package version from npm's CouchDB-style JSON body. |
| `PUT` | `/{namespace}/@{scope}/{name}` | Publish a scoped package version. |
| `PUT`, `POST` | `/{namespace}/-/package/{name}/dist-tags/{tag}` | Add or move a dist-tag. Body is a JSON string containing the version. |
| `GET`, `HEAD` | `/{namespace}/-/package/{name}/dist-tags` | List dist-tags. |
| `DELETE` | `/{namespace}/-/package/{name}/dist-tags/{tag}` | Remove a dist-tag. |
| `GET` | `/{namespace}/-/ping`, `/{namespace}/` | npm ping / registry root health response. |

Examples:

```text
PUT /myteam/left-pad
GET /myteam/left-pad
GET /myteam/left-pad/-/left-pad-1.0.0.tgz
GET /myteam/@scope/foo
GET /myteam/@scope/foo/-/foo-1.0.0.tgz
PUT /myteam/-/package/left-pad/dist-tags/next   body: "1.0.0"
```

Package names must be canonical npm names: unscoped `name` or scoped
`@scope/name`, lower-case, non-empty, with no `.` / `..` path segments,
empty segments, leading slash, or trailing slash. Characters that are
valid npm names but unsafe in OCI repo path segments are encoded before
storage.

## Supported client commands

| Command | Status | Reference |
|---|---|---|
| `npm publish` | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_install_dist_tag`, `scoped_publish_install` |
| `npm install` | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_install_dist_tag`, `scoped_publish_install` |
| `npm dist-tag add` | ✅ Supported | `pkg/handler/npm/integration_test.go` — `publish_install_dist_tag` |
| `npm dist-tag ls` / `npm dist-tag rm` | ✅ Supported | `pkg/handler/npm/handler_test.go` — `TestDistTags` |
| `npm unpublish` | ❌ Not supported | CouchDB `_rev` / yank semantics are deferred. |
| `npm login` | ❌ Not supported | Operators provide OIDC bearer tokens in `.npmrc` out of band. |
| `npm search` | ❌ Not supported | Search scoring and query APIs are deferred. |
| Abbreviated metadata | ❌ Not supported | ocifactory returns full packuments; modern npm clients accept them. |

## OCI storage layout

The namespace registry wrapper prepends `<namespace>/` to each OCI repo.
The handler itself uses namespace-relative names:

| npm concept | OCI repo | OCI tag | Layer name |
|---|---|---|---|
| Package version tarball | `packages/<encoded-name>` | `<version>` | `<basename>-<version>.tgz` |
| Dist-tag | `packages/<encoded-name>` | Alias tag `<tag>` | Alias manifest pointing at the canonical version. |
| Materialized packument | `packuments/<encoded-name>` | `current` | `packument.json` |
| Package list index | `index` | `<encoded-name>` | `present` |

`artifactType` = `application/vnd.ocifactory.npm`.

The package-list `index` repo follows the same sentinel pattern as the
Python handler: the tag is the encoded package name, the layer name is
`present`, and the body is `1`. The namespace wrapper also records its
format-independent package index for admin cascade-delete bookkeeping.

### Scoped-package encoding

OCI repository path segments cannot safely contain npm's `@` and `/`.
ocifactory stores package names with a lossless `_HH` byte escape:

| npm name | Encoded name |
|---|---|
| `left-pad` | `left-pad` |
| `foo_bar` | `foo_5Fbar` |
| `@scope/foo` | `_40scope_2Ffoo` |

The encoding is internal, but it is visible if an operator browses the
backend OCI registry directly.

## Protocol compliance

| Feature | Status | Notes |
|---|---|---|
| Full packument reads | ✅ Supported | Returned for normal and install-v1 accept headers. |
| Tarball reads | ✅ Supported | GET streams or redirects; HEAD streams headers only and never redirects. |
| Publish | ✅ Supported | Base64 attachment is decoded; sha1 and sha512 integrity are recomputed and verified when provided. |
| Dist-tag add / list / remove | ✅ Supported | Dist-tags are OCI alias tags and are reflected in the materialized packument. |
| Existing-version immutability | ✅ Supported | `--allow-overwrite=false` rejects re-publish with `409 Conflict`. |
| Yank / unpublish | ❌ Not supported | DELETE package revision routes return `501 Not Implemented`. |
| Login / whoami / token introspection | ❌ Not supported | Use `.npmrc` bearer tokens. |
| Search | ❌ Not supported | No `/-/v1/search` endpoint in v1. |

## Authentication

Every npm route is gated by the configured `pkg/auth.Middleware`,
including reads, writes, registry root, and ping. Public read mirrors
are intentionally out of scope for v1; put an unauthenticated proxy in
front if you need public reads.

```text
- All routes gated by pkg/auth.Middleware: yes
- Public-by-default routes: none
- Outlier endpoints with their own auth contract: none
```

See [`docs/auth.md`](../auth.md) for authenticator configuration.

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--npm-max-upload-bytes` | `1073741824` | Caps the total JSON publish request body. Set to `0` to disable the cap. |
| `--allow-overwrite` | `false` | Leave false for immutable npm releases. Set true only for workflows that intentionally re-publish the same version. |
| `--disable-blob-redirect` | `false` | Disable backend presigned-URL redirects for tarball GETs. HEAD never redirects. |

Common server-wide flags (`--port`, `--backend-registry`, metrics,
frontend auth, and backend auth) are documented in [`docs/auth.md`](../auth.md)
and [`docs/observability.md`](../observability.md).

## Migration: pre-existing data in the OCI backend

There is no migration path from the old npm stub because it never wrote
npm artifacts. Data written by other registries is not auto-discovered;
ocifactory expects the storage layout above, including the materialized
packument and index repos.

## Limitations

- No `npm unpublish` / yank support. The current DELETE routes return
  `501 Not Implemented` until CouchDB `_rev` behavior is designed.
- No `npm login` shim. Operators must issue OIDC tokens out of band and
  place them in `.npmrc` as `_authToken` values.
- No `npm search` endpoint.
- No abbreviated packument optimization; full packuments are returned.
- No pull-through caching from the public npm registry until the Phase 4
  caching work lands.
