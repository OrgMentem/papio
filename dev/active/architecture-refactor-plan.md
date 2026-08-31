# Architecture refactor plan

**State:** reviewed and revised.  
**Scope:** internal refactors with no intended product, wire, or config change. Slice 7 adds one store migration for crash-safe artifact publication and fixes the adoption lease race.

This plan deepens a small set of modules. It does not rewrite *papio* or split
large files by topic. Each slice must remove repeated policy or hide state that
callers currently manage.

The major seams remain correct:

- The Go daemon owns durable state and acquisition policy.
- The TypeScript extension owns browser-local facts and effects.
- The browser protocol stays strict across Go, TypeScript, and the schema.
- zotio remains the only deep Zotero integration.
- `internal/work` and `internal/ownership` keep their different identity rules.

## First review 2026-08-31

Three independent reviews checked architecture, code, and compatibility. They
found defects in four slices. This revision accepts every finding:

- Slice 2 now amends ADR-0017 before it moves cross-store orchestration.
- Slice 5 preserves pending acknowledgements, holder-independent traffic, and
  the legacy empty-session path.
- Slice 7 names the persistence owners and fences promotion with the existing
  adoption lease.
- Slice 8 checks source constructors by role instead of treating every source
  as an acquisition resolver.

The reviewers also confirmed that the plan must preserve current transport
adapters, protocol validation, and product behavior.

## Second review 2026-08-31

Four fresh reviews checked execution, architecture, verification, and product
behavior. They found nine remaining defects across four slices:

- Slice 1 now requires invalid-scope tests at both transport seams.
- Slice 3 now has an active-plan dependency and an AST cutover check.
- Slice 5 now amends ADR-0003 and states the time-based reload limit precisely.
  Its live smoke requires isolated profiles and unpacked development builds.
- Slice 7 now keeps one shared acceptance seam and uses a durable publication
  journal. The journal closes the crash and commit-failure window between
  SQLite and filesystem publication.

The second reviews grounded these corrections in current code and ADR evidence.

## Why this work exists

Four concrete locality failures now carry maintenance risk.

1. The API and browser adapters repeat triage mutations. See
   `internal/api/triage.go:triageDecide` and
   `internal/browser/bridge.go:triageDecide`.
2. The same adapters repeat document-delivery reconciliation. See
   `internal/api/delivery.go:deliveryConfirmRequestAbsent` and
   `internal/browser/bridge.go:deliveryConfirmRequestAbsent`.
3. The extension has two copies of provider-page classification. See
   `extension/src/adapters/types.ts:interpret` and
   `extension/src/plan.ts:planExecution`.
4. Browser request correlation and daemon session arbitration expose their
   implementation across many callers. See
   `extension/src/background.ts:requestNative` and
   `internal/browser/bridge.go:Sync`.

Two lower-level problems follow the same pattern:

- Resolver, enrichment, sibling, and fetch paths update
  `internal/app/app.go:retryPlan` directly.
- Adoption orchestration reaches raw persistence through
  `internal/app/browser_adopt.go:leaseAwaitingHuman` and
  `internal/app/browser_adopt.go:candidateIDByKey`.

The deletion test sets the scope. A useful change removes one implementation of
a rule or moves hidden state behind a smaller interface. A change that only
moves lines does not qualify.

## Invariants

Every slice must preserve these rules.

1. Do not add or widen an IPC method, browser frame, or result shape.
2. Do not change a job state, legal transition, terminal reason, or retry limit.
3. Do not change access-mode behavior or institutional authority.
4. Do not change provider pacing, credit admission, or gate timing.
5. Add only Slice 7's `artifact_publications` migration. Do not add another
   migration or any config field.
6. Keep routine browser failures as structured results. Keep transport failures
   fatal.
7. Keep all provider rules packaged in the extension.
8. Keep pending native requests in worker memory.
9. Keep browser session identity outside the browser protocol.
10. A pending identified session receives a pending-role `hello_ack` before
    `session_busy`.
11. Keep every currently allowed holder-independent frame available from a
    pending session.
12. Keep the legacy empty-session path last-hello-wins in both directions.
13. Preserve the different `internal/work` and `internal/ownership`
    normalization rules.
14. Do not add a generic repository, mutation framework, or plug-in system.
15. Keep each slice small enough for one direct review and one commit.

## Non-goals

This plan does not:

