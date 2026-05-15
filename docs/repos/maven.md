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

## 🚨 Operator requirement: `--allow-overwrite=true`

**You must run `--allow-overwrite=true` for any production Maven
deployment.** The default `--allow-overwrite=false` rejects every
re-upload with `409 Conflict`, which sounds reasonable but breaks
`mvn deploy` on the second invocation of *any* artifact:

`mvn` rewrites the artifact-level `maven-metadata.xml` (the file at
`<groupId>/<artifactId>/maven-metadata.xml` that lists known versions)
on every deploy as part of the standard release/snapshot workflow.
With overwrite disabled the second push of that file 409s and the
deploy fails. The same is true for the version-level metadata of
snapshots.

Concretely, run:

```bash
ocifactory serve \
  --repo-type=maven \
  --backend-registry=zot.local:5000/ocifactory \
  --allow-overwrite=true \
  --authn-kind=oidc \
  --authn-oidc-issuers=https://accounts.google.com \
  --authn-oidc-audience=https://ocifactory.your-domain \
  --port=8080
```

This loosens immutability for **every** Maven file type, not just
metadata — operators who require immutable releases (production
binaries that should never be silently re-published) should add a CI
gate that refuses re-deploys of fixed-version artifacts. A future
code-side fix that special-cases `maven-metadata.xml` for overwrite
without affecting other files is tracked separately; until that lands,
`--allow-overwrite=true` is the only way to run a working Maven
ocifactory.

## Quickstart

After starting the server with `--allow-overwrite=true` (above),
configure `~/.m2/settings.xml`:

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
          <url>https://ocifactory.your-domain/</url>
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
    <url>https://ocifactory.your-domain/</url>
  </repository>
  <snapshotRepository>
    <id>ocifactory</id>
    <url>https://ocifactory.your-domain/</url>
  </snapshotRepository>
</distributionManagement>
```

`_oidc` is one of three sentinel usernames the auth middleware
recognises; see [`docs/auth.md`](../auth.md#how-clients-send-credentials).
The `password` is an OIDC ID token — there is no static-password path.

## URL layout

All routes accept `PUT` and `POST` for upload, and `GET` and `HEAD`
for resolve.

| Path | Purpose |
|---|---|
| `/{groupId}/{artifactId}/{version}/{filename}` | Regular artifact (jar, pom, sources, javadoc). |
| `/{groupId}/{artifactId}/{version}/{filename}.{sha1,md5,sha256,sha512}` | Checksum companion. **Must arrive after the artifact it covers.** |
| `/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | Version-level snapshot metadata. |
| `/{groupId}/{artifactId}/maven-metadata.xml` | Artifact-level metadata (mvn rewrites this on every deploy — see overwrite warning above). |
| `/archetype-catalog.xml` | Global archetype catalog. |

`{groupId}` is the dotted Java group with `.` rewritten to `/`
(e.g. `com.example.foo` → `/com/example/foo/`), matching standard
Maven 2 layout. Both release and snapshot artifacts use the same
URL shape; the routing differs only on the `-SNAPSHOT` suffix in the
version segment.

### Concrete examples

```
PUT /com/example/foo/1.0.0/foo-1.0.0.jar
PUT /com/example/foo/1.0.0/foo-1.0.0.jar.sha1
PUT /com/example/foo/1.0.0/foo-1.0.0.pom
PUT /com/example/foo/1.0.0/foo-1.0.0.pom.sha1
PUT /com/example/foo/maven-metadata.xml
PUT /com/example/foo/maven-metadata.xml.sha1

# Snapshot
PUT /com/example/foo/1.1.0-SNAPSHOT/foo-1.1.0-20260101.123456-1.jar
PUT /com/example/foo/1.1.0-SNAPSHOT/maven-metadata.xml
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

## OCI storage layout

Each `(OCI repo, canonical tag)` tuple holds one version anchor
manifest tagged with the canonical tag, plus one file manifest per
uploaded file (subject-linked to the anchor, addressable via the
OCI 1.1 referrers API, and individually tagged with a deterministic
`_f_<sha256>` for fast reads). The mapping from Maven URL to the tuple
the file lands under:

| Maven URL | OCI repo | Canonical tag | File name on the file manifest |
|---|---|---|---|
| `/{groupId}/{artifactId}/{version}/{filename}` | `{groupId}/{artifactId}` | `{version}` | `{filename}` |
| `/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | `{groupId}/{artifactId}` | `{version}-SNAPSHOT-metadata` | `maven-metadata.xml` |
| `/{groupId}/{artifactId}/maven-metadata.xml` | `{groupId}/{artifactId}` | `metadata` | `maven-metadata.xml` |
| `/archetype-catalog.xml` | `archetype` | `latest` | `archetype-catalog.xml` |

`{groupId}` keeps its slash form (`com/example/foo`), matching the URL.

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

## Authentication

Every Maven route is gated by `pkg/auth.Middleware` — there are no
public-by-default endpoints in the Maven format. The middleware chain
runs **before** any route handler, so an unauthenticated request gets
a `401 Unauthorized` before touching the OCI backend.

Configure the authenticator via `--authn-*` (or `--disable-authn` for
local dev). See [`docs/auth.md`](../auth.md) for the full table.

Authorization (per-coordinate, per-operation policy) is **not**
implemented yet — every authenticated caller can read and write every
artifact. Tracked by [#49](https://github.com/yolocs/ocifactory/issues/49).

## Operator knobs

| Flag | Default | Purpose |
|---|---|---|
| `--allow-overwrite` | `false` | **REQUIRED to set to `true` for any working Maven deployment** — see the warning at the top. |
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
- **No pull-through caching of Maven Central.** Tracked under Phase 4
  of [`docs/ROADMAP.md`](../ROADMAP.md).
- **`--allow-overwrite=true` loosens immutability for every file type.**
  Operators who want immutable release jars should add a CI gate that
  refuses re-deploys of fixed-version artifacts; ocifactory itself
  does not distinguish "metadata" from "jar" once overwrite is on.
