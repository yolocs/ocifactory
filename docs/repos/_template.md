# <Format>

> **Template note:** copy this file to `docs/repos/<format>.md` when
> adding a new repo type, then strip every `> Template note:` block
> before publishing. The acceptance criterion is that an operator who
> has never seen the codebase can configure their client against a
> running ocifactory using only this doc — match the depth of
> `docs/repos/python.md` and `docs/repos/maven.md`.

One-paragraph pitch: which client protocol does this format speak,
which clients are known to work, and what's the OCI storage shape in
one sentence.

## Quickstart

Server invocation (full `ocifactory serve` line including `--repo-type`,
`--backend-registry`, `--allow-overwrite` if required, and the auth
flags):

```bash
ocifactory serve \
  --repo-type=<format> \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

Namespace creation via the admin service (every URL lives under a
namespace — see [`../admin.md`](../admin.md)):

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/myteam \
  -H 'content-type: application/json' \
  -d '{"policy":{
        "readers":[{"issuer":"https://accounts.google.com"}],
        "writers":[{"issuer":"https://accounts.google.com",
                    "email":"release-bot@myteam.example"}]}}'
```

Client configuration (`pip.conf`, `~/.m2/settings.xml`, `~/.npmrc`,
`go env GOPROXY`, etc. — show the exact snippet operators paste, with
the namespace prefix in the URL):

```text
# ... client config pointing at https://ocifactory.your-domain/myteam/... ...
```

> **Template note:** if there's a single operator gotcha that breaks
> the second invocation of the most common command (Maven's
> `--allow-overwrite=true` is the precedent), call it out
> **before** the URL layout — front-and-center under a 🚨 heading,
> not buried in "Operator knobs".

## URL layout

Every route lives under `/{namespace}/...` (and optionally a fixed
format prefix after the namespace — e.g. `/{namespace}/maven2/...` —
when needed for disambiguation). Document the per-route shape under
the namespace prefix; requests to an unknown namespace return `404 Not
Found`, requests whose policy denies return `403 Forbidden`.

| Method | Path (under `/{namespace}/`) | Purpose |
|---|---|---|
| `GET …` | `/…` | … |
| `PUT …` | `/…` | … |

Concrete examples for the most common deploys / fetches:

```
GET /myteam/…
PUT /myteam/…
```

Any path validation rules (`pkg/handler/<format>/paths.go` if you
borrow the Maven pattern). Reject anything that could escape the OCI
repo namespace at the handler boundary; the data-plane wrapper's
`resolveRepo` is the second line of defence but per-handler
validation produces better error messages.

## Supported client commands

| Command | Status | Reference |
|---|---|---|
| `<client> publish` | ✅ Supported | `pkg/handler/<format>/integration_test.go` — `<subtest_name>` |
| `<client> install` | ✅ Supported | `pkg/handler/<format>/integration_test.go` — `<subtest_name>` |
| `<client> search` | ❌ Not supported | reason / tracking issue |

> **Template note:** the integration test file proves the supported
> claim. If a row says ✅ Supported, link to a real subtest.

## OCI storage layout

| Client URL | OCI repo | OCI tag | Layer name |
|---|---|---|---|
| … | `<namespace>/<format>/<package>` | `<version>` | `<filename>` |
| … | `<namespace>/index` | `<package>` | `<sentinel>` |

`artifactType` (base) = `application/vnd.ocifactory.<format>`. The
data-plane wrapper adds the `<namespace>/` prefix to every OwningRepo
transparently; per-format handlers address repos without the
namespace and the wrapper joins them at request time. A separate
`<namespace>/ocifactory-packages` repo is maintained by the wrapper
itself — its tags enumerate every owning-repo the wrapper has written
to, and the admin service's soft-delete consults it. Format handlers
don't write there directly.

> **Template note:** if you maintain a per-format `index` repo for
> the "list all packages" use case, document the sentinel layer's
> name and body exactly. The Python handler's pattern is the
> reference — see `docs/repos/python.md`.

## Protocol compliance

| Feature | Status | Notes |
|---|---|---|
| <core protocol feature> | ✅ Supported | … |
| <optional feature> | ❌ Not supported | tracking issue |

## Authentication and authorization

State whether every route is gated, or whether some routes are
public-by-default (Go module proxy listings, anonymous-read
mirrors). If there are public routes, list them explicitly.

```
- All routes gated by pkg/auth.Middleware: yes / no
- Public-by-default routes (if any): /...
- Outlier endpoints with their own auth contract (if any): /...
```

Describe per-op mapping (which client commands invoke `OpRead`
versus `OpWrite`). The namespace's `Policy` (readers / writers
`SubjectMatcher` lists) controls both — see
[`docs/auth.md#namespace-authorization`](../auth.md#namespace-authorization).
Note any granularity beyond per-namespace (almost certainly: none
in v1; per-package authz is operator-pluggable via
`artifact.WithAuthzFactory`).

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--<format>-…` | `…` | … |
| `--allow-overwrite` | `false` | leave default unless this format requires it (Maven does, Python doesn't) |

Common server-wide flags (`--port`, `--backend-registry`,
`--enable-metrics`, the `--authn-*` and `--backend-auth-*` families)
are documented in [`docs/auth.md`](../auth.md) and
[`docs/observability.md`](../observability.md). Don't re-document
them here.

## Migration: pre-existing data in the OCI backend

> **Template note:** required for any format that does name
> normalisation, content-addressing, or anything else that changes
> how the same logical artifact maps to OCI storage between releases.
> If your format passes the client's coordinates through verbatim,
> say so explicitly so operators with pre-existing data know nothing
> needs to change.

## Limitations

- Concrete missing features that operators will hit on day one.
- Tracking issues for each.
- Phase 4 pull-through caching note if relevant.

> **Template note:** be specific. "No yank API" is useful;
> "incomplete" is not.
