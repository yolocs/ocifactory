# Authentication and authorization

ocifactory has three independent identities and policy surfaces:

- **Frontend authentication** — how callers of the ocifactory HTTP
  API authenticate _to ocifactory_. OIDC only; configured via
  `OCIFACTORY_AUTHN_*`.
- **Per-namespace authorization** — what an authenticated caller is
  allowed to do on a specific namespace. Defined inside each
  namespace's `Policy` (set via the admin API) and enforced
  automatically by the data-plane wrapper.
- **Backend authentication** — how ocifactory authenticates _to the
  backend OCI registry_. Pluggable `backend.Provider` interface with
  in-tree adapters for ADC, env vars, and docker config; configured
  via `OCIFACTORY_BACKEND_AUTH_*`.

There is no config file. Every knob is a CLI flag or environment
variable so deployments on Cloud Run / k8s / docker compose stay a
single deployable unit with no extra mounts. The only state outside
the binary is what lives in the OCI backend — including namespace
metadata.

## Where auth runs

`pkg/auth` is a library; the middleware it exposes
(`auth.Middleware(authn)`) is **not** installed at the server
level. Each format handler chains the middleware on its own
router, so:

- Observability endpoints (`/healthz`, `/readyz`, `/metrics`) are
  reachable without auth — they're served before any format
  handler runs.
- Today every built-in format (python, maven, npm) gates every
  route via `router.Use(authMW)` on the root router (python,
  maven) or on the `/{namespace}` sub-router (npm).
- Future formats with a public/private route split (Go module
  proxy listings, anonymous-read mirrors) can chain the middleware
  on a sub-router and leave public routes ungated.
- Outlier endpoints that need their own auth contract (e.g. a
  future npm login bootstrap that mints a token) would sit on a
  sub-router that doesn't `Use(authMW)`.

Authorization runs one layer deeper, inside the
`artifact.ScopedNamespace` wrapper: every `AddFile` / `ReadFile` /
`ListTags` / `ListFiles` call routes through the namespace's
compiled `Policy` before reaching the OCI backend, so even a
handler bug that skipped its own pre-check would not let a request
bypass authz.

## Frontend: authenticating clients

| Flag | Env var | Meaning |
|---|---|---|
| `--disable-authn` | `OCIFACTORY_AUTHN_DISABLED=true` | Wire `AlwaysAnonymous`. Local dev only — logs a loud warning. Mutually exclusive with `--authn-kind`. |
| `--authn-kind` | `OCIFACTORY_AUTHN_KIND` | Authenticator kind. Allowed: `oidc`. |
| `--authn-oidc-issuers` | `OCIFACTORY_AUTHN_OIDC_ISSUERS` | Comma-separated trusted OIDC issuer URLs. One authenticator per entry. |
| `--authn-oidc-audience` | `OCIFACTORY_AUTHN_OIDC_AUDIENCE` | Required audience claim every accepted token must carry. |

Either `--authn-kind` or `--disable-authn` must be set. The server
refuses to start without one — **no implicit "allow everything"
mode**.

