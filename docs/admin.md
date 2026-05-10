# Admin service

`ocifactory admin serve` runs the control-plane HTTP service. It is a separate
listener from the data plane (`ocifactory serve`) and should be deployed on a
separate hostname behind platform or network access controls.

> **Security requirement:** the admin service intentionally has no internal
> authentication or authorization middleware. Put it behind Cloud Run
> authenticated invoker, an internal load balancer ACL, an identity-aware proxy,
> or equivalent controls before exposing it to operators.

## Start the service

```bash
ocifactory admin serve \
  --backend-registry=zot.local:5000/ocifactory \
  --port=8081 \
  --namespace-prefix=control-plane
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--backend-registry` | Required. OCI registry URL that stores namespace metadata and the namespace index. |
| `--port` | Listener port. Defaults to `8081`; `PORT` is also honored for PaaS deployments. |
| `--namespace-prefix` | Optional OCI repository prefix for namespace metadata and the global namespace index. Defaults to empty. Use a lowercase OCI-safe path segment such as `control-plane`. |
| `--backend-auth-kind` and related `--backend-auth-*` flags | Same backend credential providers as the data-plane service. |
| `--enable-metrics`, `--metrics-path` | Expose and configure Prometheus metrics. |

The service logs a WARN during startup to remind operators that it trusts the
network/platform in front of it.

## HTTP API

All endpoints are JSON-only and versioned under `/admin/v1`.

| Method | Path | Body | Success response |
|---|---|---|---|
| `PUT` | `/admin/v1/namespaces/{name}` | `namespace.Spec` JSON | `201` with a `namespace.Namespace` on create; `200` on update. |
| `GET` | `/admin/v1/namespaces/{name}` | none | `200` with a `namespace.Namespace`. |
| `DELETE` | `/admin/v1/namespaces/{name}` | none | `204` when the namespace exists and is empty. |
| `GET` | `/admin/v1/namespaces` | none | `200 {"namespaces":[...]}`. |

Errors use `{"error":"message"}`. Invalid namespace names and invalid specs
return `400`; missing namespaces return `404`; soft-delete of a non-empty
namespace returns `409`.

## Soft delete

`DELETE` is intentionally conservative in this phase. ocifactory checks the
namespace package index and refuses to delete a namespace that still contains
packages. Remove package data through a future cascade-delete flow before
deleting those namespaces.

## Health, readiness, and metrics

The admin service reuses the same observability endpoints as the data plane:

- `/healthz` returns liveness.
- `/readyz` calls `Store.List` against the OCI-backed namespace index under the
  standard readiness timeout.
- `/metrics` exposes Prometheus metrics when metrics are enabled.

## Namespace metadata storage

Namespace metadata is stored with OCI artifact type
`application/vnd.ocifactory.namespace`, independent of the data-plane format
served by a process. If you experimented with namespace metadata from the short
window before this admin service existed, recreate those namespaces through the
admin API so Python, Maven, and future repo types all read the same control-plane
documents.
