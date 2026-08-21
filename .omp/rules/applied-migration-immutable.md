---
name: applied-migration-immutable
description: "Never edit a shipped store migration in place — constraint/column changes need a NEW internal/store/migrations file"
condition: ['.*']
scope: ["tool:edit(internal/store/migrations/*.sql)"]
interruptMode: always
---

**Stop. A file under `internal/store/migrations/` has already been applied to real databases — including the user's dev box. Editing it in place is silent, total, and invisible to the entire test suite.**

The incident this rule exists for: `0025_pdf_grabs.sql` shipped without `'abandoned'` in its `state` CHECK and was later edited to add it (`28fbc97` → `70055e7`). Every database migrated in between kept the *original* constraint, no schema version records the difference, and every test migrates a fresh database and therefore sees the *corrected* one. Result: no grab abandonment could be written on the dev box at all — the CHECK violation surfaced as `outcome: "conflict"`, so a capture stuck in `awaiting_file` forever, its tab answered `existing` for good (allocation is idempotent per host and title), and even `AbandonStaleAwaiting` could not retire it. It cost most of a session to find, because the daemon, the extension and every suite were all correct.

**Do this instead:** add a new numbered migration. `0038_pdf_grabs_abandoned_state.sql` repaired the case above by rebuilding the table. A constraint or column change needs a new migration **even when the old one is "obviously wrong"** — especially then, because "obviously wrong" is what makes editing it feel safe.

A new migration bumps `user_version`, and four test files pin the latest number (`internal/cli/clean_install_test.go` twice plus a compare, `internal/doctor/doctor_test.go`, `internal/store/migrate_forward_test.go`'s two post-migration compares, and `internal/store/migrate_guard_test.go`'s `TestOpenRefusesSchemaNewerThanBinary`, which names latest+1 and the exact refusal string). Leave the deliberate historical fixtures alone: that file's sibling `TestGuardCapableSchema33RefusesSchema34` and `migrate_forward_test.go`'s schema-33 fixture model a past ceiling on purpose.

The only edit to an applied migration that is not this bug is a pure comment/whitespace change that cannot alter the SQL executed. If that is what you are doing, say so explicitly and continue.
