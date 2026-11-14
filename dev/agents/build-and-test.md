# Build and test contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

```
# Go
go build ./...
go test -race ./internal/config ./internal/protocol ./internal/browser   # scope to what you touch
make test              # -race, like CI; a suite green WITHOUT -race proves less than it looks
go vet ./...
make identity-corpus   # measure the PDF identity rules against your Zotero library — reads your own library and its output names your own papers, don't paste it anywhere
make live-cohort       # measure UNATTENDED acquisition against real works through the running daemon — submits REAL jobs; see dev/live-cohort.md

# Extension (bun)
cd extension
bun run typecheck      # tsc --noEmit
bun test               # bun's test runner
bun run build          # emits Chrome dist/ AND Firefox firefox/ (both targets)
bun run dev            # web-ext hot-reload loop into Firefox Developer Edition

# Docs (Python — neither brew nor bun can install the SSG)
uv tool install "zensical==$(sed -n 's/^zensical==//p' docs/requirements.txt)"   # once; that file is the pin, and Renovate bumps it
zensical build                     # what CI runs; "No issues found" = clean
zensical serve -a localhost:8042   # local preview; :8000 is often already taken
go run ./cmd/docs-gen              # regenerates docs/reference/* — never hand-edit those
```

The extension has **zero runtime deps** — both bundles are plain browser JS. `bun` is a build tool only.

Test-suite notes, both of which cost a red CI build to learn: **CI runs `-race` and
local runs must too** (`make test`), because a race the detector sees is not visible
any other way — a double `cmd.Wait()` on one `*exec.Cmd` and a `ProcessState` read
racing the reaper that writes it both shipped green locally. And **a store-per-test
package must seed its data dir from `internal/store/storetest`** (`storetest.DataDir(t)`
where the test would pass `t.TempDir()`), not run the migrations again: under `-race`
a full migration run measures ~0.6s against ~0.045s to open an already-migrated file,
and `internal/browser` (285 tests) reached 471s that way, then crossed `go test`'s
**10-minute per-package timeout** and failed the build. That cap is per package and
silent until it is hit; `internal/app` was next at 422s. The template is built once
per process by the real migrations, so schema stays byte-identical — including the
`user_version` the three pinned assertions check.

