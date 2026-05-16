# Observability

ocifactory exposes three operational endpoints and a Prometheus metrics
surface so you can run it like a normal service. Both `ocifactory serve`
and `ocifactory admin serve` expose the same endpoints — they use the
same wrapper (`handler.ObservabilityHandler`) so probes look identical
across the two processes.

## Endpoints

| Path | Purpose | Notes |
|---|---|---|
| `GET /healthz` | Liveness probe | Returns `200 ok` if the process is up. No backend call. |
| `GET /readyz` | Readiness probe | Data plane (`serve`): issues `HEAD /v2/` against the configured backend with a 2-second timeout. Admin (`admin serve`): issues a namespace `Store.List` against the same 2-second timeout. `200` on success (backends answering `200` or `401` count — a challenge proves reachability); `503` otherwise. Result is cached for 1 second so probe storms don't fan out. The response body is JSON with `status`, `backend`, and the binary's `build` block (`version`, `commit`, `os_arch`) so multi-instance deployments are diagnosable per-pod. |
| `GET /metrics` | Prometheus exposition | Path is configurable via `--metrics-path`. Disabled when `--enable-metrics=false`. |

The endpoints live on the **same listener** as the rest of the service.
There is no separate metrics port — Cloud Run / Cloudflare Workers don't
benefit from one and it complicates the deployment. If you need access
control on `/metrics`, front it with your reverse proxy.

## Flags

```
--enable-metrics=true     Expose Prometheus metrics and instrument the
                          HTTP and OCI backend layers. When false, the
                          no-op recorder is wired throughout and the
                          metrics endpoint returns 404.
--metrics-path=/metrics   Path on the main listener that serves
                          Prometheus exposition.
```

## Metrics

### HTTP layer

| Metric | Type | Labels |
|---|---|---|
| `ocifactory_http_requests_total` | counter | `format`, `op`, `status` |
| `ocifactory_http_request_duration_seconds` | histogram | `format`, `op` |
| `ocifactory_http_request_bytes_in_total` | counter | `format`, `op` |
| `ocifactory_http_response_bytes_out_total` | counter | `format`, `op` |

- `format` — the active `--repo-type` (`python`, `maven`, `npm`, …)
  or `admin` for the control-plane service. `unknown` for the
  observability endpoints themselves.
- `op` — coarse verb. Either a per-route name set by the format
  handler (`read`, `write`, `list`; or `admin_namespaces_put` /
  `admin_namespaces_get` / `admin_namespaces_delete` /
  `admin_namespaces_list` on the admin service) or a
  method-derived fallback (`read` for GET/HEAD, `write` for
  PUT/POST/PATCH, `delete` for DELETE).
- `status` — HTTP status code as a string.

### OCI backend layer

| Metric | Type | Labels |
|---|---|---|
| `ocifactory_oci_backend_requests_total` | counter | `op`, `status` |
| `ocifactory_oci_backend_request_duration_seconds` | histogram | `op` |

- `op` ∈ `push_blob`, `push_manifest`, `fetch_blob`, `fetch_manifest`,
  `exists`, `resolve`, `tag`, `delete`, `list_tags`, `list_referrers`,
  `push_blob_streaming`. Push/fetch are split by media type so
  dashboards can separate manifest churn from blob traffic.
- `status` ∈ `ok`, `not_found`, `already_exists`, `error`, or the
  numeric backend HTTP status (e.g. `503`). The `not_found` and
  `already_exists` buckets are normal-flow control labels — don't page
  on them.

### Blob redirect

| Metric | Type | Labels |
|---|---|---|
| `ocifactory_blob_redirect_total` | counter | `outcome` |

- `outcome` ∈ `redirected` (the backend returned a presigned URL the
  handler 307'd the client to), `inline` (the backend serves blobs
  itself; handler fell back to streaming), `error` (the redirect probe
  failed; handler also fell back to streaming).
- A high `redirected:inline` ratio against a hosted backend (GAR, ECR,
  ACR, GHCR, Docker Hub) confirms the egress short-circuit is doing its
  job. The metric is not emitted when `--disable-blob-redirect` is set.

### Standard collectors

The Prometheus recorder also registers `prometheus.NewGoCollector()`
and `prometheus.NewProcessCollector()`, so `go_*` and `process_*`
metrics come along for free.

## Recipes

**"Is ocifactory slow or is the backend slow?"** Compare
`rate(ocifactory_http_request_duration_seconds_sum[5m])` against
`rate(ocifactory_oci_backend_request_duration_seconds_sum[5m])`. The
gap is the time ocifactory spent inside its own handlers.

**"How many uploads per minute?"**
`sum(rate(ocifactory_http_requests_total{op="write"}[1m])) * 60`.

**Backend error rate.**
`sum(rate(ocifactory_oci_backend_requests_total{status!~"ok|not_found|already_exists"}[5m]))`.

## Pluggability

`pkg/metrics.Recorder` is the swap point. The default is
Prometheus-backed; `metrics.NoOp()` is wired when `--enable-metrics=false`.
A future OpenTelemetry recorder would implement the same interface
without touching `pkg/handler` or `pkg/oci`.
