# ADR-0026: Institution-session awareness after a publisher landing

Status: **Accepted** (2026-08-28)


## Context

A researcher can enter institutional sign-in from a publisher's own **Get
access** path. papio can then keep an older signed-out verdict because its
session classifier reads only configured library resolver pages.

The classifier boundary is correct. A publisher page can be open access and
cannot prove institutional entitlement. An untracked publisher landing is
therefore only a reason to check the configured resolver. A tracked papio job
return retains its existing, same-origin release authority, but neither path
sets the popup's signed-in or signed-out verdict.

The current extension already has two related paths:

- `extension/src/background.ts:recordInstitutionalSession` handles a completed
  landing in a papio-owned job tab. It records job-scoped evidence and can
  release work, but it requests a resolver probe only when no recent release
  evidence exists. That condition can leave the popup verdict stale.
- `extension/src/keepalive.ts:noteResolverNavigation` observes configured
  resolver tabs. It correctly rejects every other origin from the institution
  state set.

Commit `4b0c506` connected browser permission changes to the keepalive manager.
That link remains useful, but its first form probes every new exact grant. This
ADR narrows it to parked demand and makes effective wildcard grants correct.

Four independent reviews rejected the earlier draft. The rejected draft had
five material faults:

1. Any HTTPS tab could trigger a check while any `requires_auth` job existed.
2. Queued work was treated as an operator-facing sign-in block.
3. A worker-local offer URL was the only reliable resolver binding.
4. Exact-origin grant comparison misread wildcard grants.
5. The proposed resolver blocker had no state or action that could implement it.

This decision fixes those faults before implementation.

## Decision 1: only a correlated, settled publisher landing triggers

`extension/src/background.ts:onTabUpdated` owns admission because it has the
current job store and provider metadata. The keepalive manager receives only an
already-selected configured resolver origin.

An untracked tab triggers a check only when all conditions hold:

1. The event is a completed top-frame navigation.
2. At least one active job has `status === "auth_pending"`.
3. The landing hostname matches that job's declared `provider_hosts`.
4. The matching jobs resolve to exactly one configured institution origin.
5. The selected origin remains in the current `hello_ack.resolver_origins` set.

The extension refuses unmatched hosts, queued-only work, missing origins, and
multiple matching institution origins. It does not guess.

The demand check runs before URL parsing. With no `auth_pending` job, the new
path does not inspect the landing URL and causes no state write or probe.

A tracked job keeps `recordInstitutionalSession` as its owner. That path uses
its existing job-scoped return evidence to release same-origin work only while
the browser still holds the resolver grant. It also requests a resolver check
on every accepted return so the popup verdict can update. The new untracked
path records no release evidence and does not run for a tracked tab.

A same-document publisher change can emit no completion event. This decision
does not invent ambient page observation to catch it. The existing keepalive
cycle remains the bounded fallback. The user promise is **papio rechecks after a
correlated host landing, and continues its normal keepalive cycle otherwise**.

## Decision 2: persist only the configured institution binding

`extension/src/state.ts:ActiveJob` gains an optional `institution_origin`.

The value has these rules:

- It is a normalized bare HTTPS origin.
- It must match the current daemon-supplied resolver origin set.
- It is derived from the job offer or its declared provider hosts.
- It never contains the publisher landing host, path, query, fragment, title,
  identity-provider address, or permission pattern.

The field survives an MV3 worker restart. It lets an `auth_pending` job retain
its institution binding after worker-local offer maps disappear.

The managed browser state version advances for this field. Old rows without it
remain valid and fail closed until a daemon re-offer supplies the binding.

No origin from tab traffic, permission traffic, or stored legacy data can widen
the configured institution set. This preserves ADR-0013's closed-origin rule.

## Decision 3: the keepalive manager owns a durable pending check

`extension/src/keepalive.ts:KeepaliveManager` gains one entry point for an
already-correlated institution origin.

The entry point:

1. verifies configured membership;
2. verifies current `auth_pending` demand for an untracked landing, or accepts
   the tracked job return that just completed that demand;
3. records the pending recheck reason for that origin;
4. persists that reason before returning; and
5. requests a probe with the dedicated `institutional_landing` reason.

The probe still uses the existing per-origin 10-second start floor, generation
fence, bounded tab scan, and single commit path. It never creates or surfaces a
tab.

The pending reason contains only `parked_demand` or `tracked_auth_return`
beside the configured origin already present in the snapshot. It survives
worker sleep. `onWake` resumes a parked-demand check only while matching
`auth_pending` demand still exists. A tracked auth return remains pending after
its job advances, because updating the popup verdict is the work it represents.
If parked demand ended, `onWake` clears that reason without probing.