- merge the Go daemon and TypeScript extension;
- reduce strict protocol validation;
- centralize browser and IPC size caps;
- add daemon push;
- redesign the inbox, popup, Activity, or page-bulk surfaces;
- split the remaining `Bridge` code because of file size;
- add a general recovery coordinator;
- merge main-file adoption with component adoption;
- change source priority, defaults, or credentials;
- add compatibility aliases or deprecated paths.

## Execution order

The slices below form one clean cutover. Finish each slice before the next slice
starts. Slice 3 overlaps the classifier work in
`dev/active/institutional-signin-sharing.md`. Either land Slice 3 first and
change that plan's remaining classifier work to target `planExecution` only, or
wait until that work stops. Never edit the two classifier bodies concurrently.

Slices 4 and 5 touch active browser lifecycle work. Start them only when
`dev/active/surface-lifecycle-plan.md`,
`dev/active/entry-lease-lifecycle.md`, and
`dev/active/institutional-signin-sharing.md` no longer change the same
invariants.

### Slice 1: one triage mutation module

**Problem.** `internal/api/triage.go:triageDecide` and
`internal/browser/bridge.go:triageDecide` repeat watch-hit lookup, acquire,
dismiss, retraction acknowledgement, conflict handling, and dismiss-scope
selection.

**Change.**

1. Add one mutation entry point to `internal/triage.Service`.
2. Move watch-hit and retraction decision policy into that entry point.
3. Keep raw JSON shape, bounds, and strict decoding at the IPC and browser
   protocol seams.
4. Both seams must reject a missing, null, empty, duplicate, nonpositive, or
   over-100 watch scope, plus trailing JSON.
5. Move `all` expansion and validation against the hit's current watch IDs into
   the triage module after normalized transport decoding.
6. Keep browser-only PDF-grab dismissal in the grab path.
7. Keep API error mapping in `internal/api`.
8. Keep browser `outcome` and `detail` frame mapping in `internal/browser`.
9. Delete the duplicated decision branches and semantic scope helpers.

**Interface rule.** The triage module accepts a normalized item, operation, and
scope. It returns a typed domain outcome. It does not import IPC or browser
protocol types.

**Acceptance.**

- API and browser callers leave the same durable watch or retraction state.
- Both callers retain their current error and result shapes.
- A vanished digest remains a conflict.
- A repeated retraction dismissal remains `already_applied`.
- PDF-grab dismissal remains browser-only.
- Invalid scope inputs keep their current adapter-specific error mapping and
  change no durable state.
- No adapter contains watch or retraction decision branches.

**Verification.**

```sh
go test -race ./internal/triage ./internal/api ./internal/browser
```

Keep the existing API and browser tests. Add one table-driven matrix at both
transport seams. It must cover missing, null, empty, duplicate, nonpositive,
over-100, and trailing-JSON scopes. Each case must assert the current
adapter-specific error mapping and unchanged watch or retraction state. Move
shared valid-mutation assertions to the triage module where this removes
duplicate setup.

### Slice 2: one document-delivery reconciliation module

**Problem.** `internal/api/delivery.go:deliveryConfirmRequestExists` and
`internal/browser/bridge.go:deliveryConfirmRequestExists` repeat one
transaction. Their `deliveryConfirmRequestAbsent` siblings repeat a second,
order-sensitive operation.

The absent path must close the old action through
`internal/job/job.go:RepairAwaitingHuman` before it calls
`internal/app/app.go:SubmitDelivery`. The reverse order attempts an illegal
`awaiting_human -> awaiting_human` transition.

**Change.**

1. Amend ADR-0017 before code moves. Record this ownership split:
   `internal/delivery` owns request rows and provider reconciliation,
   `internal/job` owns job and human-action state, and `internal/app` owns the
   cross-store Decision 4 operator operation.
2. Add one reconciliation operation to `internal/app.Service`.
3. Keep `internal/delivery.Service` focused on `delivery_requests` persistence
   and provider reconciliation. It must not become a second job-state owner.
4. Move open-action lookup, request-row mutation, job transitions, and failure
   compensation into the app operation.
5. Keep the existing single-transaction path for
   `confirm_request_exists`.
6. Keep the exact `cancel -> repair -> submit` order for
   `confirm_request_absent`.
7. Return domain outcomes and typed errors to both transport adapters.
8. Keep API detail rendering in `internal/api`.
9. Keep browser result-frame rendering in `internal/browser`.
10. Delete both adapter implementations after every caller moves.

**Interface rule.** The app module owns the cross-store operation because it
already owns job and delivery routing. The ADR amendment records this locality
change. It changes no wire or product behavior.

