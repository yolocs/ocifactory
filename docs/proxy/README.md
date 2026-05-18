# Pull-through proxy design

Proxy namespaces are read-through caches. On a file miss, ocifactory
checks policy, fetches the upstream artifact itself, streams the bytes
to the client, and writes the same bytes into OCI. It intentionally
does **not** redirect cold-miss clients to the upstream artifact URL
and warm the OCI cache asynchronously.

The primary reason is network topology. In many deployments,
ocifactory is the only artifact endpoint allowlisted from build
workers, CI runners, or developer networks. The public upstream
(`pypi.org`, `registry.npmjs.org`, Maven Central, or an internal
upstream in another network zone) may be blocked by firewall, egress
policy, proxy configuration, or audit controls. A redirect makes the
client open the upstream URL directly, so installs would fail exactly
in the environments where a proxy is most useful.

Keeping cold-miss bytes on the ocifactory data path also preserves the
proxy contract:

- Clients only need network access to ocifactory.
- Namespace readers can populate the cache without being granted
  publish rights.
- Filters, size caps, and audit logs stay at the same boundary as the
  artifact bytes.
- A successful cold miss has a deterministic cache-fill attempt tied
  to the request, rather than a best-effort background job that may be
  lost if the serving instance stops.
- Concurrent first misses can collapse to one upstream fetch per
  process; followers serve from OCI after the leader fills the cache.

This is distinct from **backend blob redirect** on cache hits. When an
artifact already exists in OCI, ocifactory may return a `307` to an
operator-controlled backend presigned URL, depending on backend support
and the `--disable-blob-redirect` flag. That redirect targets the OCI
storage backend, not the package upstream.
