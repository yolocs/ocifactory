# Maven

ocifactory speaks the [Maven 2 repository layout](https://maven.apache.org/repository/layout.html)
that `mvn`, `gradle`, `sbt`, and friends already know. Each
`(groupId, artifactId, version)` becomes one OCI manifest, and the files
inside that version (jar, pom, sources, javadoc, checksum companions)
become its layers.

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

After starting the server (above), configure `~/.m2/settings.xml`:

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
| `/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | Version-level snapshot metadata. Always overwritable (snapshot-mutable). |
| `/{groupId}/{artifactId}/maven-metadata.xml` | Artifact-level metadata. Follows release-immutability semantics — see the [Snapshots vs releases](#snapshots-vs-releases-overwrite-semantics) section above. |
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

ocifactory writes one OCI manifest per `(repo, tag)` tuple, with the
uploaded files as layers. The mapping from Maven URL to OCI tuple:

| Maven URL | OCI repo | OCI tag | Layer name |
|---|---|---|---|
| `/{groupId}/{artifactId}/{version}/{filename}` | `{groupId}/{artifactId}` | `{version}` | `{filename}` |
| `/{groupId}/{artifactId}/{version}-SNAPSHOT/maven-metadata.xml` | `{groupId}/{artifactId}` | `{version}-SNAPSHOT-metadata` | `maven-metadata.xml` |
| `/{groupId}/{artifactId}/maven-metadata.xml` | `{groupId}/{artifactId}` | `metadata` | `maven-metadata.xml` |
| `/archetype-catalog.xml` | `archetype` | `latest` | `archetype-catalog.xml` |

`{groupId}` keeps its slash form (`com/example/foo`), matching the URL.

The OCI manifest `artifactType` is `application/vnd.ocifactory.maven`
so a backend with multiple ocifactory tenants (or formats) can tell
them apart.

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
- **No pull-through caching of Maven Central.** Tracked under Phase 4
  of [`docs/ROADMAP.md`](../ROADMAP.md).
- **`--allow-overwrite=true` loosens immutability for every release
  file type.** Snapshot versions are always overwritable by design,
  independent of the flag. Operators who want immutable release jars
  but also need to re-publish the artifact-level `maven-metadata.xml`
  should leave the flag at its default (`false`) and use a Maven
  workflow that publishes only snapshots, or accept that toggling the
  flag will let release artifacts be overwritten too and add a CI
  gate that refuses re-deploys of fixed-version artifacts.