**Acceptance.**

- Both caller paths produce the same final job, request, and action state.
- `confirm_request_exists` changes the row and job atomically.
- `confirm_request_absent` never submits while the job remains parked.
- A failed submit restores an operator-visible reconciliation path.
- Concurrent repeats remain idempotent or return the current conflict result.
- The browser adapter returns structured failures instead of raw Go errors.

**Verification.**

```sh
go test -race ./internal/delivery ./internal/app ./internal/api ./internal/browser
```

The tests must keep the discriminating end state. They must check the job state,
one resolved action, one open action after re-park, and reuse of the existing
request row.

### Slice 3: one extension classifier

**Problem.** `extension/src/adapters/types.ts:interpret` and
`extension/src/plan.ts:planExecution` repeat rule order, `all`, `any`,
`textAny`, `deferUntilDeadline`, title-token checks, evidence labels, and the
unknown fallback.

Production injects `planExecution`. Most provider fixture tests call
`interpret`. A one-sided change can test one implementation and run the other.

**Change.**

1. Make `planExecution` the only classifier implementation.
2. Change every provider fixture test to read the verdict from the production
   planning path.
3. Keep a test helper only when it projects a `Plan` verdict. The helper must
   contain no classification policy.
4. Delete `interpret`, `AdapterContext`, and unused exports after all callers
   move.
5. Keep `PageVerdict` if planning and diagnostics still use it.
6. Update stale comments in `extension/tools/adapter-try.ts`.
7. Keep the per-rule diagnostic table independent. It reports every rule and
   does not authorize an effect.
8. Keep `planExecution` self-contained for `chrome.scripting.executeScript`.

**Acceptance.**

- Every committed provider fixture tests `planExecution`.
- The adapter tool and the browser use the same planning implementation.
- Deferred rules cannot wake early.
- A non-article rule gets the existing settle window.
- An article marker must remain through the full deadline.
- Work evidence and effect binding remain fail-closed.
- No second classifier body remains in the tree.

**Verification.**

```sh
cd extension
bun run typecheck
bun test
bun run build
bun run adapter:try -- fixtures/tandfonline/success.html --id tandfonline --expect article
bun run adapter:try -- fixtures/tandfonline/no-entitlement.html --id tandfonline --expect no_entitlement
```

Before merge, run an AST-aware search across `extension/src`,
`extension/test`, and `extension/tools`. It must find no `interpret`
declaration, import, or call. It must also confirm that fixture verdicts come
from `planExecution`. This is a cutover check, not a source-text unit test.

### Slice 4: a deep native-request correlation module

**Problem.** `extension/src/background.ts:requestNative`,
`extension/src/background.ts:sendCorrelated`,
`extension/src/background.ts:resolveNativeResponse`, and
`extension/src/background.ts:resolveNativeError` spread request identity,
expected reply, timeout, retry, and late-response rules across `Bridge`.

Many workflows must supply policy flags. Tests cast the private `Bridge`
interface to reach correlation behavior.

**Change.**

1. Add one focused TypeScript module for correlated native requests.
2. Move request ID creation, validation, pending state, timeout, expected-reply
   matching, and fail-all behavior into it.
3. Replace free combinations of expected type, feature, mutation, and retry
   flags with one closed request-policy table.
4. Let callers provide only workflow data that the policy table cannot derive.
5. Keep `Bridge.ensureConnected` and feature negotiation in `Bridge`.
6. Keep `enqueueInbound` in `Bridge`. It serializes all inbound frames, not only
   replies.
7. Let the correlation module consume matched response and error frames.
8. Return unmatched errors to `Bridge` for hello and page-acquire handling.
9. Keep all pending requests worker-local. A restart fails them.
10. Delete the old maps and helpers after every caller moves.

**Depth gate.** Stop if the new interface still exposes the current collection
of independent flags. That change would only move code.

**Acceptance.**

- Every correlated workflow uses the module.
- A duplicate supplied request ID fails before send.
- A late or mismatched response changes no state.
- Read requests keep their current transport retry behavior.
- Mutations do not gain an unsafe retry.
- Disconnect fails each pending request once.
- Tests no longer cast `Bridge` to call private correlation methods.

**Verification.**

```sh
cd extension
bun run typecheck
bun test test/background_correlation.test.ts test/background.test.ts
bun run build
```

### Slice 5: a private browser-session arbitration module

