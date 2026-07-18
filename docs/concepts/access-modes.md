# Access modes

*papio* separates discovery and acquisition policy from institutional access. Every
acquisition starts with open-access and enabled licensed API sources; browser
handoff is only considered when those sources do not produce an acceptable
PDF. See [Browser handoff](../concepts/browser-handoff.md) for the browser-side flow.

`access_mode` is a required first-run choice. A fresh guided `papio init` selects
`conservative`; *papio* never silently selects an automation mode. See the
[`access_mode` reference](../reference/config-reference.md).

## Choose an access mode

| Mode | Institutional-access behavior |
| --- | --- |
| `conservative` | Uses OA repositories and enabled licensed APIs only. *papio* can emit institutional or document-delivery actions, but does not open them. |
| `assisted` | Opens OpenURL in the user's ordinary browser. You log in and download the file; *papio* then adopts and validates the selected file. |
| `maximal` | Opens OpenURL, but login, MFA, and CAPTCHA remain human actions. After you return to a granted provider host, a verified adapter can navigate and initiate the one requested download. An unknown or changed UI falls back to assisted behavior. |

Licensed and text-and-data-mining adapters are separate, per-source capabilities.
They require their own explicit credentials, terms acknowledgement, rate and cost
budgets, and allowed uses; `maximal` does not grant them permission.

## Safety contract

Institutional access covers one explicit work request per
subscription-provider job. *papio* does not crawl a subscription database or a
journal issue; only OA and API sources process batches, and each batch is capped.

!!! warning "Never automate around access controls"

    Maximal automation operates only inside legitimate, user-authorized access.
    *papio* never bypasses access controls, captures credentials, solves CAPTCHAs,
    evades anti-bot measures, circumvents paywalls, automates MFA, or accepts
    publisher or library terms. Terms acceptance is always a human action.

- **Login stays human and local.** Authentication happens in the user's ordinary
  browser. The extension has no `cookies` or `debugger` permission and no host
  permissions for Example University, OpenAthens, or identity-provider domains. While
  authenticating, it compares origins locally and sends no identity-provider URL,
  path, title, query, or fragment through native messaging.
- **Browser reach is opt-in and narrow.** Each source has separate enablement and
  optional host permissions; *papio* never requests `<all_urls>`. A user gesture in
  the extension UI grants a permission, and revoking it immediately returns that
  source to assisted behavior.
- **papio decides, not the browser.** papio's own policy is authoritative; the
  browser reports what it sees and does, but cannot authorize a disallowed source
  or step. Saved job state lives in papio's local database, not in the
  restartable, disposable extension.
- **Uncertainty stops automation.** Unknown provider, page, or protocol states
  fail closed to `action_required` or `needs_review`; *papio* does not use a generic
  "click the likely download button" fallback.
- **The link to the browser carries neither files nor secrets.** The browser downloads
  into the folder papio set aside for that job, and the connector reports only a short
  description and a path. papio rejects paths outside that folder. Persisted URLs
  are redacted; signed query values, cookies, API keys, credential fields, and
  page bodies are not logged.
- **Ready means verified.** Before zotio sees a file, *papio* makes it permanent,
  checks its structure and identity, records its content hash, and links its
  provenance. `access_basis` and `reuse_license` remain
  separate: downloadable does not imply an open license or redistribution right.
- **zotio is the only path that writes to Zotero.** *papio* hands finished files to
  zotio rather than writing to Zotero directly; nothing else receives
  acquisition state.
