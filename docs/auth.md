# Authentication and authorization

ocifactory has two independent identities:

- **Frontend** — how callers of the ocifactory HTTP API authenticate
  to ocifactory. OIDC only; configured via `OCIFACTORY_AUTHN_*`.
- **Backend** — how ocifactory authenticates to the backend OCI
  registry. Pluggable `backend.Provider` interface with in-tree
  adapters for ADC, env vars, and docker config; configured via
  `OCIFACTORY_BACKEND_AUTH_*`.

Authentication asks "who is calling?". A separate **authorization**
layer (`Authorizer`, configured via `--authz-config` /
`OCIFACTORY_AUTHZ_CONFIG`) asks "what is this caller allowed to
do?". See the [Authorization](#authorization) section below.

Authentication and backend-credential knobs are CLI flags / env
vars so single-process deploys (Cloud Run, fly.io, ...) stay a
mountless unit. The authorization policy is the one exception:
operators write a YAML file describing rules and pass its path via
`--authz-config`. Mounting a file is the natural fit for tabular
data of unbounded size.

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

## Authorization

`pkg/auth.Authorizer` is the swap point for authorization policy.
The interface is small:

```go
type Authorizer interface {
    Authorize(ctx context.Context, ac *AuthContext, act Action) error
}

type Action struct {
    Repo   string  // OCI repository path, matches RepoFile.OwningRepo
    Format string  // "python", "maven", "echo", ...
    Op     Op      // OpRead | OpWrite
}
```

Per-format handlers translate the inbound HTTP request into an
`Action` and call `auth.Check(ctx, h.authz, act)` at the start of
every routed handler function. `auth.ErrUnauthorized` becomes
`403 Forbidden`; any other authorizer error becomes `500
Internal Server Error` so transient policy-system failures don't
look like a legitimate denial.

ocifactory ships:

- `pkg/auth.AllowAll` — no authz, every action allowed. Used in
  tests and behind another authz layer.
- `pkg/auth.DenyAll` — explicit deny. Used in tests.
- `pkg/auth/staticauthz` — config-file backed implementation.

Out-of-tree implementations (OPA / Casbin / Rego, GitHub team
membership, cloud IAM bindings, custom HTTP authz, ...) implement
`Authorizer` directly and pass an instance through the same
`WithAuthorizer` option from a custom main.

### Static config policy (`--authz-config`)

When `--authz-config=/path/to/policy.yaml` is set, ocifactory loads
the file at startup, compiles every regex / glob, and consults it
for every routed request. When the flag is empty, no authorization
runs and every authenticated caller can read and write everything
— acceptable for local dev, not for production. The startup log
warns when no policy is configured.

Schema:

```yaml
default: deny             # deny | allow (default: deny)
rules:
  # GitHub Actions OIDC: build-bot publishes packages/* (Python).
  - subject:
      issuer: https://token.actions.githubusercontent.com
      sub_match: "^repo:my-org/build-bot:.*"
    allow:
      - { repo: "packages/*", format: python, op: write }
      - { repo: "packages/*", format: python, op: read  }

  # Any GH Actions job in my-org can read across formats.
  - subject:
      issuer: https://token.actions.githubusercontent.com
      sub_match: "^repo:my-org/.*"
    allow:
      - { repo: "**", format: "*", op: read }

  # Dedicated Google service account: full access.
  - subject:
      issuer: https://accounts.google.com
      email: build-sa@project.iam.gserviceaccount.com
    allow:
      - { repo: "**", format: "*", op: "*" }

  # Any Google account from the example.com domain can read.
  - subject:
      issuer: https://accounts.google.com
      claims_match:
        hd: "^example\\.com$"
    allow:
      - { repo: "**", op: read }
```

**Subject matchers.** Every field is optional and ANDed; an empty
matcher matches every authenticated subject (use sparingly).

| Field | Matches against | Match style |
|---|---|---|
| `issuer` | `AuthContext.Issuer` | exact |
| `sub_match` | `AuthContext.ID` | regex |
| `email` | `AuthContext.Email` | exact |
| `claims_match` | `AuthContext.Claims[<name>]` | map of claim → regex |

**Action matchers.** `repo`, `format`, and `op` accept globs:

- `*` matches any single path segment (does not cross `/`).
- `**` matches across slashes (use this for "any repo").
- Other characters match literally — regex metacharacters in the
  pattern are escaped, so `foo.bar` requires a literal dot.
- An empty value means "any" (equivalent to `**`).

`op` is one of `read`, `write`, or `*`.

**Evaluation.** Rules are tried in order. The first rule whose
subject matches AND whose `allow` list contains a matching action
wins. If subject matches but no action matches, evaluation
continues to the next rule. If no rule allows, the configured
`default` (deny by default) decides. There is no negation / deny
list in v1; operators who need richer expressiveness implement
`Authorizer` themselves.

**Read/write granularity.** `OpRead` covers GET/HEAD reads and
listing endpoints (PyPI simple index, future npm registry root).
`OpWrite` covers PUT/POST uploads. There is no separate `delete`
or `list` op today — ocifactory does not expose a delete endpoint
to clients, and folding listing into read avoids surprising
operators with policy that lets a caller fetch a file they cannot
discover.

**Action repo names.** Match the OCI repo that the format handler
uses. For Python, that is `packages/<normalized-name>` for
artifact requests and the literal `index` for the simple-index
list endpoint; for Maven it is the `groupId/artifactId` path. The
echo format uses the literal `echo`. Writing a policy is a matter
of looking at the same names that show up in the backend OCI
registry.

### Hot-reload

The static authorizer watches the policy file and reloads on
change. No restart is needed when an operator edits the rules.

| Deploy shape | What changes | What happens |
|---|---|---|
| Plain file mount, edit in place | The file's contents | fsnotify on the parent dir picks up `WRITE`, debounces 100ms, re-reads the path. |
| kubernetes ConfigMap volume | `..data` symlink swap | Same watcher catches the burst of `CREATE` / `RENAME` / `REMOVE` events the kubelet emits during rotation. The path operators reference (`/etc/ocifactory/authz.yaml`) is itself a symlink to `..data/authz.yaml`, so re-reading the original path after the swap resolves to the new file. No additional configuration needed. |
| Atomic rename (e.g. helm upgrade) | The symlink target | Same path — the watcher sees the parent directory event and reloads. |

A reload that fails to parse, validate, or compile is logged at
`WARN` and **discarded** — the previously loaded rules stay in
effect, so a typo on disk cannot brick authorization.
Successful reloads log at `INFO` with the rule count for an
audit trail.

The reload is atomic at the rule-set level: every concurrent
`Authorize` call sees either the old rule set or the new one,
never a mix. There is no per-request locking; the swap goes
through `atomic.Pointer`.

### 401 vs 403

| Outcome | Status |
|---|---|
| No / invalid credential | 401 Unauthorized |
| Issuer unreachable | 503 Service Unavailable |
| Verified subject, lacks permission | **403 Forbidden** |
| Authorizer itself failed | 500 Internal Server Error |

The split matters for clients: 401 means "rotate / mint a token
and retry", 403 means "your identity is fine, ask the operator
for access".

## What's out of scope (for now)

- **GitHub PATs / App tokens** — opaque, not OIDC. A future
  authenticator could validate them via `api.github.com/user`,
  but it has different perf characteristics; meanwhile the
  out-of-tree pattern above is the supported path.
- **Token revocation lists / introspection endpoints** — punt.
- **Web UI / login flows** — API-only product.
