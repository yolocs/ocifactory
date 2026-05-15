# Partial uploads, retries, and recovery

`twine upload` and `mvn deploy` both publish a release as a sequence of
independent HTTP requests — one per file. ocifactory writes each file
atomically, but neither protocol carries a "release commit" signal, so
the *version* a release composes file-by-file. A network blip on the
third of four `twine` POSTs leaves the first two files visible to
clients and the rest absent; the same applies to `mvn deploy`.

This doc explains what ocifactory does and does not guarantee when an
upload fails partway through, and the retry idioms that recover
safely.

> **TL;DR for the impatient.** Per-file writes are atomic. Per-version
> publishes are not. Retry python with `twine upload --skip-existing
> dist/*`; retry maven snapshots by re-running `mvn deploy`; retry
> maven releases against a registry running with
> `--allow-overwrite=true`, or stage to a separate namespace and
> promote.

## Per-file atomicity is guaranteed

Every `AddFile` call lands as one of two outcomes:

- **Fully on the backend.** The blob, the file manifest with
  `subject = versionDesc`, and the deterministic `_f_<sha256>` tag are
  all present. `ReadFile` resolves it in one round-trip.
- **Not at all.** The blob upload session is aborted (digest mismatch,
  client disconnect mid-stream, server abort) before any manifest is
  written. Nothing user-visible lands.

There is no intermediate "half a wheel on disk" or "manifest without
its blob" state. Partial-byte writes are impossible — the OCI
distribution protocol commits a blob only when the final digest matches
what the client claimed, and the file manifest is pushed *after* the
blob is committed.

The storage primitives that make this work are the same ones described
in [`../architecture/storage-model.md`](../architecture/storage-model.md):
each file is its own immutable manifest, referrers-linked to the
version anchor. There is no read-modify-write cycle whose failure could
leave a manifest pointing at a half-written blob.

## Per-version atomicity is NOT guaranteed

A logical "release" — `requests-2.31.0`, `com.example:foo:1.0.0` — is
N files written by N separate HTTP requests. ocifactory has no way to
gate visibility on "all N files have arrived" because the protocols it
speaks (PEP 503 / legacy PyPI upload, Maven 2 deploy) don't carry that
signal. If request k of N fails for any reason — TCP reset, client
crash, OOM kill — the first k−1 files are durably on the backend and
the rest are not.

Clients reading the release while the upload is in flight will see the
files that have landed so far. From their perspective there is no
distinction between "a partial upload in progress" and "the publisher
only ever shipped these files".

This is **the same behaviour PyPI exhibits** and is a property of the
wire protocol, not an ocifactory bug. The next two sections walk
through what a partial deploy looks like in practice for each format.

### Python — partial `twine upload`

A release with one sdist plus three platform wheels:

```
requests-2.31.0.tar.gz
requests-2.31.0-py3-none-any.whl
requests-2.31.0-cp310-cp310-manylinux_2_17_x86_64.whl
requests-2.31.0-cp311-cp311-manylinux_2_17_x86_64.whl
```

`twine` fires four independent POSTs against `/`. Suppose POST #3 fails
mid-stream (say a TCP reset during the upload of the cp310 wheel).
State on the backend:

```
packages/requests
├── 2.31.0                                (version anchor manifest)
├── _f_<sha256("2.31.0\0requests-2.31.0.tar.gz")>   ← file manifest, present
├── _f_<sha256("2.31.0\0…py3-none-any.whl")>        ← file manifest, present
└──                                                   (cp310 missing)
                                                      (cp311 not yet attempted)
```

The `2.31.0` version manifest exists. The simple index for
`requests` lists two files. `pip install requests==2.31.0` succeeds
for users on platforms whose wheel landed (or who can fall back to the
sdist), and fails for users whose wheel didn't (no compatible wheel
found, no usable sdist on their platform).

This matches what PyPI itself does — a publisher who's interrupted
mid-upload to PyPI sees the same "first k files visible, rest missing"
state.

### Maven — partial `mvn deploy`

A single release deploy fires roughly 12 PUTs (JAR + POM + sources +
javadoc, each with two or three checksum companions, plus the
artifact-level `maven-metadata.xml`). Say PUT #7 fails — network drop
between sources upload and javadoc upload. State on the backend:

```
com/example/foo
├── 1.0.0                                (version anchor manifest)
├── _f_<…foo-1.0.0.jar>                  ← present
├── _f_<…foo-1.0.0.jar.sha1>             ← present
├── _f_<…foo-1.0.0.pom>                  ← present
├── _f_<…foo-1.0.0.pom.sha1>             ← present
├── _f_<…foo-1.0.0-sources.jar>          ← present
├── _f_<…foo-1.0.0-sources.jar.sha1>     ← present
└──                                        (javadoc and below missing,
                                            maven-metadata.xml never written)
```

The version exists, the primary jar and pom are usable, but the
artifact-level `maven-metadata.xml` (which would otherwise advertise
`1.0.0` as a known version of `com.example:foo`) has not been written.
Consumers running `mvn dependency:get -Dartifact=com.example:foo:1.0.0`
can still resolve the jar directly, but a resolver that scans
`maven-metadata.xml` to discover available versions won't see this one
yet.

