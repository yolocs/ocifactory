# Authentication

ocifactory authenticates inbound requests through a chain of
**OIDC** authenticators — one per trusted issuer. The only
credential ocifactory accepts is a verifiable token issued by an
OIDC provider; static passwords are not supported.

The auth chain is configured via a YAML file passed to
`--auth-config`. No other process state is involved; reload by
restarting.

## Where auth runs

`pkg/auth` is a library; the middleware it exposes
(`auth.Middleware(authn)`) is **not** installed at the server
level. Each format handler chains the middleware on its own
router, so:

- Observability endpoints (`/healthz`, `/readyz`, `/metrics`) are
  reachable without auth — they're served before any format
  handler runs.
- Today both built-in formats (python, maven) gate every route
  via `router.Use(authMW)` on their root router.
- Future formats with a public/private route split (npm registry
  reads, Go module proxy listings) can chain the middleware on a
  sub-router and leave public routes ungated.
- Outlier endpoints that need their own auth contract (e.g. an
  npm login bootstrap that mints a token) sit on a sub-router
  that doesn't `Use(authMW)`.

> **Note**: ocifactory authenticates **clients** (callers of the
> ocifactory HTTP API). It does **not** speak any authentication
> for the backend OCI registry — that's a separate concern handled
> by the backend credential provider (see roadmap).

## Configuration

```yaml
# /etc/ocifactory/auth.yaml
authenticators:
  - kind: oidc
    issuer: https://accounts.google.com
    audience: https://ocifactory.your-domain
  - kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: https://ocifactory.your-domain
```

Run with:

```bash
ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --auth-config=/etc/ocifactory/auth.yaml \
  --port=8080
```

Authenticators are tried in declaration order. Each authenticator
peeks the unverified `iss` claim of the incoming token; only the
authenticator whose configured issuer matches will run full
verification. Mismatched issuers fall through silently to the next
authenticator. This is what lets a multi-issuer chain (Google +
GitHub Actions + ...) accept tokens from any of its members
without short-circuiting on the first one.

### Required vs optional

Either `--auth-config` or `--disable-auth` must be set. The server
refuses to start without one or the other — **no implicit
"allow everything" mode**. `--disable-auth` exists for local
development against zot-with-no-auth and prints a loud warning at
startup.

## How clients send credentials

Two presentations are supported:

1. **`Authorization: Bearer <token>`** — preferred. Modern `pip`
   (twine), `npm`, and direct `curl` flows.

2. **`Authorization: Basic base64("<sentinel>:<token>")`** —
   sentinel-username fallback for tools that only do Basic auth.
   Three sentinel usernames are recognised:
   - `_oidc` (canonical)
   - `oauth2accesstoken` (matches Google Artifact Registry tooling)
   - `_token` (matches npm registry conventions)

   When a request uses any of these sentinels, the `password`
   field is treated as an OIDC bearer token, not a password.

A regular Basic header with a non-sentinel username (e.g.
`alice:hunter2`) is rejected — there is no static-password path.

## Worked example: Google ID tokens

| | Value |
|---|---|
| Issuer | `https://accounts.google.com` |
| JWKS (auto-discovered) | `https://www.googleapis.com/oauth2/v3/certs` |
| Audience | Operator-configured, typically `https://ocifactory.your-domain` |
| `sub` | Stable numeric account or service-account ID |
| Useful claims | `email`, `email_verified` |

> ⚠️ Only Google **ID tokens** are JWTs. Google **access tokens**
> are opaque and cannot be verified offline. Callers must
> explicitly mint an ID token with the correct audience.

```bash
# Cloud Run / GCE / GKE (metadata server)
TOKEN=$(curl -s -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://ocifactory.your-domain")

# Local with gcloud (ID token, NOT access token)
TOKEN=$(gcloud auth print-identity-token --audiences=https://ocifactory.your-domain)

# Use it
twine upload --username _oidc --password "$TOKEN" dist/*
# or
curl -H "Authorization: Bearer $TOKEN" https://ocifactory.your-domain/...
```

## Worked example: GitHub Actions OIDC

| | Value |
|---|---|
| Issuer | `https://token.actions.githubusercontent.com` |
| JWKS (auto-discovered) | `https://token.actions.githubusercontent.com/.well-known/jwks` |
| Audience | Workflow-requested per run; ocifactory pins what it accepts |
| `sub` | Structured: `repo:owner/repo:ref:refs/heads/main`, `repo:owner/repo:environment:prod`, ... |
| Useful claims | `repository`, `actor`, `workflow`, `ref`, `environment`, `runner_environment` |

