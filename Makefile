.PHONY: build test vet docs-gen docs-build docs-serve ext-bump

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Regenerate the code-generated reference page (docs/reference/commands.md) from
# the cobra command tree. Drift-gated in CI — run after any command/flag change.
docs-gen:
	go run ./cmd/docs-gen

# Build the static site (regenerates the command reference first).
# Requires the docs toolchain: pip install -r docs/requirements.txt
docs-build: docs-gen
	zensical build

# Live-preview the site locally (regenerates the command reference first).
docs-serve: docs-gen
	zensical serve

# Bump the browser-extension version in BOTH files that must move together
# (extension/manifest.json + extension/package.json — CI's compat preflight
# fails if they differ). Usage: make ext-bump VERSION=0.3.2
ext-bump:
ifndef VERSION
	$(error usage: make ext-bump VERSION=x.y.z)
endif
	python3 scripts/release_metadata.py bump-extension --repo-root . --version $(VERSION)
