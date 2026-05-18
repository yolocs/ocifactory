# Maven

ocifactory speaks the [Maven 2 repository layout](https://maven.apache.org/repository/layout.html)
that `mvn`, `gradle`, `sbt`, and friends already know. Each uploaded
file (jar, pom, sources, javadoc, checksum companion, `maven-metadata.xml`)
becomes its own file manifest attached via `subject` to a per-version
anchor manifest, so the dozen-plus PUTs `mvn deploy` fires for one
release land independently and never read-modify-write each other.

> **How it's stored on OCI:** see
> [`docs/architecture/storage-model.md`](../architecture/storage-model.md).
> That doc walks `mvn deploy` step-by-step through the manifests and
> tags ocifactory writes for one release, including why the layout is
> race-free under `mvn deploy -T <n>`.

## Snapshots vs releases: overwrite semantics

ocifactory matches Maven's own immutability rules:

- **Snapshot versions** (any version ending in `-SNAPSHOT`,
  case-insensitive) are **always overwritable**, regardless of the
  registry's `--allow-overwrite` flag. The second `mvn deploy` of a
  `1.0-SNAPSHOT` succeeds against the default
  `--allow-overwrite=false`. This covers both the artifact files
  themselves (jars, poms, sources, javadoc, checksums) and the
  per-version `maven-metadata.xml` that tracks the latest timestamped
  build under a unique-snapshot setup.
- **Release versions** are **immutable by default**. A second deploy
  of `1.0` returns `409 Conflict` unless you started ocifactory with
  `--allow-overwrite=true`. Pass `--allow-overwrite=true` when you
  need lax behaviour for releases too (e.g. fix-the-CI-job retries,
  ephemeral staging) and add a CI gate of your own if you also want
  immutable fixed-version releases.

The artifact-level `maven-metadata.xml`
(`<groupId>/<artifactId>/maven-metadata.xml`) follows the same
release-immutability rule. With the default `--allow-overwrite=false`
the second rewrite of the artifact-level metadata 409s — workflows
that rewrite it on every deploy (some `mvn deploy:deploy-file`
invocations, custom uploaders) need `--allow-overwrite=true`.
Snapshot CI workflows are unaffected because everything under a
`-SNAPSHOT` version is always overwritable.

A typical Maven server config:

```bash
ocifactory serve \
  --repo-type=maven \
  --backend-registry=zot.local:5000/ocifactory \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

## Quickstart

After starting the server (above), create a namespace via the admin
service (see [`../admin.md`](../admin.md)). Every Maven URL lives
under `/{namespace}/maven2/...` — there is no implicit root namespace.

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/myteam \
  -H 'content-type: application/json' \
  -d '{"policy":{
        "readers":[{"issuer":"https://accounts.google.com"}],
        "writers":[{"issuer":"https://accounts.google.com",
                    "email":"release-bot@myteam.example"}]}}'
```

Then configure `~/.m2/settings.xml`:

```xml
<settings>
  <servers>
    <server>
      <id>ocifactory</id>
      <username>_oidc</username>
      <!-- mvn supports password from env via Maven 4 / settings-security; -->
      <!-- for older mvn, drop the OIDC ID token in here directly. -->
      <password>${env.OCIFACTORY_TOKEN}</password>
    </server>
  </servers>
  <profiles>
    <profile>
      <id>ocifactory</id>
      <repositories>
        <repository>
          <id>ocifactory</id>
          <url>https://ocifactory.your-domain/myteam/maven2/</url>
        </repository>
      </repositories>
    </profile>
  </profiles>
  <activeProfiles>
    <activeProfile>ocifactory</activeProfile>
  </activeProfiles>
</settings>
```

And in `pom.xml`:

```xml
<distributionManagement>
  <repository>
    <id>ocifactory</id>
    <url>https://ocifactory.your-domain/myteam/maven2/</url>
  </repository>
  <snapshotRepository>
    <id>ocifactory</id>
    <url>https://ocifactory.your-domain/myteam/maven2/</url>
  </snapshotRepository>
</distributionManagement>
```

`_oidc` is one of three sentinel usernames the auth middleware
recognises; see [`docs/auth.md`](../auth.md#how-clients-send-credentials).
The `password` is an OIDC ID token — there is no static-password path.

## Proxy mode (pull-through Maven)

Set the namespace's `mode` to `"proxy"` and point `proxy.upstream` at
the Maven 2 repository you want to mirror. For Maven Central, use
`https://repo.maven.apache.org/maven2`:

```bash
curl -X PUT https://ocifactory-admin.your-domain/admin/v1/namespaces/maven-central \
  -H 'content-type: application/json' \
  -d '{"mode":"proxy",
       "proxy":{"upstream":"https://repo.maven.apache.org/maven2"},
       "policy":{"readers":[{"issuer":"https://accounts.google.com"}]}}'
```

Point Maven at the proxy namespace exactly like a hosted namespace:

```xml
<repository>
  <id>ocifactory-maven-central</id>
  <url>https://ocifactory.your-domain/maven-central/maven2/</url>
</repository>
```

What changes on the wire:

- **Artifact metadata** (`/{namespace}/maven2/<group>/<artifact>/maven-metadata.xml`)
  and **snapshot-version metadata**
  (`/{namespace}/maven2/<group>/<artifact>/<version>-SNAPSHOT/maven-metadata.xml`)
  are always fetched live from upstream and served byte-for-byte. Maven
  metadata is small and contains no absolute URLs, so ocifactory does
  not use the OCI index cache for Maven.
- **Artifact files** (`.jar`, `.pom`, sources, javadoc, and checksum
  siblings such as `.sha1` / `.md5`) check OCI first. On a miss,
  ocifactory runs the namespace's filter chain, fetches upstream
  metadata to confirm the version and get `<lastUpdated>` for delay
  filters, streams the file from upstream to the client, and tees the
  same bytes into OCI. The second request for the same file serves from
  the OCI cache.
- **Writes** (`PUT` / `POST`) return `405 Method Not Allowed` on proxy
  namespaces. Publish to a hosted namespace when ocifactory should be
  the source of truth.

Degraded operation is intentionally simpler than PyPI/npm: upstream
metadata errors return `503 Service Unavailable`, and file fetch errors
return `404` for upstream not found or `502 Bad Gateway` for malformed /
unavailable upstream responses. There is a short in-memory negative
cache for repeated file 404s, but no stale metadata fallback in v1.

Filters use Maven package refs in `groupId:artifactId` form:

```json
{
  "mode": "proxy",
  "proxy": {
    "upstream": "https://repo.maven.apache.org/maven2",
    "filters": [
      {"kind": "deny", "patterns": ["com.example.bad:*"]}
    ]
  }
}
```

The filter chain is the same one documented in
[`docs/proxy/filter-policy.md`](../proxy/filter-policy.md): name
allowlist / denylist filters run before any upstream call, and the
publish-time `delay` filter runs after `maven-metadata.xml` is fetched
using `<versioning><lastUpdated>` as Maven's upload-time proxy.

## URL layout

Every route lives under `/{namespace}/maven2/...`. The `maven2/`
segment after the namespace is a fixed format prefix — it lets one
hostname route python, maven, and npm under disjoint paths per
namespace without ambiguity. Requests to an unknown namespace return
`404 Not Found`; requests whose verified OIDC subject doesn't match
the namespace's `readers` / `writers` policy return `403 Forbidden`.

All routes accept `PUT` and `POST` for upload, and `GET` and `HEAD`
for resolve.

| Path (under `/{namespace}/maven2/`) | Purpose |
|---|---|
| `/{groupId}/{artifactId}/{version}/{filename}` | Regular artifact (jar, pom, sources, javadoc). |
| `/{groupId}/{artifactId}/{version}/{filename}.{sha1,md5,sha256,sha512}` | Checksum companion. **Must arrive after the artifact it covers.** |
| `/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | Version-level snapshot metadata. Always overwritable (snapshot-mutable). |
| `/{groupId}/{artifactId}/maven-metadata.xml` | Artifact-level metadata. Follows release-immutability semantics — see the [Snapshots vs releases](#snapshots-vs-releases-overwrite-semantics) section above. |
| `/archetype-catalog.xml` | Global archetype catalog (one per namespace). |

`{groupId}` is the dotted Java group with `.` rewritten to `/`
(e.g. `com.example.foo` → `/com/example/foo/`), matching standard
Maven 2 layout. Both release and snapshot artifacts use the same
URL shape; the routing differs only on the `-SNAPSHOT` suffix in the
version segment.

### Concrete examples

```
PUT /myteam/maven2/com/example/foo/1.0.0/foo-1.0.0.jar
PUT /myteam/maven2/com/example/foo/1.0.0/foo-1.0.0.jar.sha1
PUT /myteam/maven2/com/example/foo/1.0.0/foo-1.0.0.pom
PUT /myteam/maven2/com/example/foo/1.0.0/foo-1.0.0.pom.sha1
PUT /myteam/maven2/com/example/foo/maven-metadata.xml
PUT /myteam/maven2/com/example/foo/maven-metadata.xml.sha1

# Snapshot
PUT /myteam/maven2/com/example/foo/1.1.0-SNAPSHOT/foo-1.1.0-20260101.123456-1.jar
PUT /myteam/maven2/com/example/foo/1.1.0-SNAPSHOT/maven-metadata.xml
```

## Path validation grammar

Each segment of the URL must match `[A-Za-z0-9._-]+`. Specifically:

- No `..` or `.` segments (rejected with `400 Bad Request`).
- No empty segments (no leading, trailing, or doubled slashes).
- No characters outside the conservative class above. Maven's standard
  groupId / artifactId / version grammar
  (`[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*`) fits inside this class with
  room to spare; if your build uses unusual characters in coordinates,
  ocifactory will reject the deploy at the handler boundary before it
  reaches the OCI backend.

Reference: `pkg/handler/maven/paths.go`.

## Supported client commands

| Command | Status | Reference |
|---|---|---|
| `mvn deploy` (release) | ✅ Supported | `pkg/handler/maven/integration_test.go` — `release_deploy_and_get` |
| `mvn deploy` (snapshot) | ✅ Supported | `pkg/handler/maven/integration_test.go` — `snapshot_deploy_round_trip` |
| `mvn dependency:get` | ✅ Supported | `pkg/handler/maven/integration_test.go` — `release_deploy_and_get` |
| `gradle publish` | ⚠️ Untested but expected to work | Same Maven 2 protocol as mvn; report bugs. |
| `mvn deploy:deploy-file` (raw upload) | ⚠️ Works only if the caller writes `maven-metadata.xml` themselves | ocifactory does not synthesize metadata — see [Limitations](#limitations). |

The integration tests are gated behind `-tags=integration`; CI runs
them in the dedicated client-integration job. Locally:

```bash
go test -tags=integration ./pkg/handler/maven/...
```

## Retrying a failed `mvn deploy`

`mvn deploy` issues ~12 PUTs per artifact (jar, pom, sources, javadoc,
their checksums, and the artifact-level `maven-metadata.xml`). If the
run is interrupted partway through, the files that landed are durably
on the backend and the rest are not — including, often,
`maven-metadata.xml`, which `mvn` writes last.

The right recovery depends on whether you're deploying a snapshot or a
release.

**Snapshot (`<version>-SNAPSHOT`):** snapshot versions are
always-overwrite by design, so the safe retry is just:

```bash
mvn deploy
```

Every file that already landed gets overwritten with the new build's
copy, and the files that didn't land get pushed. No additional flags
needed.

**Release (`<version>`, no `-SNAPSHOT`):** the safe retry depends on
your overwrite policy.

- If you run ocifactory with `--allow-overwrite=true` (which the
  operator-requirement warning at the top of this doc says every
  working Maven deployment needs anyway), re-running `mvn deploy`
  works the same as for snapshots — it overwrites whatever landed
  and pushes the rest.
- If you need stricter release immutability, adopt a Sonatype-style
  staging workflow: deploy first to a staging namespace (or a separate
  ocifactory instance), validate end-to-end, then promote the
  artifacts (`oras cp` or a publisher pipeline) to your release repo.
  The staging repo absorbs the partial-deploy risk; the release repo
  only ever sees complete sets.

Either way, `mvn deploy -DretryFailedDeploymentCount=N` is worth
enabling: it retries the *failing PUT* in-process without re-running
the whole deploy. If the failure is network-level and the earlier
PUTs landed cleanly, that's the cheapest recovery and skips
everything above.

See [`../operations/partial-uploads.md`](../operations/partial-uploads.md)
for the full picture, including why per-version atomicity isn't a
property the Maven 2 wire protocol provides on its own.

## OCI storage layout

Each `(OCI repo, canonical tag)` tuple holds one version anchor
manifest tagged with the canonical tag, plus one file manifest per
uploaded file (subject-linked to the anchor, addressable via the
OCI 1.1 referrers API, and individually tagged with a deterministic
`_f_<sha256>` for fast reads). The mapping from Maven URL to the tuple
the file lands under (with `<ns>` standing for the URL's namespace
segment):

| Maven URL | OCI repo | Canonical tag | File name on the file manifest |
|---|---|---|---|
| `/{ns}/maven2/{groupId}/{artifactId}/{version}/{filename}` | `<ns>/{groupId}/{artifactId}` | `{version}` | `{filename}` |
| `/{ns}/maven2/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | `<ns>/{groupId}/{artifactId}` | `{version}-SNAPSHOT-metadata` | `maven-metadata.xml` |
| `/{ns}/maven2/{groupId}/{artifactId}/maven-metadata.xml` | `<ns>/{groupId}/{artifactId}` | `metadata` | `maven-metadata.xml` |
| `/{ns}/maven2/archetype-catalog.xml` | `<ns>/archetype` | `latest` | `archetype-catalog.xml` |

`{groupId}` keeps its slash form (`com/example/foo`), matching the URL.
The namespace wrapper adds the `<ns>/` prefix transparently; the maven
handler addresses repos as `{groupId}/{artifactId}` and the wrapper
turns them into `<ns>/{groupId}/{artifactId}` before they hit the OCI
backend.

A fourth per-namespace repo, `<ns>/ocifactory-packages`, is maintained
by the namespace wrapper itself — its tags enumerate every owning-repo
the wrapper has written to. It backs the admin service's "namespace is
empty" check on soft delete; operators don't write to it directly.

The base `artifactType` is `application/vnd.ocifactory.maven` and
ocifactory appends `.version`, `.file`, and `.alias` suffixes to
distinguish the three manifest kinds — operators inspecting the
registry see e.g. `application/vnd.ocifactory.maven.file` on the JAR.

For the manifest graph and a worked example walking `mvn deploy`'s
~12 PUTs through to the per-file `AddFile` calls (and why parallel
reactor mode `-T <n>` doesn't lose files), see
[`docs/architecture/storage-model.md`](../architecture/storage-model.md).

## Checksum verification

Every checksum companion (`.sha1`, `.md5`, `.sha256`, `.sha512`) is
verified at upload time against the previously-uploaded artifact in
the same `(OwningRepo, OwningTag)`. Failures:

| Failure | Status | Reason |
|---|---|---|
| Companion artifact not yet uploaded | `400 Bad Request` | Checksums must arrive **after** the file they cover. mvn does this naturally; raw `curl` uploaders need to ensure ordering. |
| Body is not a hex digest | `400 Bad Request` | "Malformed checksum body". |
| Computed digest disagrees with the body | `400 Bad Request` | "Checksum mismatch". |
| Body exceeds 1 KiB | `400 Bad Request` | DoS guard — sha512 hex is 128 bytes; legitimate bodies are far below the cap. |

The parser tolerates three common body formats:

```
9b8a3a8a4a8c…                          # bare hex
9b8a3a8a4a8c…  foo-1.0.0.jar           # GNU coreutils style (two-space-separated)
9b8a3a8a4a8c… *foo-1.0.0.jar           # BSD-style (asterisk-prefixed filename)
```

Anything past the first whitespace run is ignored; the digest is
compared case-insensitively.

For `.sha256` the OCI backend already records the digest at push time,
so verification reuses it without re-streaming the blob. `.sha1`,
`.md5`, and `.sha512` re-hash the companion at the edge — fine in
practice because checksum files arrive immediately after the artifact
(warm in the backend's cache).

Reference: `pkg/handler/maven/checksum.go`.

## Authentication and authorization

Every Maven route is gated by `pkg/auth.Middleware` — there are no
public-by-default endpoints in the Maven format. The middleware chain
runs **before** any route handler, so an unauthenticated request gets
a `401 Unauthorized` before touching the OCI backend.

Configure the authenticator via `--authn-*` (or `--disable-authn` for
local dev). See [`docs/auth.md`](../auth.md) for the full table.

Authorization is **per-namespace**: every read and write is checked
against the namespace's `Policy` (the `readers` / `writers` matchers
operators wrote when they created the namespace via the admin API).
Authenticated callers whose `(issuer, sub, email, claims)` don't match
any matcher get a `403 Forbidden`. Granularity is coarse — `OpRead`
covers blob fetches and metadata reads, `OpWrite` covers every PUT/POST.
There is no per-coordinate granularity today: every reader in a
namespace can read every coordinate in it, every writer can write to
any. Out-of-tree authorizers (OPA / Cedar / Casbin) plug in via
`namespace.WithAuthzFactory` if you need finer control.

See [`docs/auth.md`](../auth.md#namespace-authorization) for the
policy model and matcher reference.

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--allow-overwrite` | `false` | Whether to allow re-uploading release-version files. Snapshot versions (`-SNAPSHOT`) are *always* overwritable regardless of this flag — see [Snapshots vs releases](#snapshots-vs-releases-overwrite-semantics). Flip to `true` if your workflow also re-publishes the artifact-level `maven-metadata.xml` on every deploy. |
| `--maven-max-upload-bytes` | `1073741824` (1 GiB) | Caps the total request body the upload endpoint accepts. Defends against an authenticated client streaming arbitrary bytes to burn instance hours / egress before the OCI backend rejects the layer. Set to `0` to disable; tighten for cost-sensitive deployments. Oversize requests are rejected with `413 Payload Too Large` before they touch the OCI backend. |
| `--disable-streaming-push` | `false` | Force buffered+monolithic uploads through the OCI backend instead of chunked PATCH. Set only if your backend has broken chunked-PATCH support. |
| `--disable-blob-redirect` | `false` | Disable `307` redirects to backend-issued presigned URLs on blob downloads. Set when exposing backend URLs to clients is unacceptable (egress restrictions, DLP, audit). |

Common server-wide flags (`--port`, `--backend-registry`,
`--enable-metrics`, the `--authn-*` and `--backend-auth-*` families)
are documented in [`docs/auth.md`](../auth.md) and
[`docs/observability.md`](../observability.md).

## Limitations

- **Passive store: ocifactory does not synthesize `maven-metadata.xml`.**
  The client (`mvn`, `gradle`) writes it. Tools that don't write it —
  raw `curl` deploys, custom uploaders, hand-rolled scripts — must
  construct the metadata themselves. The handler stores whatever bytes
  the client uploads, byte-for-byte.
- **Single global archetype catalog.** `/archetype-catalog.xml` is one
  OCI tag in one OCI repo (`archetype/latest`). Multi-tenant catalogs
  (per-team, per-environment) are not supported.
- **No timestamp-based snapshot resolution synthesis.** Timestamped
  filenames like `foo-1.0-20260101.123456-1.jar` are stored as plain
  artifacts; the `<snapshot>` indirection in `maven-metadata.xml` is
  whatever the client uploaded. Tools that depend on the registry
  generating the indirection will not get it.
- **No staging / promotion workflow.** Every deploy is final. There is
  no "stage to a sandbox repo, then promote to releases" feature.
- **Proxy mode has live metadata only.** `maven-metadata.xml` is
  fetched from upstream on every proxy metadata request. If upstream is
  unavailable, ocifactory returns `503` rather than serving stale or
  synthesizing Maven metadata.
- **`--allow-overwrite=true` loosens immutability for every release
  file type.** Snapshot versions are always overwritable by design,
  independent of the flag. Operators who want immutable release jars
  but also need to re-publish the artifact-level `maven-metadata.xml`
  should leave the flag at its default (`false`) and use a Maven
  workflow that publishes only snapshots, or accept that toggling the
  flag will let release artifacts be overwritten too and add a CI
  gate that refuses re-deploys of fixed-version artifacts.
