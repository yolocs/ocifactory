# Convenience targets. The release pipeline itself runs in GitHub
# Actions (see .github/workflows/release.yml); these targets are for
# local smoke testing.

.PHONY: snapshot
snapshot: ## Build local-only release artifacts (archives + loadable docker image) without contacting GHCR.
	goreleaser release --snapshot --clean --skip=publish

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