**Problem.** `internal/browser/bridge.go:Sync`,
`internal/browser/bridge.go:Sessions`, `internal/browser/bridge.go:Claim`,
`internal/browser/bridge.go:promote`, and `internal/browser/bridge.go:release`
share holder, pending, stale-takeover, demotion, generation, and reload state.
`Sync` also owns frame handling and offer scheduling.

**Change.**

1. Amend ADR-0003 before implementation. Record the role-bearing pending
   acknowledgement and the holder-independent admission principle. Keep the
   exact frame list in code and tests.
2. Add a private session-arbitration module inside `internal/browser`.
3. Move holder and pending records, counters, epoch changes, stale takeover,
   demotion notices, and development reload reservations into it.
4. Keep session IDs on the native-host IPC envelope.
5. Preserve the legacy empty-session path and its mixed
   legacy/identified last-hello-wins behavior.
6. Keep hello, pending-role acknowledgement, and busy frame encoding in
   `Bridge`.
7. Keep the current holder-independent frame admission rules in `Bridge`.
8. Keep offer, cancel, handoff, and materialization maps in `Bridge`.
9. Keep `Sync` responsible for frame order and transport-fatal validation.
10. Make `Sync`, `Sessions`, `Claim`, and reload handling ask the arbitration
    module for session decisions.
11. Recheck holder identity and epoch after `Sync` releases the bridge mutex
    for scheduler work.
12. Delete the old holder and pending state from `Bridge` after the cutover.

**Depth gate.** The module owns a state machine. It must not become a set of
getters around fields that `Bridge` still mutates.

**Acceptance.**

- The first identified hello wins.
- A pending identified hello receives a pending-role `hello_ack`, then
  `session_busy`, and receives no automatic offer traffic.
- A pending session can send every currently allowed holder-independent frame
  and receives the existing typed result.
- An empty session ID keeps last-hello-wins behavior against identified and
  legacy holders.
- An explicit claim demotes the old holder once.
- A stale holder yields to a live pending session.
- During a development reload reservation, pending polls cannot claim the
  vacant slot.
- The first new hello can claim that time-based reservation. A session ID
  alone does not prove that the same browser returned.
- `papio browser reload` reports success only when observation attributes the
  new holder. It fails on ambiguity or sibling theft.
- An ordinary disconnect still promotes a sibling immediately.
- Holder generation still fences work resumed after the mutex release.

**Verification.**

```sh
go test -race ./internal/browser ./internal/api ./internal/nativehost
```

Run one isolated live smoke with two browser sessions.

Preconditions:

1. Use a disposable `PAPIO_CONFIG_DIR`, data directory, socket, download root,
   and two dedicated browser profiles.
2. Install the matching native-host manifest in both profiles.
3. Load the unpacked development extension in both profiles. Confirm
   `chrome.management.getSelf()` reports `installType === "development"`.
4. Do not connect the operator's normal daemon or browser profiles.

Then:

1. Confirm that the first session holds the bridge.
2. Confirm that the second session receives a pending acknowledgement and
   remains pending.
3. Send a read-only `stats_request` from the pending browser. Confirm its typed
   result. Do not use `page_acquire` or another mutating request.
4. Run `papio browser use <second-id>`.
5. Confirm that the old holder shows demotion.
6. Send the same `stats_request` from the demoted browser. Confirm its typed
   result.
7. Run `papio browser reload`.
8. Confirm a new session ID and the command's honest success or attribution
   failure. Do not infer browser identity from the new ID.

### Slice 6: hide retry-decision policy

**Problem.** Resolution, enrichment, sibling, and fetch paths write fields on
`internal/app/app.go:retryPlan`. Later code in
`internal/app/app.go:parkForRetry` and
`internal/app/app.go:retryCutoverDecision` interprets those fields.

The callers must know which waits spend the retry budget, which gate time wins,
and when a source counts as called.

**Change.**

1. Move `retryPlan` and its policy into a dedicated file inside `internal/app`.
2. Replace direct field updates with typed observation operations.
3. Make the module own time precedence, chargeability, advisory throttles,
   source gates, budget refusals, and cutover classification.
4. Keep durable event reads and job transitions in `app.Service`.
5. Keep every retry limit, delay, gate time, and terminal reason unchanged.
6. Do not create a new package unless the final interface hides more policy
   than the current fields.
7. Delete obsolete field-level helpers after every caller moves.

**Acceptance.**