```bash
ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com,https://token.actions.githubusercontent.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

Issuers are tried in declaration order. Each authenticator peeks
the unverified `iss` claim of the incoming token; only the
authenticator whose configured issuer matches will run full
verification. Mismatched issuers fall through silently to the next
authenticator, so a multi-issuer chain (Google + GitHub Actions +
...) accepts tokens from any of its members without
short-circuiting on the first one.

## Namespace authorization

Every data-plane URL ocifactory serves lives under a namespace
prefix (`/{namespace}/...` for python and npm, `/{namespace}/maven2/...`
for maven). Each namespace carries its own `Policy` — a pair of
`SubjectMatcher` lists, one for `readers` and one for `writers` —
that gates every read and write through the namespace.

The control plane is `ocifactory admin serve`; see
[`admin.md`](admin.md) for the CRUD API. The data plane
(`ocifactory serve`) reads namespace specs out of the same OCI
backend, caches the compiled authorizer (default
`DefaultPolicyCacheTTL = 60s`), and invalidates the cache the moment
an admin `Put` or `Delete` lands so policy changes take effect on
the next request.

### Op model

Today there are two ops, deliberately coarse:

| Op | What it covers |
|---|---|
| `read` | Every artifact fetch and listing (`pip install`, `mvn dependency:get`, `npm install`, `GET /simple/`, `GET <pkg>`, dist-tag GET, tarball GET, …). |
| `write` | Every publish (`twine upload`, `mvn deploy`, `npm publish`, `npm dist-tag add`). |

Granularity is per-namespace, not per-package. A caller matched by
any reader matcher can read every package in the namespace; a caller
matched by any writer matcher can write to any. Per-coordinate
granularity is intentionally deferred — operators who need it plug
in their own `Authorizer` via `artifact.WithAuthzFactory` (OPA,
Cedar, Casbin) and consume the same `AuthContext`.

### Policy shape

A namespace `Spec` looks like:

```json
{
  "schema_version": 1,
  "policy": {
    "readers": [
      {"issuer": "https://accounts.google.com"}
    ],
    "writers": [
      {"issuer": "https://token.actions.githubusercontent.com",
       "sub_match": "repo:myorg/myapp:ref:refs/heads/main"},
      {"issuer": "https://accounts.google.com",
       "email": "release-bot@myteam.example"}
    ]
  }
}
```

An **empty** policy is **deny-all**: a caller is allowed to perform
an op only if at least one matcher in the corresponding list
matches. The `readers` and `writers` lists are independent —
granting `write` does not implicitly grant `read`.

### `SubjectMatcher` reference

Every populated field on a matcher must match the corresponding
field on the verified `AuthContext` (fields AND together). An
entirely-empty matcher is rejected at admin write time.

| Field | Match against | Notes |
|---|---|---|
| `issuer` | `AuthContext.Issuer` (OIDC `iss`) | Exact string. |
| `sub_match` | `AuthContext.ID` (OIDC `sub`) | RE2 regex anchored at both ends (`^...$`). Anchors that already exist on the pattern are honoured. |
| `email` | `AuthContext.Email` (OIDC `email`, only when `email_verified=true`) | Exact string. |
| `claims_match` | Other verified claims (`AuthContext.Claims`) | Map from claim name → RE2 regex (anchored). Non-string claim values are JSON-encoded (with sorted object keys) before matching. |
| `kind` | Credential family | `"oidc"` (default) is the only supported value in v1; `"basictoken"` is reserved and rejected at validation time. |

### Worked example — restrict writes to one GitHub repo's main branch

```json
{
  "policy": {
    "readers": [
      {"issuer": "https://token.actions.githubusercontent.com",
       "sub_match": "repo:myorg/.+"}
    ],
    "writers": [
      {"issuer": "https://token.actions.githubusercontent.com",
       "sub_match": "repo:myorg/myapp:ref:refs/heads/main"}
    ]
  }
}
```

Any GitHub Actions workflow in any `myorg/*` repository can pull
artifacts; only a run on `myorg/myapp`'s `main` branch can publish.
The GitHub Actions `sub` is structured (see [Worked example: GitHub
Actions OIDC](#worked-example-github-actions-oidc)) so policies can
match on environment, ref, or workflow file precisely.

### Failure modes

| Case | Status |
|---|---|
| Unknown namespace | `404 Not Found` (`namespace not found`). |
| Authenticated caller, no matching reader/writer matcher | `403 Forbidden`. |
| Spec JSON has an unknown `schema_version` | Admin `PUT` returns `400`; data-plane `Get` returns `500` (operator must upgrade ocifactory). |
| Owning-repo with `..` / absolute / reserved-prefix path | `400 Bad Request` — defends against cross-namespace escape via crafted URL parameters. |

### Plugging in an alternative authorizer

`artifact.AuthzFactory` is `func(namespace.Policy) (auth.Authorizer, error)`.
A custom main can replace the built-in factory:

```go
import (
    "github.com/yolocs/ocifactory/pkg/artifact"
    "github.com/yolocs/ocifactory/pkg/auth"
    "github.com/yolocs/ocifactory/pkg/namespace"
)

reg := artifact.NewStore(ociReg, store,
    artifact.WithAuthzFactory(func(p namespace.Policy) (auth.Authorizer, error) {
        // compile p (or fetch a richer policy keyed by namespace name) into
        // an authorizer that consumes auth.AuthContext.
        return myOPAAuthorizer(p)
    }),
)
```

Out-of-tree authorizers slot in the same way as out-of-tree
authenticators — implement `auth.Authorizer`, wire it into your own
main, and the rest of the wiring is unchanged.

## Backend: how ocifactory talks to the OCI registry

`pkg/auth/backend.Provider` is the swap point:

```go
type Provider interface {
    Credential(ctx context.Context, host string) (Credential, error)
}
```

Out-of-tree credential providers (Vault, IAM Roles Anywhere,
SPIFFE-issued certs, ...) implement this directly and pass an
instance through `oci.WithBackendAuth`. They never touch oras-go's
auth client — that wiring is private to `pkg/oci`.

In-tree adapters are selected via flag / env var:

| Flag | Env var | Meaning |
|---|---|---|
| `--backend-auth-kind` | `OCIFACTORY_BACKEND_AUTH_KIND` | `anonymous` (default) \| `gcpadc` \| `staticenv` \| `dockerconfig` |
| `--backend-auth-gcpadc-scopes` | `OCIFACTORY_BACKEND_AUTH_GCPADC_SCOPES` | Comma-separated OAuth2 scopes for `gcpadc`. Empty = `cloud-platform`. |
| `--backend-auth-staticenv-user-env` | `OCIFACTORY_BACKEND_AUTH_STATICENV_USER_ENV` | Name of the env var holding the username for `staticenv`. |
| `--backend-auth-staticenv-password-env` | `OCIFACTORY_BACKEND_AUTH_STATICENV_PASSWORD_ENV` | Name of the env var holding the password for `staticenv`. |
| `--backend-auth-dockerconfig-path` | `OCIFACTORY_BACKEND_AUTH_DOCKERCONFIG_PATH` | Path to a docker-format `config.json` for `dockerconfig`. Empty = `~/.docker/config.json`. |

### `gcpadc` — Google Application Default Credentials

Works on Cloud Run / GCE / GKE via the metadata server, locally
via `gcloud auth application-default login`, and via Workload
Identity Federation when the environment is set. ocifactory mints
short-lived access tokens via `oauth2.TokenSource`; refresh is
handled by the upstream library.

The `host` argument to `Credential` is ignored — ADC issues bearer
tokens for any GCP service the credential has access to, so the
same token works against any GAR location.

### `staticenv` — username/password from env vars

```
--backend-auth-kind=staticenv \
--backend-auth-staticenv-user-env=REGISTRY_USERNAME \
--backend-auth-staticenv-password-env=REGISTRY_PASSWORD
```

Reads the named environment variables on **every** call so
operators can rotate secrets via Vault Agent / SOPS / External
Secrets without restarting. If either variable is unset or empty,
the adapter returns the empty credential and the backend's
`WWW-Authenticate` response surfaces the failure.

### `dockerconfig` — `~/.docker/config.json` (with credential helpers)

Wraps oras-go's `credentials.Store`. Credential helpers
(`docker-credential-gcr`, `docker-credential-ecr-login`,
`docker-credential-acr`, …) are honoured. Operators who already run
`gcloud auth configure-docker` or `aws ecr get-login-password` get
working credentials with no further configuration.

### `anonymous` — no credential

Default when `--backend-auth-kind` is unset. Every call presents
the empty credential. Fine for public read-only registries; fails
closed against any private backend, which is the safe shape.

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
all can still run with `--disable-authn` behind a network ACL.

## Adding your own authenticator (out-of-tree)

The `Authenticator` interface is public. To add an authenticator
(static password, GitHub PAT, mTLS, anything else), implement it
in a fork of `cmd/ocifactory` that wires the new authenticator
into the chain:

```go
// example.com/myocifactory/main.go
package main

import (
    "github.com/spf13/cobra"
    "github.com/yolocs/ocifactory/pkg/auth"
    "github.com/yolocs/ocifactory/pkg/auth/chain"
    // ... and the rest of the ocifactory wiring
)

type myAuth struct{ /* ... */ }
func (a *myAuth) Authenticate(r *http.Request) (*auth.AuthContext, error) { /* ... */ }
```

Backend credential providers slot in the same way: implement
`backend.Provider` and pass it through `oci.WithBackendAuth` from
your custom main.

## What's behind the load balancer

Audience claims are tied to the public URL the token-issuer was
told to mint a token for. Behind a reverse proxy this means: the
audience the **caller** specified must match the audience
**ocifactory** is configured to accept — not whatever
`X-Forwarded-Host` happens to say. Pick one canonical public URL
(`https://ocifactory.your-domain`), put it in
`--authn-oidc-audience`, and have callers use it when they request
tokens.

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

- **Per-coordinate authorization** — every reader in a namespace
  can read every package in it, every writer can write to any.
  Operators who need finer control plug in their own `Authorizer`
  via `artifact.WithAuthzFactory` (OPA, Cedar, Casbin).
- **Scoped-token issuance.** A planned `POST /admin/v1/tokens`
  endpoint would take a verified OIDC token and mint a shorter-lived
  ocifactory-audience token bound to a namespace and op; the wire
  shape is open. Tracked in [`ROADMAP.md`](ROADMAP.md#phase-3--authauthz-extensibility).
- **GitHub PATs / App tokens** — opaque, not OIDC. A future
  authenticator could validate them via `api.github.com/user`,
  but it has different perf characteristics; meanwhile the
  out-of-tree pattern above is the supported path.
- **Token revocation lists / introspection endpoints** — punt.
- **Web UI / login flows** — API-only product.
