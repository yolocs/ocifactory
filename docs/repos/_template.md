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

Client configuration (`pip.conf`, `~/.m2/settings.xml`, `~/.npmrc`,
`go env GOPROXY`, etc. — show the exact snippet operators paste):

```text
# ... client config ...
```

> **Template note:** if there's a single operator gotcha that breaks
> the second invocation of the most common command (Maven's
> `--allow-overwrite=true` is the precedent), call it out
> **before** the URL layout — front-and-center under a 🚨 heading,
> not buried in "Operator knobs".

## URL layout

| Method | Path | Purpose |
|---|---|---|
| `GET …` | `/{namespace}/…` | … |
| `PUT …` | `/{namespace}/…` | … |

Concrete examples for the most common deploys / fetches (use a real namespace
such as `default`, `platform`, or `payments-prod`):

```
GET /default/…
PUT /default/…
```

Any path validation rules (`pkg/handler/<format>/paths.go` if you
borrow the Maven pattern). Reject anything that could escape the OCI
repo namespace at the handler boundary. Link to [`docs/namespaces.md`](../namespaces.md)
for the namespace policy model rather than duplicating it here.

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
| … | `<format>/<package>` | `<version>` | `<filename>` |
| … | `index` | `<package>` | `<sentinel>` |

`artifactType` = `application/vnd.ocifactory.<format>`.

> **Template note:** if you maintain an `index` repo for the "list all
> packages" use case, document the sentinel layer's name and body
> exactly. The Python handler's pattern is the reference — see
> `docs/repos/python.md`.

## Protocol compliance

| Feature | Status | Notes |
|---|---|---|
| <core protocol feature> | ✅ Supported | … |
| <optional feature> | ❌ Not supported | tracking issue |

## Authentication

State whether every route is gated, or whether some routes are
public-by-default (npm registry root, future Go module proxy
listings). If there are public routes, list them explicitly.

```
- All routes gated by pkg/auth.Middleware: yes / no
- Public-by-default routes (if any): /...
- Outlier endpoints with their own auth contract (if any): /...
```

See [`docs/auth.md`](../auth.md) for authenticator configuration.

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