- A pure source gate spends no retry attempt.
- A pass that reached a source counts as temporary when it also saw a gate.
- Advisory local throttling stays distinct from a provider refusal.
- Scheduling uses the earliest temporary time and the latest pending gate where
  the current rules require them.
- Retry-budget exhaustion keeps one final pending-gate wait.
- Institutional cutover reads the same current-pass facts as retry scheduling.
- No caller edits retry counters or gate times directly.

**Verification.**

```sh
go test -race ./internal/app ./internal/job
```

Run the existing temporary failure, source gate, mixed pass, sibling failure,
and budget exhaustion tests. Do not tune a threshold during this slice.

### Slice 7: put adoption persistence behind store operations

**Problem.** `internal/app/browser_adopt.go:leaseAwaitingHuman`,
`internal/app/browser_adopt.go:candidateIDByKey`, and parts of
`internal/app/component_adopt.go:AdoptComponent` reach database details from the
app orchestration layer.

The shared `internal/app/app.go:validateCandidate` path writes artifact metadata,
calls `internal/artifact/artifact.go:Promote`, then accepts the candidate and
transitions the job. A crash or ambiguous commit after promotion can leave
published bytes without a completed acquisition edge. A SQLite rollback cannot
undo a hard link or rename.

**Change.**

1. Record a focused ADR before implementation. Define the two-resource
   publication protocol, durable ownership, recovery, and cleanup rules.
2. Add one narrow `artifact_publications` migration. A prepared row binds a job,
   candidate or component role, digest, and optional adoption lease owner before
   bytes become visible.
3. Make `internal/job.Store` own the publication journal, job lease, candidate
   lookup, artifact metadata, acquisition edge, and final job transition.
4. Keep filesystem quarantine, hashing, and promotion in
   `internal/artifact.Store`.
5. Keep `validateCandidate` as the one main-artifact validation and acceptance
   seam. Pass an optional adoption lease owner into it. Resolver acceptance
   supplies no lease and keeps current behavior.
6. Keep component adoption on the same publication protocol, but preserve its
   no-job-state-transition rule.
7. Prepare publication in one committed store transaction. Check the optional
   lease, persist artifact metadata, and persist the journal row before
   filesystem publication.
8. Add one narrow store operation for final publication. It opens a write
   transaction, rechecks the journal and optional lease, and invokes exactly one
   precomputed promotion callback while the transaction prevents lease
   replacement.
9. After promotion, the same transaction marks the candidate, writes the
   acquisition edge, performs the legal job transition when applicable, and
   removes the journal row.
10. Make `Promote` report whether it created or reused the digest path. Never
    delete reused content.
11. Treat a commit error after promotion as ambiguous. Read the durable
    acquisition edge and journal on a new connection. Return success only when
    the edge committed. Otherwise, retry recovery from the prepared journal.
12. Reconcile prepared publications before the affected job resumes ordinary
    work. A matching file is re-verified and finalized. A surviving quarantine
    file is promoted again through the idempotent path.
13. If neither file survives, remove the journal and artifact metadata only
    after one store transaction proves that no acquisition edge or other
    publication intent references the digest.
14. Keep the adoption lease alive through preparation. A failed renewal stops
    the original process before it enters final publication.
15. Keep path confinement, PDF validation, identity decisions, and acceptance
    order in the app module.
16. Keep validation evidence bound to the job and candidate.
17. Remove direct database access from the adoption paths after the cutover.

The write transaction is the lease fence during the bounded link or rename.
The committed journal is the durable owner during a crash. A pre-promotion
lease check without both mechanisms is not sufficient.

**Acceptance.**

- Every visible artifact file has either a prepared publication row or a
  committed acquisition edge.
- Every finalized acquisition edge resolves to a hash-verified file.
- A crash after preparation but before promotion is recoverable.
- A crash or commit failure after promotion cannot lose the publication owner.
- An ambiguous commit is resolved by durable reads, never by assuming rollback.
- A process that loses the adoption lease cannot enter final publication.
- The lease cannot change owners during the bounded promotion callback.
- A lease-renewal failure before final publication creates no accepted
  candidate.
- A durable publication intent binds job, candidate or role, and digest before
  promotion.
- Shared or reused content is never removed during compensation.
- Main resolver and browser adoption use one `validateCandidate` acceptance
  seam.
- Resolver acceptance keeps its current state and retry behavior.
- Main adoption resolves only the actions that current behavior resolves.
- Component adoption leaves the main artifact and job state unchanged.
- `internal/job.Store` owns the journal, lease, candidate, artifact metadata,
  and acquisition-edge persistence.
