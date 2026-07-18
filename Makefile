.PHONY: build test vet docs-gen docs-build docs-serve

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
