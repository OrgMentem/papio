.PHONY: build test vet docs-gen docs-build docs-serve ext-bump hooks dev-deploy

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

# Enable the committed git hooks (.githooks/) for this clone. One-time setup;
# pre-commit = identity guard + gofmt + staged secret scan, pre-push = vet +
# compat preflight. CI remains the authority.
hooks:
	git config core.hooksPath .githooks

# Local dev deploy of the daemon + native host in one atomic step, so no manual
# step (rebuild, symlink repoint, host restart) gets skipped. Installs to a
# stable path under $HOME that Homebrew never touches — pinning the native host
# there avoids the dangling-symlink disconnect you get when it resolves to a
# versioned Caskroom path that `brew upgrade` later deletes.
# Usage: make dev-deploy            (installs to ~/.local/bin/papio)
#        make dev-deploy DEV_BIN=/custom/path/papio
DEV_BIN ?= $(HOME)/.local/bin/papio
DEV_VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null | sed 's/^v//')-dev.$(shell git rev-parse --short HEAD)

dev-deploy:
	@mkdir -p $(dir $(DEV_BIN))
	go build -ldflags "-X papio/internal/api.Version=$(DEV_VERSION)" -o $(DEV_BIN).new ./cmd/papio
	@# macOS SIGKILLs an overwritten signed-binary inode; rm then mv for a fresh one.
	rm -f $(DEV_BIN)
	mv $(DEV_BIN).new $(DEV_BIN)
	-$(DEV_BIN) daemon stop
	@# Pins the host symlink to $(DEV_BIN) (this binary) — stable across brew upgrades.
	$(DEV_BIN) native-host install
	@# Drop any running host so the browser respawns it from the new binary.
	-pkill -f papio-native-host
	@# Autostarts the new daemon, runs migrations, and verifies the whole chain.
	-$(DEV_BIN) doctor
	@echo "dev-deploy: $(DEV_VERSION) -> $(DEV_BIN) (host symlink repointed)"
