# Surface lifecycle: the remainder

The surface-lifecycle mechanism shipped. Its normative content now lives in
`dev/adr/0028-surface-lifecycle-ownership.md`, and the 1,455-line plan that
carried it is deleted; `git log -- dev/active/surface-lifecycle-plan.md` is the
archive.

This file holds only what is still open, verified against the tree on
2026-09-03. It is a short list on purpose. Do not grow it into a ninth round of
that plan; a stale nine-round document was itself a blocker twice.

## Open items

### 1. Terminal jobs leave their candidate rows behind

`internal/browser/bridge.go:10143` and `:10153` sweep close authorizations and
terminal entry leases, and `internal/job/institutional_materialization.go:1739`
(`AbandonStaleMaterializations`, called at `internal/browser/bridge.go:992`)
fences stale claims and restores eligible candidates. Neither reaps a
`browser_candidates` row whose job has reached a terminal state, so a dead job
keeps a candidate row that eligibility code must keep stepping over
(`internal/browser/bridge.go:1739`).

Size: multi-day. Needs a decision on whether the reaper runs in the poll sweep
or in the terminal transition itself.

### 2. No durable "papio is working on it" disposition

`extension/src/state.ts:13` holds the whole durable job-status vocabulary
(`JobStatus`), and it has no waiting-for-papio member. An internal
authentication stall therefore reuses `auth_pending`, which means "a human must
sign in", and `signInBlockerCount` (`extension/src/background.ts:3855`) counts
it in the blocker total. The operator sees a sign-in request for work that
needs no human.

Size: multi-day, and it needs a product decision on how the inbox and the popup
render the new state before any code lands.

### 3. Superseded attempts have no claim teardown contract

The attempt fence at `internal/browser/bridge.go:1746`
(`currentMaterializationEligibility`) stops a superseded attempt's candidate
from being treated as eligible. It does not retire the old claim, and no
attempt-specific close or retire operation exists. A superseded claim can
remain until an ordinary sweep or lease expiry removes it, so its surface can
outlive the attempt that justified it.

Size: unclear until the intended contract is written down. Start by deciding
whether teardown is a daemon transition or an extension close transaction.

### 4. A challenge observation after lease promotion is rejected

`internal/job/claim_observation_apply.go:200` handles `challenge` in the same
branch as `wall_observed`, and line 201 requires a reserved lease owned by the
same job. After the lease is promoted past `reserved`, a challenge report is
refused with "no live reserved entry for this owner". The dead end is defended
twice, so nothing loops; the report is simply lost.

Size: one sitting, plus an amendment sentence in ADR-0028 recording whichever
disposition is chosen. Low priority.

### 5. The reoffer throttle is unmeasured

`maxInstitutionalReoffers` is four (`internal/browser/bridge.go:159`), pinned by
`internal/browser/bridge_test.go:3427`. Nobody measured the number. Per
`AGENTS.md`, plot the distribution before tuning a threshold.

Size: one sitting of measurement against the live store.

### 6. Manual-download route classification is undecided

`internal/app/action_guidance.go:36` gives a `manual_download` action its next
step through `HumanActionNextStepFor` (`:29`). It is still undecided whether
such an action should carry an institutional route or hand off through the
openurl handler.
This is the same question the entry-lease work raises as "which path owns a
paper", and it is deliberately not taken here.

Size: product decision first, then small.

## The field-evidence gate

Two behaviours have suite coverage but no observed live instance:

- a restart-recovered claim observation actually applied on the operator's
  machine, covered by `internal/browser/claim_observation_activity_test.go:39`
  and `internal/browser/authentication_claim_test.go:821`;
- causal operator cession in the terminal foreground case; the tokens exist at
  `extension/src/background.ts:4374`.

Neither is a coding task. Both close by reading the store after ordinary
institutional traffic: `papio jobs show <id>` for the daemon side, and the
`materialization_claims` and `claim_observation_journal` tables for the durable
side.
