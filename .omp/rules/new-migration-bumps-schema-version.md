---
name: new-migration-bumps-schema-version
description: "A new store migration bumps user_version — four test files pin the latest number, and two historical fixtures must NOT be bumped"
condition: ['.*']
scope: ["tool:write(internal/store/migrations/*.sql)"]
interruptMode: never
---

You are adding a migration, so `user_version` moves. **`go test ./...` fails until four assertions are bumped**, and two nearby fixtures must be left alone — that asymmetry is where this goes wrong.

Confirm the new latest number first (`ls internal/store/migrations | tail -1`), then bump:

- `internal/cli/clean_install_test.go` — the string `"schema version N"` **twice**, plus a `user_version` compare
- `internal/doctor/doctor_test.go` — `"schema version N"`
- `internal/store/migrate_forward_test.go` — both post-migration `user_version` compares
- `internal/store/migrate_guard_test.go` — `TestOpenRefusesSchemaNewerThanBinary`, the easiest to miss because it is the only site naming *two* numbers: it sets `user_version` to latest+1 and asserts the exact string `"database schema version <N+1> is newer than this binary supports (<N>); refusing to open"`

**Leave these alone:** that file's sibling `TestGuardCapableSchema33RefusesSchema34` deliberately models a historical ceiling, and `migrate_forward_test.go`'s *fixture* pins schema 33 on purpose — it selects migrations by `migrationNumber(filename) > 33` rather than a hardcoded prefix list, so the bound stays 33 and a new migration needs no entry there.

Also: never go back and edit this file once it has been applied anywhere (`rule://applied-migration-immutable`) — the correction has to be yet another migration.
