---
name: protocol-triad-lands-together
description: "papio-browser/1 is validated in three places — protocol.go, protocol.ts, and browser-v1.schema.json must change in the same commit"
condition: ['.*']
scope:
  - "tool:edit(internal/protocol/protocol.go)"
  - "tool:write(internal/protocol/protocol.go)"
  - "tool:edit(extension/src/protocol.ts)"
  - "tool:write(extension/src/protocol.ts)"
  - "tool:edit(protocol/browser-v1.schema.json)"
  - "tool:write(protocol/browser-v1.schema.json)"
interruptMode: never
---

The protocol is validated **twice** and documented once. A wire change needs all three, in the same commit:

1. `internal/protocol/protocol.go` — emit + decode + `validate()`
2. `extension/src/protocol.ts` — `parseBrowserMessage`, and the `FieldSpec<T>` entry. That spec is **exhaustive**: every field of the payload interface must appear in `requireFields<T>(...)` with a disposition (`"required"`, `"optional"`, `"forbidden"`), and naming a field the interface does not declare is also a `bun run typecheck` error. Never silence a spec/interface mismatch by widening the interface — the mismatch is the signal that the field landed in one of the three places and not the others.
3. `protocol/browser-v1.schema.json`

**"Optional field" is only backward compatible in one direction, and it is not the one you need.** Both parsers reject unknown fields, so an optional field added to an *existing* message type is fine for a new parser reading an old frame and **fatal** for an old extension reading a new daemon's frame — it rejects the whole message. `stats_*` was added as a new message *type* for exactly this reason.

The breaking-change exception is gated on *verified* zero installs (AMO `average_daily_users` + the Chrome Web Store listing user count, both zero as of 2026-08-06) and requires all three files plus a rebuilt-and-reloaded extension. Store extensions auto-update while daemons update by hand, so the first real install ends the exception — and it can happen without anyone noticing. If you cannot verify zero installs right now, make the change additive behind a new message type or a `hello_ack` feature flag (`page_capture_terms_v1` is the worked example).

Two adjacent fail-closed edges while you are here: adding a daemon feature flag means updating the `required` literal in `NewBridge` **and** the exact advertised list asserted in `internal/browser/bridge_test.go`'s hello-ack test, in the same order; and the daemon's emitted feature cap stays at exactly 32 until an extension shipping the wider accept-side bound (`HELLO_ACK_FEATURES_ACCEPT_CAP`, 64) is actually in the field.
