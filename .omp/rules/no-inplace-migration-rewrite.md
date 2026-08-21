---
name: no-inplace-migration-rewrite
description: "Shell rewrite (sed -i, redirect, mv/rm) targeting internal/store/migrations/NNNN_*.sql — applied migrations are immutable"
condition:
  - '\b(sed|perl|awk|tee|truncate|mv|rm|cp|install)\b[^\n]*migrations/[0-9]{4}_[a-z0-9_]*\.sql'
  - '>[^\n]*migrations/[0-9]{4}_[a-z0-9_]*\.sql'
scope: ["tool:bash"]
interruptMode: always
---

**This command mutates a migration file that has already been applied to real databases.** The same footgun as `rule://applied-migration-immutable`, reached through a shell rewrite instead of an editor.

A migration already run against the user's dev box cannot be corrected by changing its text: the deployed database keeps the constraint it was created with, no schema version records the difference, and every test migrates a fresh database and so sees only the corrected file. The suite stays green while the dev box is broken. `0025_pdf_grabs.sql`'s missing `'abandoned'` state cost most of a session exactly this way.

Add a **new** numbered migration instead (`0038_pdf_grabs_abandoned_state.sql` is the worked example — it rebuilt the table), and bump the four test files that pin the latest `user_version`.

Reading a migration is fine — this rule is about writes. If the command is genuinely read-only, or it creates a *new* migration file, say so and continue.
