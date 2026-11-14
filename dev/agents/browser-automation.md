# Browser automation boundaries

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Automation detection (this is load-bearing — papio's whole value is "real human browser")
- **Never drive the user's browser via WebDriver/BiDi for real work.** Firefox BiDi sets
  `navigator.webdriver = true` → Cloudflare/Turnstile hardens and often becomes unpassable.
- `navigator.webdriver` is set by `--enable-automation` (which `puppeteer.launch` adds),
  in headless Chrome, and by `--remote-debugging-pipe` alone in headed Chrome 156. signet
  launches with `--disable-blink-features=AutomationControlled`, which keeps it false (its
  `docs/DESIGN.md` section 6, measured 2026-11-04).
- The claim that Cloudflare fingerprints **any CDP attachment** does not hold for a client
  that never sends `Runtime.enable` and reads the page only in isolated worlds. On
  2026-11-05, signet's headless Chrome over `--remote-debugging-pipe` (those rules, a
  persistent profile, no sign-in) opened article pages on SAGE, Wiley and T&F: each showed
  Cloudflare's "Just a moment..." (403) and cleared it by itself within ~8 s, with no
  click. ScienceDirect refused the same Chrome with a hard 403 that did not clear. The
  egress is the home ISP address, not a VPN, so the cause is either that
  profile or the address itself; one ScienceDirect article opened by hand in the user's
  own Chrome tells them apart. The older observation used `--remote-debugging-port` on a
  fresh profile, and its client's commands are not recorded. This evidence is for a
  separate browser that carries the user's session (`INTENT.md` section 4), not for the
  user's own profile.
- Production papio uses **native messaging + extension APIs only — no CDP, no WebDriver —
  in the user's own browser.** `chrome.debugger` was considered for the agent and rejected
  (ADR-0029, 2026-11-05 amendment): CDP input is visible to the page (pointer events carry
  `sourceCapabilities: null`), Chrome shows a debugging banner, and a provider flag would
  land on the user's own profile, home IP and institutional licence.