- The app adoption paths contain no SQL and do not reach `Jobs.S.DB()`.

**Verification.**

```sh
go test -race ./internal/app ./internal/job ./internal/store ./internal/artifact
```

Add fault tests for each publication window:

1. Stop after the prepared journal commits but before promotion.
2. Stop after promotion but before the final SQL commit.
3. Force an ambiguous final commit result, then recover on a new connection.
4. Reuse a digest that another job already owns.
5. Replace the adoption lease before final publication.
6. Pause inside promotion and run stale recovery. Recovery must block until the
   transaction finishes and must not replace the owner.

Restart the store in the first three tests. Prove that recovery reaches one
legal final state, publishes at most one digest path, and preserves shared
content. Also cover resolver acceptance without a lease, browser adoption with
a lease, and component adoption without a job transition.

The migration must bump every latest `user_version` assertion named in
`AGENTS.md`. Keep the schema-33 historical fixtures unchanged. Seed
store-per-test packages with `storetest.DataDir(t)`.

Smoke one browser adoption and one component adoption through the actual CLI or
daemon path. Use disposable config and data directories. Check the final job,
candidate, artifact, journal, acquisition-edge, and action records.

### Slice 8: reduce the source-registration edit surface

**Problem.** Source names and defaults live in
`internal/config/config.go:SourceNames` and
`internal/config/config.go:defaultSources`. Constructors live in
`internal/bootstrap/bootstrap.go:resolverEntries`. The benchmark has another
constructor list in `internal/bench/runner.go:resolverEntries`.

**Change.**

1. Keep one config catalog for implemented names, stable display order, shipped
   defaults, and declared source roles.
2. Let a source declare one or more roles: acquisition resolver, discovery
   backend, metadata enricher, or retraction source.
3. Derive validation and error-list text from that catalog.
4. Keep every production constructor explicit in its current bootstrap owner.
5. Compare each catalog role with its matching constructor set.
6. Compare acquisition resolvers with
   `internal/bootstrap/bootstrap.go:resolverEntries`.
7. Check discovery backends against the explicit discovery switch.
8. Check `crossref_metadata` against enrichment wiring and
   `retraction_watch` against sentinel wiring.
9. Reuse the production acquisition order in the benchmark where its injected
   adapters have the same meaning.
10. Keep acquisition and discovery enablement separate.
11. Keep OpenAlex credit and OpenAIRE credential-tier policy explicit.
12. Do not add reflection, dynamic loading, or provider registration from
    config.

**Acceptance.**

- Adding a source requires one catalog row and one constructor for each declared
  role.
- A catalog role without matching production wiring fails a test.
- Production wiring without the matching catalog role fails a test.
- Metadata and retraction sources never enter the acquisition resolver chain.
- Unknown config names still fail closed.
- Removed shipped names remain accepted and dropped through the existing
  compatibility path.
- Default source values, order, and roles do not change.

**Verification.**

```sh
go test -race ./internal/config ./internal/bootstrap ./internal/bench
```

## Final cleanup

After all slices:

1. Change `internal/api/handler.go:getJob` to use
   `internal/job/job.go:ListHumanActionsForJob`.
2. Remove obsolete helpers, comments, imports, and test-only seams.
3. Run formatters only for touched code.
4. Run the full repository checks once.
5. Do not add a changelog entry unless a slice intentionally changes visible
   behavior after separate approval.
6. Amend ADR-0017 in Slice 2.
7. Amend ADR-0003 in Slice 5.
8. Add the artifact-publication ADR and its public summary in Slice 7.
9. Do not add another ADR unless implementation requires a decision that this
   plan currently forbids.

Final checks:

```sh
go build ./...
make test
go vet ./...

cd extension
bun run typecheck
bun test
bun run build
```

## Stop conditions

Stop a slice and return to design when any condition occurs:

- The change needs a new wire field, frame, method, or compatibility alias.
- The change changes a job state or transition order.
- The change needs an unplanned migration, config field, or second schema
  change.
- The new module exposes as much policy as the old callers.
- The change adds callbacks only to move lines out of a large file.
- A current active plan changes the same authority or lifecycle rule.
- A test passes only after weakening a fail-closed check.
- A live smoke needs an automated browser or copied authentication state.

## Completion and retirement

This plan completes when every accepted slice ships with its checks, or when a
review records why a slice fails its depth gate. Any new lasting decision moves
to `dev/adr/`. Then delete this file. Git history is the archive.
