# Authentication

ocifactory authenticates every inbound request through a pluggable
chain of authenticators. Out of the box it ships:

- **OIDC** — verify ID tokens issued by any RFC-7591 compliant
  issuer. Worked examples below for Google and GitHub Actions.
- **Basictoken** — static `(username, bcrypt-hashed-password)` list
  for environments where OIDC isn't viable.

The chain is configured via a YAML file passed to `--auth-config`.
No other process state is involved; reload by restarting.

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
  - kind: basictoken
    file: /etc/ocifactory/tokens.yaml
```

Run with:

```bash
ocifactory serve \
  --repo-type=python \
  --backend-registry=zot.local:5000/ocifactory \
  --auth-config=/etc/ocifactory/auth.yaml \
  --port=8080
```

Authenticators are tried in declaration order. The first one that
recognises the request's credential format gets to verify it; if
verification fails, the request is rejected (later authenticators
don't get a second pass at a credential one of them already
rejected).

### Required vs optional

Either `--auth-config` or `--disable-auth` must be set. The server
refuses to start without one or the other — **no implicit
"allow everything" mode**. `--disable-auth` exists for local
development against zot-with-no-auth and prints a loud warning at
startup.

## How clients send credentials

Two presentations are supported, in this order:

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

## Basictoken (static list)

Tokens are stored as bcrypt hashes; plaintext is rejected at load
time so a malformed config can't silently allow weaker auth.

Generate hashes with `htpasswd -nbB` (Apache utils) or any bcrypt
CLI:

```bash
htpasswd -nbB -C 12 alice mypassword
# alice:$2y$12$LkJ4FJ7yh3.f.../...
```

```yaml
# /etc/ocifactory/tokens.yaml
users:
  alice: $2y$12$LkJ4FJ7yh3.f.../...
  bob:   $2y$12$3NyXQbJ8ABCD.../...
```

The sentinel usernames (`_oidc`, `oauth2accesstoken`, `_token`)
are reserved and cannot appear in this file. Loading a config with
any of them returns an error.

## What's behind the load balancer

Audience claims are tied to the public URL the token-issuer was
told to mint a token for. Behind a reverse proxy this means: the
audience the **caller** specified must match the audience
**ocifactory** is configured to accept — not whatever
`X-Forwarded-Host` happens to say. Pick one canonical public URL
(`https://ocifactory.your-domain`), put it in the auth config, and
have callers use it when they request tokens.

## Failure modes

- **No credential** → `401 Unauthorized` with
  `WWW-Authenticate: Bearer realm="ocifactory"` and
  `WWW-Authenticate: Basic realm="ocifactory"`.
- **Bad token** (signature / issuer / audience / expiry) →
  `401 Unauthorized`. Body is intentionally generic; details are
  in the server log at debug level.
- **Issuer unreachable** (JWKS endpoint down, discovery doc
  unfetchable) → `503 Service Unavailable`. The operator can
  distinguish "your credential is bad" from "I can't even check
  your credential right now". JWKS state is cached after the first
  successful fetch; transient network blips don't kick everything
  to 503.

## Multi-issuer chains

Stack as many `oidc` entries as you trust:

```yaml
authenticators:
  - kind: oidc
    issuer: https://accounts.google.com
    audience: https://ocifactory.your-domain
  - kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: https://ocifactory.your-domain
  - kind: basictoken
    file: /etc/ocifactory/tokens.yaml
```

The chain's request-parsing semantics:

- A Bearer header reaches each `oidc` authenticator in order; the
  first one that accepts the issuer wins. If none does, the chain
  returns 401.
- A regular Basic header (non-sentinel username) skips every
  `oidc` authenticator (they return `ErrNoCredential`) and lands
  on `basictoken`.
- A sentinel-Basic header (`_oidc:<token>`, etc.) is treated as a
  Bearer presentation by `oidc` and ignored by `basictoken`.

## What's out of scope (for now)

- **Authorization** — what a verified caller is allowed to do.
  Tracked separately; today every authenticated caller can
  read/write everything.
- **Backend credentials** — how ocifactory talks to GAR / ECR /
  zot. Tracked separately; today the backend must accept whatever
  identity the runtime provides (typically Cloud Run's service
  account against GAR).
- **GitHub PATs / App tokens** — opaque, not OIDC. Future
  authenticator.
- **Token revocation lists / introspection endpoints** — punt.
- **Web UI / login flows** — API-only product.
