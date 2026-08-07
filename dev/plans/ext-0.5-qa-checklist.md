# ext-v0.5.0 manual QA checklist (triage inbox)

Automated e2e is architecturally prohibited (no CDP/WebDriver on the user's
browser); this checklist is the release gate's manual matrix. Run on Chrome
(stable) and Firefox (built `firefox/` loaded via about:debugging), against a
deployed daemon with the triage features.

## Skew matrix (must pass all four)
- [ ] new extension + new daemon: inbox loads, all verbs work
- [ ] new extension + OLD daemon (no triage features): popup + inbox render
      the compat/feature-unavailable state; acquisition still works; no new
      message types sent (check daemon log for unknown-type errors — none)
- [ ] OLD extension + new daemon: hello/offers/page_acquire unchanged; daemon
      never emits triage frames unsolicited
- [ ] fresh install (no daemon): popup and inbox render daemon-down states

## Popup
- [ ] wordmark header, no "Paper launcher" heading, version in muted footer
- [ ] acquire disabled until DOI detected; "No DOI detected" muted state
- [ ] Enter triggers acquire when enabled
- [ ] Open inbox focuses the existing inbox tab (singleton), never duplicates
- [ ] CAPTURE FIXTURE section absent on store-installed build (Chrome),
      absent always on Firefox

## Inbox page
- [ ] bands ordered: retractions, human actions, watch hits
- [ ] j/k roving focus; a/d/o act on selection; shortcuts dead while the
      confirm dialog or an input has focus; help line visible
- [ ] acquire removes the hit (all watches), focus moves to next item
- [ ] dismiss (d) removes hit; re-running the watch does NOT resurrect it
- [ ] verify_identity: Accept disabled before View PDF; preview opens the
      loopback capability URL and renders in the browser PDF viewer (Range
      scrubbing works); Accept requires typed-out confirm dialog; job proceeds
      to ready and Zotero import (AutoImporter) after accept
- [ ] reject cancels the job after confirm; no preview required
- [ ] stale action (resolve via CLI in parallel, then accept in page) →
      inline conflict + automatic re-snapshot, no double-apply
- [ ] Load more (cursor) with >50 items; counts stay complete
- [ ] Chrome 142+: note whether a Local Network Access prompt appears on the
      loopback preview; if so, record UX and add to options-page guidance

## Broker / lifecycle
- [ ] badge: disconnected '!' > ungranted-permission count > pending count;
      tooltip names the state; count clears at 0
- [ ] notification click opens/focuses the inbox tab
- [ ] Chrome: let the SW go idle (30s+), then open inbox — first request
      reconnects and succeeds (ensureConnected); no stuck spinner
- [ ] Firefox: background event page unloads; reopening inbox re-snapshots
- [ ] daemon restart while inbox open: banner appears, Reconnect recovers
- [ ] mutation during disconnect fails cleanly and is NOT replayed on
      reconnect; refresh shows canonical state
- [ ] two inbox tabs cannot be created (openInbox focuses); direct URL open
      of a second tab still behaves (no broker state corruption)

## Security spot-checks
- [ ] options page / content script sending papio.triage.* is rejected
      (check via temporary console attempt on options page)
- [ ] preview URL from another profile/browser 404s after TTL; token absent
      from daemon logs; wrong Host (e.g. localhost:port) → 403
- [ ] hostile watch-hit title (markup in title from a seeded digest row)
      renders inert as text everywhere (list, dialog, notification)

## Update lifecycle
- [ ] with the inbox tab open, trigger an extension update (unpacked
      reload): update banner appears; reload flow lands on the new version
