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
| `DELETE` | `/admin/v1/namespaces/{name}[?cascade=true]` | none | `204` on success. |
| `GET` | `/admin/v1/namespaces` | none | `200 {"namespaces":[...]}`. |

Errors use `{"error":"message"}`. Invalid namespace names and invalid specs
return `400`; missing namespaces return `404`; deleting a non-empty namespace
without `?cascade=true` returns `409`.

## Cascade delete

`DELETE /admin/v1/namespaces/{name}` removes a namespace's metadata and its
entry in the global index. A namespace that still holds packages must opt in to
a cascade with `?cascade=true`; without it the call returns `409` and a body of
`{"error":"namespace is not empty; pass ?cascade=true to delete all packages"}`.
The explicit query parameter is a guardrail against typo-DELETEs nuking a
namespace full of artifacts; an empty namespace deletes either way.

```bash
# Empty namespace: either form succeeds.
curl -X DELETE https://admin.example.com/admin/v1/namespaces/empty

# Non-empty namespace: opt in.
curl -X DELETE 'https://admin.example.com/admin/v1/namespaces/team-a?cascade=true'
```

When `?cascade=true` is set, the admin service enumerates every sub-repo the
data plane has recorded for the namespace, deletes each one, drops the package
index repo, and finally removes the namespace metadata. The package index entry
for each sub-repo is dropped right after that sub-repo is deleted, so a partial
backend failure is resumable: a retried `DELETE` picks up where the previous
attempt stopped instead of restarting from scratch.

The OCI registry does not give us an atomic multi-repo transaction, so cascade
is best-effort across two boundaries:

- **Partial failure mid-cascade** returns `500` with the underlying error. The
  namespace metadata is left intact so a retry is meaningful.
- **Concurrent writes during cascade** can leave a single sub-repo's index tag
  behind (the write landed after the snapshot listing). Operators are expected
  to block external writes before issuing `DELETE`. The current implementation
  intentionally does not introduce a "deleting" state on the namespace; that
  state machine is deferred until a real user reports the race.

Deleting an unknown namespace returns `404`. Synthetic namespaces that some
future data-plane configuration may serve without storing in OCI (for example,
a `default` namespace under a forthcoming `--default-namespace-allow-all`
mode) are not visible to the admin store and likewise return `404` on `DELETE`.

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
