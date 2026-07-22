# Architecture decisions

*papio* records its load-bearing decisions in the acquisition-stack plan. This page gives the short version for contributors: preserve these boundaries unless the stated evidence for changing them exists.

## Go core, TypeScript browser plane

**Context:** The durable workload is local orchestration and state: resource-limited HTTP and subprocess calls, SQLite, deterministic transitions, hashing, atomic files, CLI/MCP, and release packaging. An ordinary cross-platform Chrome extension is JavaScript regardless of the core language.

**Decision:** Keep the daemon, CLI, native-messaging host, policy, queue, provenance, validation, and zotio integration in Go. Keep the Manifest V3 extension and provider DOM adapters in TypeScript. Each language stays on its side of the process boundary.

**Why:** One signed Go binary can serve the CLI, daemon, MCP server, and native host, while the extension remains a separately signed browser artifact. This keeps durable policy and untrusted-file handling out of the browser UI-drift plane without adding a Node/Bun runtime and native SQLite dependency to the installed host. All-TypeScript would not remove the extension/native-host boundary or selector drift; Rust adds toolchain and lifecycle burden without a required capability for this supervised-I/O core.

## Ordinary browser, not CDP

**Context:** Institutional access depends on the user's existing, authenticated browser session and requires visible human login, MFA, CAPTCHA, and terms actions. Cloudflare fingerprinted CDP in publisher trials, causing access loops.

**Decision:** Use an ordinary user-authenticated browser through the extension and native host. Do not use CDP, headless Chrome, stealth patches, copied cookies, or anti-bot evasion for publisher access.

**Why:** The browser handoff preserves the user's control and keeps protected-site access inside legitimate, user-authorized sessions. The extension reports narrow, metadata-only outcomes to the daemon; it does not turn browser automation into a credential or evasion channel. See [Browser handoff](../concepts/browser-handoff.md).

## zotio is the Zotero boundary

**Context:** Zotero metadata creation, deduplication, attachments, and mutation plans already belong to zotio.

**Decision:** *papio* never writes Zotero directly. It produces validated, provenance-linked acquisition output and hands it to zotio behind zotio's preview/apply boundary.

**Why:** A single owner for Zotero reads, writes, item schema, deduplication, and attachments prevents a second mutation path with different rules. *papio* owns acquisition; zotio owns Zotero mutation.

## Explicit access modes

**Context:** Open access, licensed APIs, and institutional browser handoff have different authorization and automation limits.

**Decision:** Require an explicit access-mode choice; never silently enable automation. `conservative`, `assisted`, and `delegated` define the allowed behavior, and licensed/TDM adapters remain separately enabled capabilities.

**Why:** Delegated automation is still limited to legitimate, user-authorized access: login, MFA, CAPTCHA, and publisher/library terms remain human actions. Unknown or changed provider UI falls back to assisted behavior rather than guessing. See [Access modes](../concepts/access-modes.md).

## Daemon-owned durable state

**Context:** Browser extension service workers can stop and restart, while acquisition jobs need authoritative transitions, recovery, provenance, and artifact tracking.

**Decision:** The single-user Go daemon owns the SQLite write connection and all state transitions. The MV3 extension is restartable and disposable; it keeps only minimal tab/job correlation and reconnects for authoritative state.

**Why:** A durable daemon can snapshot policy into jobs, schedule capped work, validate artifacts, and retain redacted event history independently of browser lifecycle or provider UI drift. The native host forwards size-limited messages but does not own the queue or persist browser state.

## Reversal triggers

**Context:** Adapter inconvenience alone is not evidence to revisit the architecture.

**Decision:** Reconsider the core language only before the final browser protocol v1 lock after Phase 3, and only under the plan's concrete triggers. A TypeScript core requires 2–3 real adapters showing that more than 70% of core changes necessarily co-change extension code, the cross-language protocol causes more demonstrated defects than it contains, and clean-machine Node/Bun packaging on every supported target for two release cycles. A Rust core requires in-process hostile-document/archive work at parallel scale, a security requirement for a memory-safe native daemon, or a remote multi-user service, with acceptance of the Rust/Pdfium/toolchain burden.

**Why:** Those conditions would change the product's risk profile; routine provider or adapter churn does not. The browser alternative that may be promoted is the optional macOS Apple Events adapter, and only when named required providers repeatedly fail through the extension but succeed through Apple Events. It never becomes a cross-platform architecture or a hidden CDP/stealth path.

## Handoff offers do not hard-expire

**Context:** Browser handoffs need a fresh resolver link when an institution rejects an old authentication request, but the daemon already reissues that link whenever a user opens actions or the inbox.

**Decision:** *papio* keeps the fresh-link-per-open model: every `papio actions open` or inbox open mints a fresh handoff link. The wire `expires_at` remains advisory; there is no daemon-side expiry sweeper, and `human_actions.expires_at` remains unenforced.

**Why:** Offers are local-only, so there is no confidentiality window to enforce. Reissuing on open already handles stale links, while hard expiry would add a user-visible failure mode without a safety benefit.
