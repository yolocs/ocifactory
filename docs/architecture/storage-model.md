# Storage model

This doc describes what ocifactory writes to its OCI backend for a
single published version. It's the canonical reference for the
on-registry shape — the per-format docs under [`../repos/`](../repos/)
link here from their "How it's stored" sections.

The implementation lives in [`pkg/oci/registry.go`](../../pkg/oci/registry.go);
the deterministic file-tag helper lives in
[`pkg/oci/filetag.go`](../../pkg/oci/filetag.go). The data-plane
namespace wrapper that prefixes every backend repo with `<namespace>/`
before reaching `pkg/oci` lives in
[`pkg/namespace/registry.go`](../../pkg/namespace/registry.go); this
doc shows the shape `pkg/oci` ends up writing, so every repo path
below includes the namespace prefix the wrapper adds.

## TL;DR

For one published version of a package, the OCI backend holds:

- **One version manifest** — a constant-size anchor with no real layers,
  tagged with the canonical version string (e.g. `2.31.0`). This is the
  thing aliases and file manifests `subject`-link to.
- **One file manifest per file** — single layer is the file blob,
  `subject` is the version manifest, addressed via the
  [OCI 1.1 referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers).
  Each file manifest also gets a deterministic `_f_<sha256>` tag so
  reads resolve in a single round-trip.
- **One alias manifest per ref tag** (`latest`, `stable`, an npm
  dist-tag, …) — empty layers, `subject` is the version manifest, the
  canonical version recorded in the
  `ocifactory.alias.target` annotation. Tagged with the alias name.

Three distinct `artifactType` suffixes — `.version`, `.file`,
`.alias` — let an operator inspecting the registry tell the manifest
kinds apart at a glance.

## Manifest graph

```
                  alias manifest (tag: latest)               alias manifest (tag: stable)
                  artifactType: ...alias                     artifactType: ...alias
                  ann: ocifactory.alias.target=2.31.0        ann: ocifactory.alias.target=2.31.0
                              │                                          │
                              │ subject                                  │ subject
                              ▼                                          ▼
                     ┌─────────────────────────────────────────────────────┐
                     │  version manifest      (tag: 2.31.0)                │
                     │  artifactType: application/vnd.ocifactory.<fmt>.version │
                     │  layers: [empty.json sentinel]                      │
                     └────▲──────────────▲──────────────▲─────────────────┘
                          │ subject      │ subject      │ subject
              ┌───────────┘              │              └───────────┐
              │                          │                          │
   file manifest                 file manifest                 file manifest
   (tag: _f_<sha256>)            (tag: _f_<sha256>)            (tag: _f_<sha256>)
   artifactType: ...file         artifactType: ...file         artifactType: ...file
   ann.title = sdist             ann.title = wheel-cp310       ann.title = wheel-cp311
   ann.digest = sha256:…         ann.digest = sha256:…         ann.digest = sha256:…
   layer: <sdist blob>           layer: <wheel blob>           layer: <wheel blob>
```

Tags exposed to callers via `ListTags` are filtered to just the
canonical version tag and any alias tags; the deterministic `_f_*`
file tags are stripped on the way out so callers see the same
"versions + aliases" view they'd expect from any registry.

## Annotations and artifactType constants

From [`pkg/oci/registry.go`](../../pkg/oci/registry.go):

| Constant | Value | Where it lives |
|---|---|---|
| `FileNameAnnotation` | `ocifactory.file.title` | File manifests, mirrored onto the layer descriptor. Lets the referrers index surface the filename without fetching each manifest body. |
| `FileDigestAnnotation` | `ocifactory.file.digest` | File manifests. Lets `ListFiles` learn the blob digest from the referrer entry without fetching each manifest. |
| `AliasTargetAnnotation` | `ocifactory.alias.target` | Alias manifests. Records the canonical version tag the alias points at, in human-readable form. |
| `versionArtifactSuffix` | `.version` | Appended to the operator-configured base type (e.g. `application/vnd.ocifactory.python.version`). |
| `fileArtifactSuffix` | `.file` | Same shape (`...python.file`). |
| `aliasArtifactSuffix` | `.alias` | Same shape (`...python.alias`). |

