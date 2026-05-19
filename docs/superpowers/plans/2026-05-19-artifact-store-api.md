# Artifact Store API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor handler-facing OCI access behind a namespace-scoped artifact API centered on package, version, tag, and file nouns.

**Architecture:** Keep `pkg/oci` as the low-level OCI storage codec and add a higher-level data-plane API in `pkg/artifact`. The implementation wraps the existing `namespace.ScopedRegistry` behavior first, then handlers migrate from raw `oci.RepoFile` and repo strings to `artifact.Namespace`, `artifact.Package`, and `artifact.FileHandle` without changing the on-OCI layout.

**Tech Stack:** Go, gorilla/mux handlers, existing `pkg/oci` registry, existing `pkg/namespace` authz and policy cache, `go-cmp` for tests.

---

### Task 1: Add Artifact API Types

**Files:**
- Create: `pkg/artifact/artifact.go`
- Create: `pkg/artifact/artifact_test.go`

- [x] **Step 1: Write failing package/version/tag/file behavior tests**

`pkg/artifact/artifact_test.go` covers namespace resolution, package file put/get/list, alias tagging, invalid package paths, and backend download URL fallback.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/artifact`

Expected: FAIL because `pkg/artifact` production code does not exist yet.

- [x] **Step 3: Implement API and namespace-registry adapter**

Create `pkg/artifact/artifact.go` and `pkg/artifact/namespace.go`. The adapter should:
- expose `NewStore(registry *namespace.Registry) *Store`;
- validate namespace existence with `Spec(ctx)` before returning a scoped namespace;
- expose `Namespace.Package(name)` as a cheap value object;
- construct `oci.RepoFile` only inside `pkg/artifact`;
- keep package names namespace-relative and pass them to `namespace.ScopedRegistry`;
- implement `FileHandle.DownloadURL` through `BlobRedirectURL`;
- implement `FileHandle.Open` through `ReadFile`.

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/artifact`

Expected: PASS.

### Task 2: Migrate Shared Handler File Serving

**Files:**
- Modify: `pkg/handler/common.go`
- Create: `pkg/handler/storage.go`

- [ ] **Step 1: Write failing helper tests**

Add tests for redirect-or-stream serving through `artifact.FileHandle`.

- [ ] **Step 2: Implement helper**

Create a shared helper that contains the redirect/read/error mapping currently duplicated across Python, Maven, and npm.

- [ ] **Step 3: Run focused tests**

Run: `go test ./pkg/handler ./pkg/artifact`

Expected: PASS.

### Task 3: Migrate Python Hosted Path

**Files:**
- Modify: `pkg/handler/python/handler.go`
- Modify tests in `pkg/handler/python/*_test.go`

- [ ] **Step 1: Write failing Python tests for abstraction usage**

Add or adjust tests so Python hosted upload/read/index behavior does not depend on handler code calling raw `ListTags`, `ListFiles`, and `oci.RepoFile`. Tests may still inspect fake OCI backend state to prove storage compatibility.

- [x] **Step 2: Migrate code**

Change hosted path to:
- get `artifact.Namespace` from the request namespace;
- use `ns.Package(packageOwningRepo(pkg))`;
- use `pkg.PutFile`, `pkg.GetFile`, and `pkg.ListFiles`;
- keep `scoped.Index(packageIndexName)` as the transitional index path until index/cache files are moved fully behind package/version/file storage.

- [x] **Step 3: Run Python tests**

Run: `go test ./pkg/handler/python`

Expected: PASS.

### Task 4: Migrate npm Hosted Path

**Files:**
- Modify: `pkg/handler/npm/handler.go`
- Modify tests in `pkg/handler/npm/*_test.go`

- [ ] **Step 1: Write failing npm tests for tags**

Add test coverage that `pkg.Tag(ctx, "latest", "1.0.0")` and `pkg.ListTags(ctx)` provide the data needed for `dist-tags` and packument generation.

- [ ] **Step 2: Migrate code**

Replace hosted-path direct calls to `ListFiles`, `ListTags`, `AppendRefs`, and `ReadFile` with `artifact.Package` methods.

Progress: npm publish file writes, dist-tag writes, packument dist-tag resolution, and dist-tag read paths now use `artifact.Package`. `Package.ResolveTag` returns an `artifact.Version` so npm no longer infers tag targets by reading `package.json` through aliases and matching blob digests.

- [x] **Step 3: Run npm tests**

Run: `go test ./pkg/handler/npm`

Expected: PASS.

### Task 5: Migrate Maven Hosted Path

**Files:**
- Modify: `pkg/handler/maven/handler.go`
- Modify: `pkg/handler/maven/checksum.go`
- Modify tests in `pkg/handler/maven/*_test.go`

- [ ] **Step 1: Write failing Maven checksum test against package/file API**

Add a checksum verification test that uses the new package file API and proves snapshot overwrite behavior still sets `AllowOverwrite`.

- [ ] **Step 2: Migrate code**

Replace direct `oci.RepoFile` construction in request handlers with package/file construction inside shared helpers.

- [ ] **Step 3: Run Maven tests**

Run: `go test ./pkg/handler/maven`

Expected: PASS.

### Task 6: Final Cleanup and Compatibility

**Files:**
- Modify: `pkg/handler/common.go`
- Modify: `pkg/namespace/registry.go` comments as needed
- Modify: `docs/architecture/storage-model.md`

- [ ] **Step 1: Remove obsolete handler registry dependency where possible**

Keep low-level `namespace.ScopedRegistry` methods until all proxy/fetcher paths are migrated. Do not delete `oci.RepoFile`; it remains the low-level storage representation.

- [ ] **Step 2: Run full short verification**

Run:

```bash
gofmt -w pkg/artifact pkg/handler pkg/namespace
go test -race -short ./...
```

Expected: PASS.

- [ ] **Step 3: Commit**

Run:

```bash
git add .
git commit -m "Add artifact storage API"
```

Expected: commit created on `artifact-store-api`.

---

## Self-Review

- Spec coverage: The plan introduces a namespace-scoped data-plane API, keeps namespace metadata separate, keeps OCI as the backing implementation, and migrates handlers incrementally.
- Scope check: The full migration spans all handlers and proxies. The first PR can stop after the core abstraction and one handler migration if review size grows too large.
- Risk: Existing OCI layout and handler behavior must remain compatible; `pkg/oci` remains the low-level storage representation.
