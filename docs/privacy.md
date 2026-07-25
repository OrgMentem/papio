# Privacy policy

_Last updated: 2026-07-25_

*papio* is a **local** paper-acquisition tool. It runs on your own machine and, for
the institutional handoff, inside your own browser. This policy covers both the
*papio* application (daemon and CLI) and the **_papio_ browser extension**.

## The short version

**_papio_ collects no personal data. Nothing is sent to OrgMentem or any third
party.** The extension talks only to the *papio* application running locally on
your computer, over the browser's native-messaging channel. There is no *papio*
server, no account, no telemetry, and no analytics. The extension's acquisition-history
and impact figures are aggregates computed locally from your own job records and shown
only to you — see [What is stored, and where](#what-is-stored-and-where).

## What the extension does and does not do

- **No data collection or transmission to us.** The extension has no backend. It
  communicates solely with the local native-messaging host `com.orgmentem.papio`
  on your own machine.
- **No credentials are stored.** You sign in to your institution and solve any
  MFA or CAPTCHA yourself, in your own browser session. *papio* never sees, stores,
  or transmits your usernames, passwords, cookies, or session tokens.
- **No bulk scraping.** The extension downloads only the papers you explicitly
  request, one at a time, as part of a specific acquisition job. It does not
  crawl, harvest, or mass-download from publishers.
- **Your real session, not a bot.** The extension uses only standard extension
  APIs and native messaging — no WebDriver, CDP, or automation frameworks — so it
  operates as an ordinary part of your browsing.

## What is stored, and where

- **In your browser:** the extension keeps its own settings and short-lived job
  and tab state (via the `storage` API) so it can survive service-worker
  suspension and reconnect to the local app. This never leaves your browser.
- **On your computer:** the *papio* application stores the papers it acquires, along
  with their metadata and job records, in its local data directory. These files
  stay on your machine (and go only to your own Zotero library if you enable that
  integration).
- **Acquisition-history and impact figures:** the numbers the extension shows you —
  papers acquired, an estimated time saved, success rate, weekly acquisition trend,
  access-route breakdown, and human-handoff rate — are aggregates computed locally
  from those same job records, on demand, for display to you alone. No new data is
  collected to produce them, they are never transmitted anywhere, and this remains
  true to the "no telemetry, no analytics" statement above: nothing about your
  activity is sent to us or to any third party.

## Permissions

Each browser permission the extension requests is used solely to perform a
requested download and report the result to the local app — for example,
`nativeMessaging` to reach the local daemon, `downloads` to save the one
requested PDF, and host permissions to read the library/publisher pages needed
for a specific job. A per-permission explanation is available on the extension's
store listing.

## Third parties

*papio* does not sell your data, does not share it with third parties, and does not
use or transfer it for advertising, creditworthiness, or any purpose unrelated to
performing the acquisition you requested. When you request a paper, your browser
contacts your institution and the relevant publisher directly, exactly as it
would if you visited those sites yourself; *papio* adds no intermediary.

## Changes

If this policy changes, the "Last updated" date above will change and the current
version will always be available at this URL.

## Contact

Questions about privacy: open an issue at
[github.com/OrgMentem/papio](https://github.com/OrgMentem/papio/issues).
