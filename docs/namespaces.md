# Namespaces

ocifactory serves artifact formats under a namespace prefix. A namespace is a
logical registry boundary backed by the same OCI registry: Python clients use
`/<namespace>/simple/`, Maven clients use `/<namespace>/maven2/`, and backend
OCI repositories for package data are prefixed with that namespace.

Namespaces carry the authorization policy for data-plane reads and writes. A
missing namespace fails closed: authenticated requests to an unknown namespace
return `404 Not Found`, and requests that do not match the namespace policy
return `403 Forbidden`.

## Namespace names

Namespace names are URL path segments. Use short, stable names such as a team,
project, or environment name:

```text
default
platform
payments-prod
```

Avoid embedding package coordinates in namespace names. Format handlers already
map package coordinates below the namespace boundary.

## Policy shape

A namespace spec contains a `policy` block. `readers` authorize package fetches
and index/list operations; `writers` authorize uploads and metadata writes.
Granting write does not imply read, so publish workflows that also verify or
resolve artifacts usually need entries in both lists.

Example spec for GitHub Actions publishing from one repository:

```json
{
  "policy": {
    "readers": [
      {
        "issuer": "https://token.actions.githubusercontent.com",
        "sub_match": "repo:octo-org/example:.*"
      }
    ],
    "writers": [
      {
        "issuer": "https://token.actions.githubusercontent.com",
        "sub_match": "repo:octo-org/example:ref:refs/heads/main"
      }
    ]
  }
}
```

Each matcher ANDs its populated fields (`issuer`, `email`, `sub_match`, and
`claims_match`). Multiple matchers in a list are ORed. Regex fields use RE2 and
are matched as full strings.

## Managing namespaces

Namespace metadata is persisted in the OCI backend itself: namespace `payments`
maps to OCI repo `payments` with tag `_metadata`, and the namespace catalog is
maintained in `_index`. No sidecar database is required.

The namespace data model and store live in `pkg/namespace`. Until an operator
CLI/API lands, deployments should create namespaces from their control-plane
code or a small administrative tool using `namespace.NewStore(...).Put(...)`.
The real-client integration harness demonstrates this bootstrap path by
seeding a `default` namespace before starting ocifactory.

## Client URL examples

| Format | Namespace | Client base URL |
|---|---:|---|
| Python / PyPI | `default` | `https://ocifactory.example/default/` (`pip` index: `/default/simple/`) |
| Maven | `default` | `https://ocifactory.example/default/maven2` |

See the per-format docs under [`docs/repos/`](repos/) for complete client
configuration.
