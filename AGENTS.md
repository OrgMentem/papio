# AGENTS.md — papio dev notes & footguns

papio = a Go **daemon + CLI** (`cmd/papio`, `internal/*`) that acquires scholarly PDFs,
plus a MV3 **browser extension** (`extension/`) that talks to the daemon over a
**native-messaging host** (`com.orgmentem.papio`) using the `papio-browser/1`
**protocol** (Go: `internal/protocol`, TS: `extension/src/protocol.ts`, schema:
`protocol/browser-v1.schema.json`). The extension rides the user's *own*
authenticated browser session; today papio drives no separate browser.

**Read `INTENT.md` before adding a gate, refusal, review round, or human step.**
It records the owner's automation goals and limits. It takes precedence on product behavior.
Correct conflicting instructions within the task's scope. Report unrelated conflicts without expanding the task.

## Standing boundaries

- Production uses native messaging and extension APIs in the user's authenticated browser. Never use CDP or WebDriver there.
- Never overwrite an executed macOS binary with `cp`. Never edit an applied migration in place.
- Keep config, daemon, native host, and extension compatibility together. Use `make dev-deploy` only for an authorized local deploy.
- `make dev-deploy` attempts extension reload but tolerates failure. Verify the new holder session; see the reload reference below.
- A failed adoption listing never proves an empty directory. Preserve possible landing bytes; do not authorize cleanup from a failed scan.
- Live cohort runs submit real jobs. Identity reports and private cohorts name the user's papers; never publish them.
- New CLI commands enter MCP by default. Hide developer plumbing and preserve write gates and strict IPC contracts.
- Tickets belong in the rumen findings ledger, project id `papio`. Never drive `bd` directly or use GitHub Issues for agent tickets.
- ADRs live in `dev/adr/NNNN-*.md`, not `docs/adr/`. Do not create `CONTEXT.md` preemptively.
- `dev/active/` holds temporary working notes; salvage normative decisions into ADRs before deleting shipped notes.
- `dev/plans/<feature-slug>/` holds tracked specs and wayfinder maps. Do not apply the temporary-note deletion rule to these records.
- Never publish internal `dev/` material through `docs/`. Never make an ADR depend on temporary `dev/active/` content.
- Verify every `file:symbol` citation. Preserve durable field reports that code or cohorts cite.

## Entry commands

Run commands from the repository root unless the command changes directories.
Choose checks for the changed surface; the references preserve exact test contracts.

```bash
go build ./...
go test -race ./internal/config ./internal/protocol ./internal/browser
make test
go vet ./...
(cd extension && bun run typecheck && bun test && bun run build)
papio doctor
papio browser sessions
```

Go checks use `-race`. Extension bundles have zero runtime dependencies; Bun only builds them.
For documentation-only changes, check links, anchors, technical content, and task routing.
Do not run live acquisition, deployment, or browser smoke procedures for prose-only moves.
Read the relevant test reference before behavior changes; this exception does not waive its checks.

## Read before the operation

Load only the rows that match the task, including each row that a cross-surface change touches.
Paths in code spans refer to the repository root unless stated otherwise.

| Before working on | Read |
|---|---|
| Go or extension code, tests, store-per-test setup | [Build and test contracts](dev/agents/build-and-test.md) |
| Docs, generated commands, published skills, plans, ADRs or citations | [Documentation sources and lifetime](dev/agents/documentation.md) |
| Config, daemon/native-host install, migration, timestamps, terminal reasons, adoption or managed filing | [Config and daemon](dev/agents/config-and-daemon.md) |
| Identifier parsing, ownership matching or normalization | [Identifiers](dev/agents/identifiers.md) |
| PDF identity windows or thresholds | [PDF identity measurement](dev/agents/pdf-identity.md) |
| Live acquisition measurement, cohorts or interpreting bench results | [Live acquisition](dev/agents/live-acquisition.md) and `dev/live-cohort.md` |
| CLI commands, JSON output, MCP exposure or daemon IPC | [CLI and MCP](dev/agents/cli-and-mcp.md) |
| Browser wire messages, feature flags, delivery reconciliation, native transport, or extension DOM tests | [Browser protocol](dev/agents/browser-protocol.md) |
| Session selection, reload arbitration, holder generation or queued offers | [Browser sessions](dev/agents/browser-sessions.md) |
| Firefox support, native downloads, tab groups or permission differences | [Cross-browser contracts](dev/agents/cross-browser.md) |
| Extension pages, worker state, observations, smoke checks or reload | [Extension lifecycle and reload](dev/agents/extension-lifecycle.md) |
| Browser automation, detection, provider challenges or selecting a harness | [Automation boundaries](dev/agents/browser-automation.md) |
| Isolated development browser, unpacked installation, fixtures or host permissions | [Dev harness](dev/agents/dev-harness.md) and [Browser sessions](dev/agents/browser-sessions.md) |
| Adapter creation, repair, classifier rules or fixture capture | [Adapters](dev/agents/adapters.md), [Dev harness](dev/agents/dev-harness.md), and `.agents/skills/papio-provider-adapter/SKILL.md` |
| Provider routing, authentication, rate limits or job-history thresholds | [Providers](dev/agents/providers.md) |
| Work-window visibility or hidden provider rendering | [Window UX](dev/agents/window-ux.md) |
| Ledger commands, specs, maps or multi-ticket coordination | [Issue tracker](dev/agents/issue-tracker.md) |
| Domain exploration or architecture changes | [Domain docs](dev/agents/domain.md) and the relevant ADRs |
| Instruction policy or reference organization | [Policy and resolution evidence](dev/agents/instruction-policy.md) |