A completed probe clears its pending reason for that generation, including an
inconclusive result. Resolver-navigation dirtiness and reauthentication pauses
keep their existing wake behavior.

The earlier draft proposed one global cooldown. This ADR rejects that addition.
After provider and institution correlation, checks for two different
institutions represent two different blocked sessions. A global lock would let
one institution suppress the other. The existing per-origin floor supplies the
correct bound.

## Decision 4: effective permission state is distinct from a probe result

A failed scan cannot identify a missing host grant. The same result can come
from a closed tab, a privileged page, an injection failure, or a permission
failure.

The keepalive manager derives a separate ephemeral permission state for each
configured resolver:

- `granted`: a successful browser permission read shows effective coverage;
- `required`: a successful read shows no effective coverage; or
- `unknown`: the permission read failed.

Coverage matches exact origins, wildcard-host patterns, and an all-HTTPS grant.
The manager never compares a configured exact origin directly with a wildcard
pattern.

`required` means **access must be granted**. It does not claim that the
researcher declined a prompt. Browser APIs do not distinguish a decline from a
permission that was never requested.

Permission state is not a session verdict and is not persisted in the origin
snapshot. It cannot write `authenticated`, `verdict`, or session evidence.

A revocation immediately removes browser-local release authority for that
origin and makes the card show the grant requirement. It does not assert that
the institution signed the researcher out.

A new grant requests a resolver probe only when matching `auth_pending` demand
exists. A grant with no parked demand updates permission state and performs no
page read. A grant before `hello_ack` is reconsidered when configured membership
becomes authoritative.

No new browser permission, native message, daemon field, or daemon store row is
required.

## Decision 5: the session card owns the remedy

`extension/src/popup.ts:deriveSessionCardState` gives `required` permission
state precedence over session verdict copy.

The card says that library access is required. Its action requests only that
row's exact configured resolver origin. The request runs directly inside the
button click so both browsers retain the required user gesture.

The session card does not reuse provider-host blocker state. Provider blocker
fields drive provider reclassification and are a different state class.

The separate resolver-grants section can remain as setup help. The session card
action is narrower because it requests only the blocked institution shown in
that row.

The existing toolbar badge remains unchanged. A session check does not create a
count or a desktop notification.

## Decision 6: the latest completed check controls presentation

An inconclusive probe preserves the stored verdict. It does not preserve stale
presentation as if the new check had not happened.

When `lastProbeAt` is newer than `lastVerdictAt` and `lastProbeOutcome` is
inconclusive, the session card shows the outcome-specific copy before old
signed-in or signed-out copy. The underlying verdict remains unchanged.

This distinction prevents two false statements:

- a failed recheck cannot report **Signed out or expired**; and
- a prior signed-in verdict cannot hide that papio failed to confirm the
  session now.

The existing outcome vocabulary remains unchanged: `no_tab`, `no_markers`,
`scan_failed`, `partial_scan`, and `conflict` all remain inconclusive.

No new automatic-checking cause enters state. Automatic probes can settle or
queue behind the existing rate limit. The card reports their completed result,
not an estimated in-flight cause.

## Decision 7: privacy boundary

The new landing path has these limits:
- The untracked path runs only for a correlated provider host and an `auth_pending` job.
- It retains no landing URL, host, title, path, query, or fragment.
- It sends no landing data through native messaging.
- It persists only the configured resolver binding and pending reason.
- It reads only the configured resolver page already covered by the resolver
  permission model.
- It reads no cookie, credential, or identity-provider page.

These limits apply to this feature. They do not restate broader false claims
about all extension storage or all protocol frames. Existing delivery state can
contain a bounded source token, and existing browser messages can contain
provider hosts or route data under their own contracts.

## Decision 8: disclosure changes land with behavior

The same change updates these user-facing sources:

- `docs/privacy.md`
- `docs/guide/user-guide.md`
- `extension/src/options.html`
- `extension/docs/amo-listing.md`
- `extension/docs/chrome-web-store-listing.md`
- `extension/CHANGELOG.md`

The text must state:

- papio can recheck a configured library page while a paper is waiting for
  sign-in;
- the trigger uses a completed HTTPS navigation on that paper's declared
  provider host;
- other hosts and queued-only work do not authorize this publisher trigger;
- no cookie, credential, or page content leaves the machine; and
- the live store listings still require hand-copying during release.

ADR-0019 gains an amendment that limits its **one-shot explicit scan** rule to
page-content citation detection. ADR-0013 already governs automatic resolver
session probes.

## Rejected alternatives

### Read cookies

Rejected. Cookie presence cannot reproduce the resolver classifier's named
identity, non-guest, unexpired token, and sign-out-affordance checks. It also
requires a new high-trust permission and weakens current privacy promises.

