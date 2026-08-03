# Operator experience

## Summary

Overhauls the daemon, CLI, and browser operator surfaces:

- bounded, solicited activity in `papio activity` and the inbox;
- send-to-*papio* delivery for the PDF already open in the browser;
- evidence-based, per-institution session visibility and keep-warm state;
- daemon and extension governors for handoff floods;
- visible, resumable handling of Cloudflare/Turnstile challenges and redirect loops;
- origin-scoped multi-institution session release on both sides of the bridge; and
- a durable creation ledger for tabs *papio* may close, with startup reconciliation of its group and work window.

## Review

`RevDaemon`, `RevExtCore`, and `RevExtUI` reported 20 findings; all were fixed. The five highest-risk findings were:

1. Session evidence could re-offer another institution's queue; evidence and both release paths are now resolver-origin scoped.
2. Challenge resume could classify without acquiring a governor slot; resumed work now obeys the same concurrency cap as a new drive.
3. Orphan cleanup could infer ownership from tab location; closing now requires durable evidence that *papio* created the tab.
4. Terms consent could change while an awaited operation was in flight; the consent latch is checked again before delivery proceeds.
5. Delivery provenance could bind to the wrong candidate or label contextless bytes institutional; it now binds to the created candidate and falls back to `manual` without evidence.

## Live validation

- **Flood and recovery:** Bulk release of 115 institutional handoffs opened dozens of tabs, thrashed the group, left roughly 100 tabs after the deploy, and triggered Cloudflare/OpenAthens hardening. The recovered path caps daemon offers and browser drives, parks challenges with a visible route back to the tab, closes settled tabs, and reconciles only ledger-owned leftovers.
- **the default institution pipeline:** A browser-driven the default institution session and daemon event forensics exercised the signed-in-but-no-resume path. Warm evidence now reaches the daemon, finds parked handoffs from prior sessions, and re-offers them; the real institutionally entitled JSTOR capture also replaced the invalid synthesized PDF URL with the page's primary download control.
- **Phantom rows:** Provider offer URLs first appeared as institutions, and an all-sites permission grant added a second set of wildcard-derived rows. Session rows now come only from config-derived `resolver_origins` in `hello_ack`; traffic and permission grants cannot add one.

## Config changes

- `browser.default_resolver` selects the named resolver profile used by acquisitions that do not name one. Empty preserves the historical default institution.
- Each `[browser.resolvers.<name>]` profile carries its own `shibboleth_entity_id` and `proquest_account_id`; named profiles never inherit the default institution's identity.
- Config decoding is strict. An older daemon rejects the new `default_resolver` key, so deploy the supporting daemon and the config change together rather than writing the key ahead of the binary.

## Migration notes

- The branch adds `0019_browser_delivery_context.sql`: nullable `browser_route` and `session_evidence` columns, plus conservative reclassification of legacy browser rows from `institutional` to `manual` when no route evidence exists.
- If another migration has claimed `0019` on the target before merge, rename this migration to `0020`; do not reorder or overwrite an applied migration.
- Keep the final schema number aligned in the clean-install assertion, doctor schema check, and forward-migration `user_version` tests.

## Test evidence

- Full Go suite passed.
- 462 extension tests passed.
- Chrome and Firefox bundles built.
- Documentation build passed.
