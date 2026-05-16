# Repository formats

Per-format operator documentation. Each page covers URL layout,
supported client commands, OCI storage shape, authn model, operator
knobs, and known limitations — enough that someone who has never seen
the codebase can configure their client against a running ocifactory
using only the page.

| Format | Status | Doc |
|---|---|---|
| Python (PyPI) | ✅ Functional | [`python.md`](python.md) |
| Maven         | ✅ Functional | [`maven.md`](maven.md) |
| npm           | ✅ Functional | [`npm.md`](npm.md) |
| Go module proxy | ⏳ Planned | — |
| Debian / apt    | ⏳ Planned | — |

Cross-cutting concerns live elsewhere:

- [`../auth.md`](../auth.md) — frontend (caller-to-ocifactory)
  authentication, per-namespace authorization model, and backend
  (ocifactory-to-OCI) credentials.
- [`../admin.md`](../admin.md) — control-plane namespace CRUD API
  (the prerequisite for serving any data-plane URL).
- [`../observability.md`](../observability.md) — `/healthz`,
  `/readyz`, `/metrics`, Prometheus metrics surface.
- [`../operations/partial-uploads.md`](../operations/partial-uploads.md)
  — what partially-failed `twine upload` / `mvn deploy` runs look
  like on the backend and the right retry idiom per format.
- [`../architecture/storage-model.md`](../architecture/storage-model.md)
  — the on-OCI shape of a single published version: version
  anchors, file manifests, alias manifests, the deterministic
  `_f_*` file tag.
- [`../ROADMAP.md`](../ROADMAP.md) — phased plan: Phase 1 core
  formats, Phase 2 deployability, Phase 3 authz, Phase 4 pull-through
  caching, Phase 5 add-ons.

## Adding a new format

Copy [`_template.md`](_template.md) and fill it in. Step 7 of the
"Adding a new repo type" checklist in
[`AGENTS.md`](../../AGENTS.md#adding-a-new-repo-type--checklist)
points at this template.