Operators set the base type via `WithArtifactType(...)`; the per-format
handlers pass `application/vnd.ocifactory.python`,
`application/vnd.ocifactory.maven`, `application/vnd.ocifactory.npm`,
etc. The three subtypes are derived in `NewRegistry`.

## Worked example — Python release (`twine upload dist/*`)

A user runs:

```bash
twine upload --repository-url https://ocifactory.your-domain/myteam/ dist/*
```

with `dist/` containing:

```
requests-2.31.0.tar.gz
requests-2.31.0-py3-none-any.whl
requests-2.31.0-cp311-cp311-manylinux_2_17_x86_64.whl
```

`twine` issues **three independent POSTs** to ocifactory's
`/{namespace}/` endpoint — one per file. They may arrive in any order
and may overlap if `twine` is parallelised across CI workers. The
namespace wrapper resolves each request to the `myteam` namespace's
authorizer, prefixes the python handler's `packages/requests`
owning-repo with the namespace segment, and forwards to `pkg/oci`.

Each POST translates to one `Registry.AddFile` call. For the first one
to land (say the sdist):

1. **Probe** for an existing `requests-2.31.0.tar.gz` referrer under
   the `2.31.0` tag. There isn't one (the version tag doesn't exist
   yet) — proceed.
2. **Push the blob.** Either monolithic (small bodies) or chunked PATCH
   (bodies above `uploadMemThreshold`). The blob is content-addressed,
   so concurrent uploads of the same bytes deduplicate.
3. **Ensure the version manifest.** `Resolve("2.31.0")` returns
   `ErrNotFound`, so we pack the constant-size version manifest
   (artifactType `application/vnd.ocifactory.python.version`, no real
   layers) and tag it `2.31.0`. The version manifest digest is
   deterministic — concurrent first-writers race the `Tag` call but
   both push the same content, so the loser is a no-op.
4. **Push the file manifest.** `subject = versionDesc`, single layer is
   the sdist blob, `artifactType = application/vnd.ocifactory.python.file`,
   annotations set `ocifactory.file.title = requests-2.31.0.tar.gz` and
   `ocifactory.file.digest = sha256:…`.
5. **Tag the file manifest** with `_f_<sha256("2.31.0\0requests-2.31.0.tar.gz")>`
   so future `ReadFile` calls resolve it in one round-trip.

The next two POSTs (the two wheels) take the same path but
short-circuit at step 3 — `Resolve("2.31.0")` now returns the version
manifest from the first call, the artifactType matches
`...python.version`, so we reuse it. Each wheel writes its own file
manifest pointing at the same version anchor, gets its own
deterministic `_f_*` tag, and never touches the version manifest's
bytes.

**No read-modify-write anywhere on the path.** Three concurrent CI
workers running `twine upload` against three different files of the
same release race only on the version-manifest creation, which is
idempotent (same content, same digest). Nobody loses a file.

## Worked example — Maven release (`mvn deploy`)

A user runs:

```bash
mvn deploy
```

for `com.example:foo:1.0.0` with a normal release configuration. `mvn`
fires roughly 12 PUTs:

```
PUT /com/example/foo/1.0.0/foo-1.0.0.jar
PUT /com/example/foo/1.0.0/foo-1.0.0.jar.md5
PUT /com/example/foo/1.0.0/foo-1.0.0.jar.sha1
PUT /com/example/foo/1.0.0/foo-1.0.0.pom
PUT /com/example/foo/1.0.0/foo-1.0.0.pom.md5
PUT /com/example/foo/1.0.0/foo-1.0.0.pom.sha1
PUT /com/example/foo/1.0.0/foo-1.0.0-sources.jar
PUT /com/example/foo/1.0.0/foo-1.0.0-sources.jar.md5
PUT /com/example/foo/1.0.0/foo-1.0.0-sources.jar.sha1
PUT /com/example/foo/1.0.0/foo-1.0.0-javadoc.jar
PUT /com/example/foo/1.0.0/foo-1.0.0-javadoc.jar.md5
PUT /com/example/foo/1.0.0/foo-1.0.0-javadoc.jar.sha1
PUT /com/example/foo/maven-metadata.xml
PUT /com/example/foo/maven-metadata.xml.sha1
```