## Retry idioms

### Python: `twine upload --skip-existing dist/*`

The recommended retry for an interrupted `twine upload` is:

```bash
twine upload --skip-existing dist/*
```

`--skip-existing` tells `twine` to treat a `409 Conflict` response as
"this file is already on the registry, move on" instead of erroring.
With `--allow-overwrite=false` (the default for Python, and what you
should be running — see the python doc's
[Operator knobs](../repos/python.md#operator-knobs) section),
ocifactory returns `409` for every file that already landed and accepts
the rest cleanly. The net effect is a top-up that finishes the
release.

This is the same idiom PyPI recommends for the same reason.

> **What if a file was *almost* uploaded but the digest mismatched?**
> It's not on the backend at all — the blob upload aborts on digest
> mismatch and no manifest is written. The re-upload sees no file at
> that name and pushes it cleanly. The "half-uploaded" intermediate
> state can't happen.

### Maven snapshot: re-run `mvn deploy`

Snapshot versions (`1.1.0-SNAPSHOT`) are always-overwrite by design —
that's what "snapshot" means in the Maven world. To recover from a
partial snapshot deploy, just re-run:

```bash
mvn deploy
```

Every file that already landed gets overwritten with the new build's
copy (ocifactory accepts the re-upload because snapshots have to work
this way), and the files that didn't land the first time get pushed.
No additional flags are needed.

### Maven snapshot or release: `-DretryFailedDeploymentCount=N`

`mvn deploy` itself supports per-PUT retry via the
`-DretryFailedDeploymentCount=N` flag (or the
`retryFailedDeploymentCount` configuration on the
`maven-deploy-plugin`). This retries **the failing PUT**, in-process,
without re-running the rest of the deploy. If the failure is
network-level and the earlier PUTs landed cleanly, this is the cheapest
recovery — the broken request retries, the deploy finishes, no extra
state on the backend.

Important caveat: it does **not** re-upload earlier successful PUTs and
does **not** restart a deploy that was killed entirely. For "I lost
the whole `mvn` process" recovery, fall back to one of the other
idioms in this section.

### Maven release: `--allow-overwrite=true` or staging

Maven releases (`1.0.0`, not `1.0.0-SNAPSHOT`) are conceptually
immutable, and ocifactory's default would `409` on the second deploy
of the same coordinate. In practice, however,
[`docs/repos/maven.md`](../repos/maven.md) calls out that **every
working Maven deployment must run with `--allow-overwrite=true`**
because `mvn` rewrites the artifact-level `maven-metadata.xml` on
every deploy regardless of release vs snapshot. The same flag makes
naive retry-from-scratch work for release versions too: a re-run
`mvn deploy` overwrites whatever already landed.

If your team needs stricter immutability than that — production
binaries that *truly* should never be silently re-published — adopt a
Sonatype-style staging workflow:

1. Deploy first to a staging repo (a separate ocifactory instance or a
   namespace dedicated to staging).
2. Validate end-to-end (the deploy completed, all files present,
   smoke tests pass).
3. Promote the artifacts to your release repo. Promotion can be a
   one-shot `oras cp` between OCI repos, or a publisher pipeline that
   re-uploads the staged files to the release ocifactory.

The staging repo absorbs the partial-deploy risk; the release repo
only ever sees complete sets.

## Why this is by design

Neither PyPI's nor Maven Central's wire protocol carries a "release
commit" signal. Each file upload is its own HTTP request, and the
server has no way to know whether more files are coming. ocifactory's
behaviour matches PyPI's exactly for this reason — and Maven Central's
behaviour at the bare-protocol layer, which is then wrapped in the
Sonatype Nexus staging workflow Maven Central publishers know.

Sonatype layers staging *on top of* the protocol; ocifactory does not
have an equivalent today. Release-level atomicity is a candidate
future feature — see the
[forward-reference](#release-level-atomicity-future-work) below — but
its absence isn't a bug, it's the same baseline every protocol-faithful
implementation lands on.

## Release-level atomicity (future work)

A staging-style workflow — where a deploy writes to a sandbox view and
becomes visible to clients only after an explicit "promote" call — is
a candidate feature for ocifactory. The OCI backend has the primitives
(distinct repos, atomic tag updates) to make it cheap. The hold-up is
that the value proposition is small relative to the operator UX cost:
existing retry idioms cover the common cases, and most operators land
on `--allow-overwrite=true` plus a CI gate that refuses re-deploys of
fixed-version artifacts.

If you have a concrete need for release-level atomicity, open an issue
describing the workflow. When it ships, this doc will link to it.

## See also

- [`../architecture/storage-model.md`](../architecture/storage-model.md)
  — the on-OCI shape and why per-file atomicity falls out of the
  referrers layout.
- [`../repos/python.md#retrying-a-failed-twine-upload`](../repos/python.md#retrying-a-failed-twine-upload)
  — Python-specific retry recipe.
- [`../repos/maven.md#retrying-a-failed-mvn-deploy`](../repos/maven.md#retrying-a-failed-mvn-deploy)
  — Maven-specific retry recipe.
