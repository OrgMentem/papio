# ADR-0006: Compatibility gates for daemon-initiated browser frames

Status: Accepted (2026-07-27). Resolves the directionality gap in ADR-0001's
*solicited only* rule without changing its prohibition on unsolicited daemon
push — see [Relationship to ADR-0001](#relationship-to-adr-0001).

## Context

ADR-0001 requires new extension requests to wait until the daemon advertises a
feature in `hello_ack.features`, and says the daemon never pushes new message
types unsolicited. That negotiation is directional: `hello_ack` tells the
extension what the daemon supports. It cannot tell the daemon that the
extension can parse a new daemon-initiated frame.

The implementation exposed this gap with `handoff_focus`. When a user runs
`papio actions open`, the daemon sends that message to focus the corresponding
handoff. Its concrete compatibility gate is
`HandoffFocusMinExtensionVersion` in `internal/browser/bridge.go:59-62`: the
daemon sends the frame only to an extension at or above that floor. The user
explicitly requested the operation, but an old extension would still fail
closed on an unknown frame if it received one. The version floor preserves the
invariant ADR-0001 protects: never send a peer a frame it cannot parse.

The review also found an important weakness in that mechanism. Legacy sessions
all normalize to one session identity, so a stale pre-floor host could
otherwise receive `handoff_focus` and drop the session. The version-floor path
therefore needed an additional legacy-session special case. This is evidence
that a version floor is more fragile than negotiated features; it is a
necessary directional compatibility boundary here, not an equivalent general
replacement for feature negotiation.

`page_capture`, added in the same session, is outside this decision. It travels
extension-to-daemon and is gated by the daemon-advertised `page_capture_v1`
feature exactly as ADR-0001 requires.

## Decision

Keep ADR-0001's feature negotiation for every new extension-to-daemon request:
the extension sends it only after `hello_ack.features` advertises its feature.

For a new daemon-to-extension message that is sent only in response to an
explicit user action and cannot be expressed through that negotiation, gate
its emission on an extension version floor. The sender must also account for
legacy session identity before choosing a recipient. This exception does not
permit daemon-initiated background push or a new message type sent without a
compatibility gate.

## Consequences

Future protocol work chooses its gate by direction:

- **Extension to daemon:** add and require a daemon-advertised feature flag.
- **Daemon to extension:** retain ADR-0001's no-unsolicited-push rule. For the
  narrow explicit-user-action case above, require an extension version floor
  and an explicit legacy-session routing check before emitting the frame.

A future bidirectional capability advertisement would reopen this decision. It
could replace the fragile version-floor exception only after it lets the daemon
reliably determine that each connected extension can parse the new frame.

## Relationship to ADR-0001

This ADR amends only the unexpressible daemon-to-extension direction of
ADR-0001's *solicited only* rule. Its feature-gated extension-to-daemon rule,
including `page_capture_v1`, stands unchanged. The deferred live-push question
remains resolved by ADR-0005: no background daemon push is authorized by this
ADR.