Each PUT lands as one `AddFile` call against the
`<namespace>/com/example/foo` OCI repo (the maven handler addresses
the repo as `com/example/foo`; the namespace wrapper prefixes it
before forwarding). The first one (jar, say) walks the same five
steps as the python sdist above: probe, push blob, ensure `1.0.0`
version manifest, push file manifest with `subject = versionDesc`,
tag with deterministic `_f_*` tag.

The other 13 PUTs short-circuit at step 3 — the version manifest
already exists — and each one writes its own independent file
manifest. **None of them rewrite the version manifest, the jar
manifest, or each other.**

Parallel reactor mode (`mvn deploy -T 4`) interleaves PUTs across
modules. Because every file manifest is independent, there's no shared
mutable state to race on. Two workers PUTting two different files
under the same version manifest both succeed; two workers PUTting the
same file race on the deterministic `_f_*` tag, and last-writer-wins
on the tag, with the loser's file manifest left as an unreferenced
manifest the backend's GC eventually reaps.

The artifact-level `maven-metadata.xml` (the URL at
`/{namespace}/maven2/com/example/foo/maven-metadata.xml`) lands as a
file in a separate `metadata` tag on the same
`<namespace>/com/example/foo` repo — see
[`docs/repos/maven.md`](../repos/maven.md) for the full URL-to-tag
mapping.

## Read path

`Registry.ReadFile` resolves a file in **two backend round-trips on
the hot path**, regardless of how many files exist under the version:

1. **Resolve** `_f_<sha256(canonicalTag\0filename)>` directly. The
   helper is `fileTagFor` in [`pkg/oci/filetag.go`](../../pkg/oci/filetag.go);
   it produces the same tag the write path attached. The Resolve
   returns the file manifest descriptor.
2. **Fetch the file manifest body**, read its single layer descriptor,
   and stream the blob.

The naive referrers walk — Resolve version → Referrers list → match
filename → Fetch file manifest descriptor → Fetch blob — is four
serial round-trips. At a typical 30ms backend RTT that's ~60ms saved
per file, which compounds into seconds across a cold
`mvn dependency:resolve` or `npm install`.

The alias path adds one round-trip on top: `Resolve(refTag)` followed
by a fetch of the alias manifest body to read
`ocifactory.alias.target` and recover the canonical version string,
which is then fed into `fileTagFor` exactly as above. The alias path
still skips the Referrers list.

## Why not a fat version manifest?

Earlier iterations of the storage layout used a single fat version
manifest per `(OwningRepo, OwningTag)` whose layers were the files. It
was switched out for the OCI 1.1 referrers layout for one concrete
reason: **per-file PUTs from real client tools force a read-modify-write
cycle on every file, and the OCI distribution spec has no compare-and-set
primitive on manifest writes.**

A fat-manifest write looks like:

1. GET the current version manifest.
2. Append the new layer descriptor.
3. PUT the new manifest.
4. Re-tag.

Two writers running steps 1–4 concurrently against the same version
both read the same manifest in step 1, both compute different
"correct" successor manifests in step 2, both PUT in step 3, and the
later writer's PUT overwrites the earlier one's tag — silently losing
the earlier writer's file. Per-file delete and per-file overwrite have
the same RMW shape and the same race.

The two real-world client behaviours that hit this:

- **Twine.** Each wheel / sdist is a separate POST. A release with one
  sdist plus three platform wheels is four independent HTTP requests,
  arriving in arbitrary order, possibly from parallel CI workers.
- **`mvn deploy`.** Roughly a dozen PUTs per release (artifact + POM +
  sources + javadoc, each with two or three checksum companions, plus
  `maven-metadata.xml`). Reactor mode (`mvn deploy -T <n>`) interleaves
  PUTs from multiple modules concurrently.

The OCI 1.1 referrers layout sidesteps the race entirely: each file
gets its own immutable manifest with `subject = versionDesc`. Concurrent
writers race only on the version-manifest creation, which is
content-addressable and idempotent.

A second motivation: the referrers layout matches how cosign / SBOM /
attestation tooling already attaches signatures to artifacts. Each
file manifest has its own digest you can sign independently. A future
"sign every wheel on publish" feature drops in without any storage-side
plumbing.

The deterministic `_f_*` file tag exists so the read path stays cheap
despite the extra indirection — same RTT count as the old fat-manifest
shape would have given on the hot path.