### Treat a publisher page as the popup's signed-in verdict

Rejected. Open access and cached publisher pages can look usable without an
institutional session. Untracked publisher state schedules a resolver check
only. The pre-existing tracked return can release same-origin work but cannot
set the popup verdict.

### Match identity-provider hosts

Rejected. Identity-provider addresses are worker-local offer data and are not
part of durable job state. Persisting them would widen the existing privacy and
institution-identity contracts.

### Probe every non-resolver HTTPS landing

Rejected. It observes unrelated browsing and can select the wrong institution.
Provider correlation is mandatory.

### Use provider blocker fields for resolver permission

Rejected. Those fields own provider-page reclassification. Reusing them would
request or classify the wrong host.

### Produce `human_gate.browser_host_permission` now

Deferred. The vocabulary exists, but the browser protocol has no producer
frame, and the current gate identity cannot represent this resolver case
without coordinated protocol and store work. The extension-local permission
state is complete and replaceable; it is not a second user surface.

## Implementation sequence

1. Add the restart-safe institution binding and corrected demand predicate.
2. Add provider-correlated settled-landing admission and the durable pending
   recheck.
3. Correct effective permission matching, grant/revoke behavior, and the exact
   grant action.
4. Apply outcome-precedence copy and accessibility tests.
5. Update disclosures and amend ADR-0019.
6. Run targeted suites, both extension builds, and the live browser scenario.

Each slice must include its negative case. Important negative cases are queued
work, unrelated hosts, multiple institutions, no demand, missing binding,
permission-read failure, wildcard grants, revocation, and worker restart.

## Live acceptance

Before the scenario, rebuild and reload the development extension. Confirm
`papio browser sessions` shows a new session ID.

Positive path:

1. Park one non-open-access paper in `auth_pending` for one institution.
2. Confirm the session card shows the prior signed-out or unknown state.
3. Complete sign-in through that paper's publisher **Get access** path.
4. Wait for the completed provider landing.
5. Confirm the card leaves the stale signed-out presentation after the resolver
   check.
6. Confirm the parked work resumes without another sign-in.

Negative path:

1. Leave no job in `auth_pending`.
2. Browse and sign in through the same publisher.
3. Confirm no publisher-triggered institutional recheck, pending reason, or
   session-card change. The ordinary keep-warm cycle remains allowed.

Resolver-permission path:

1. Remove the configured resolver grant.
2. Confirm the card asks for library access and does not say signed out.
3. Grant only that origin from the card.
4. Confirm one recheck occurs when matching parked demand or a tracked return
   exists.
5. Remove parked demand and any tracked-return reason, then repeat the grant.
6. Confirm the grant causes no resolver read.

Ground truth comes from extension state, daemon job state, and a new browser
session ID. Popup copy alone is not sufficient proof.

### Run 2026-08-28

The positive path passed earlier against the operator's own Chrome and library.

The negative path passed in a throwaway environment built for it: a second
daemon on its own data directory with an empty library, and a dedicated Chrome
profile with the HEAD bundle installed as the deterministic dev-unpacked id.
With zero jobs, a real completed navigation to a declared provider host
(`www.cambridge.org`) left `keepalive.originStates` and
`keepalive.resolverOrigin` absent in extension storage, and left the daemon with
zero `profile_evidence` rows, zero claims, and no probe or recheck line in its
log, while the ordinary keep-warm cycle kept polling. One deviation from the
script, stated rather than glossed: the run did not perform an institutional
sign-in, because that needs the researcher's credentials. The discriminating
condition is the absence of parked demand — admission requires a correlated
job, so with zero jobs nothing can correlate — not the sign-in itself.

The resolver-permission path did NOT run, and the reason is structural rather
than incidental. Step 1 cannot be performed on Chrome at all: the resolver hosts
are declared `host_permissions`, Chrome grants them at install, and
`chrome.permissions.remove` refuses with "You cannot remove required
permissions". The path is therefore a Firefox scenario by construction, since
Firefox treats MV3 host permissions as runtime opt-in. Step 3 then needs a real
human click: `permissions.request` requires a user gesture, and extension
gesture tracking does not accept synthesized activation — the same limit already
recorded for the popup's Open button and for CDP's `userGesture`. Running it
needs a researcher at a Firefox profile, not more automation.

## Consequences

- papio becomes more proactive only while the researcher has a visible sign-in
  block for the same provider and institution.
- Some publisher sign-ins still wait for the normal keepalive cycle when no
  completed navigation occurs.
- One optional bare origin joins durable browser job state. It carries no
  landing or identity-provider data.
- Permission failures become actionable without becoming false session
  verdicts.
- The feature adds no browser permission, daemon wire field, or daemon schema
  change.