```yaml
# .github/workflows/publish.yml
permissions:
  id-token: write
  contents: read
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
      - run: pip install twine build && python -m build
      - run: |
          TOKEN=$(curl -s -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
                       "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ocifactory.your-domain" \
                  | jq -r .value)
          twine upload --username _oidc --password "$TOKEN" \
            --repository-url https://ocifactory.your-domain/ dist/*
```

The structured `sub` is the strong point — once authorization
lands (separate issue), policy can match on it precisely (e.g.
"`repo:owner/repo:environment:prod` may publish to `myproject`").

## Why no static passwords

Static passwords are a recurring source of operational pain
(rotation, sharing, leakage in logs and CI configs) and add a
class of attack surface (offline cracking of the hash file,
username enumeration via timing, etc.) that an OIDC-only design
sidesteps.

In 2026 every realistic ocifactory deployment has access to an
OIDC issuer:
- Cloud-issued: Google service accounts, GitHub Actions, AWS STS
  via Workload Identity Federation, Azure AD.
- Self-hosted: dex, keycloak, ory hydra, or even a tiny in-house
  issuer that signs short-lived JWTs from a static key.

Air-gapped or fully-offline deployments where no IDP exists at
all can still run with `--disable-auth` behind a network ACL.

## Adding your own authenticator (out-of-tree)

The `Authenticator` interface and its YAML kind registry are
public. To add an authenticator (static password, GitHub PAT,
mTLS, anything else) without forking ocifactory:

```go
// example.com/ocifactory-myauth/myauth.go
package myauth

import (
    "github.com/yolocs/ocifactory/pkg/auth"
)

func init() {
    auth.RegisterKind("myauth", func(spec auth.AuthenticatorSpec) (auth.Authenticator, error) {
        var cfg struct {
            // ... your fields
        }
        if err := spec.Decode(&cfg); err != nil {
            return nil, err
        }
        return newAuthenticator(cfg)
    })
}
```

Then build a custom ocifactory binary that imports your package
for side effects:

```go
// cmd/myocifactory/main.go
import (
    _ "example.com/ocifactory-myauth"
    // ...
)
```

Operators get a new `kind: myauth` they can put in their
auth-config YAML.

## What's behind the load balancer

Audience claims are tied to the public URL the token-issuer was
told to mint a token for. Behind a reverse proxy this means: the
audience the **caller** specified must match the audience
**ocifactory** is configured to accept — not whatever
`X-Forwarded-Host` happens to say. Pick one canonical public URL
(`https://ocifactory.your-domain`), put it in the auth config, and
have callers use it when they request tokens.

## CI: end-to-end OIDC test

`.github/workflows/ci.yml` includes an `oidc-e2e` job that mints a
real GitHub Actions OIDC token (audience `ocifactory-ci`), starts
ocifactory with `--repo-type=echo` in front of an OIDC
authenticator pointed at `https://token.actions.githubusercontent.com`,
and asserts:

| Case | Expected |
|---|---|
| Valid token, correct audience | 200; `/whoami` echoes the verified `iss` and `sub` |
| Missing `Authorization` | 401 |
| Garbage bearer token | 401 |
| Valid token, wrong audience | 401 |

The `echo` repo type is a no-op format that exists for this job — no
OCI backend, no real artifacts. See `pkg/handler/echo`.

## Failure modes

- **No credential** → `401 Unauthorized` with
  `WWW-Authenticate: Bearer realm="ocifactory"` and
  `WWW-Authenticate: Basic realm="ocifactory"`.
- **Wrong issuer / unparseable JWT** → falls through to the next
  authenticator; if none accepts the request, `401 Unauthorized`.
- **Wrong audience / expired / not-yet-valid / bad signature** →
  `401 Unauthorized`. Body is intentionally generic; details are
  in the server log at debug level.
- **Issuer unreachable** (JWKS endpoint down, discovery doc
  unfetchable) → `503 Service Unavailable`. The operator can
  distinguish "your credential is bad" from "I can't even check
  your credential right now". JWKS state is cached after the first
  successful fetch; transient network blips don't kick everything
  to 503, and the next request after recovery succeeds (lazy
  re-discovery on the failure path).

## What's out of scope (for now)

- **Authorization** — what a verified caller is allowed to do.
  Tracked separately; today every authenticated caller can
  read/write everything.
- **Backend credentials** — how ocifactory talks to GAR / ECR /
  zot. Tracked separately; today the backend must accept whatever
  identity the runtime provides (typically Cloud Run's service
  account against GAR).
- **GitHub PATs / App tokens** — opaque, not OIDC. A future
  authenticator could validate them via `api.github.com/user`,
  but it has different perf characteristics; meanwhile the
  out-of-tree pattern above is the supported path.
- **Token revocation lists / introspection endpoints** — punt.
- **Web UI / login flows** — API-only product.
