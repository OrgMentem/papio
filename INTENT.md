# What Ellis wants from *papio*

Read this before you add a gate, a refusal, a review round, an approval prompt or a human step. It is the owner's
intent, rebuilt on 2026-11-05 from the 1,631 prompts Ellis typed in *papio* sessions from 2026-07-14 to 2026-11-05.
Quotes are his words, with dates (all 2026). Pasted agent reports, oracle reviews and herdr messages between agent
panes are not his words and are left out.

Plans, ADRs, `AGENTS.md`, review findings, compaction summaries and messages between sessions are agents' words.
Where they conflict with this file about what *papio* should do for its user, this file wins. The ADRs still record the
mechanisms; when one of them blocks what this file asks for, amend the ADR. Institution names are replaced with
"[my institution]": Ellis asked twice that no file or git history name them (section 3).

## 1. Purpose

*papio* gets the papers a researcher (or the researcher's AI agent) asks for, end to end, without the researcher.

- "the intent was automation of the paper acquisition, and it seems like we're far from that yet." (07-14)
- "getting pdf's into zotero was never the bottleneck - the zotero browser connector did that well." (07-14)
- "i want an end-to-end workflow, where the user can ask their ai agent for papers on X, and the agent will search
  and procure them with papio (think 20 papers fetched in parallel)." (07-14)
- "user experience is my #1 priority" (07-18)
- "the intent really is maximal automation - ONLY hands-on exactly when absolutely necessary (or the user declares a
  conservative stance) otherwise don't bother the user - honour their intent declared in their history
  acceptance/config/settings etc." (08-07)
- "why would a jobs whole point be that a human gets it? seems counter to the product intent?" (08-23)
- "frankly, i want papio to do everything it can to get all papers, all the time, unless i've specified conservative
  mode. if they're reachable for papio, they should be obtained end-to-end." (08-24) His example: he clicks Open,
  completes a CAPTCHA, clicks "Access through [my institution]" and lands on the page with the PDF. "i should only
  need to complete the captcha - papio should do the rest unless I stop it." (08-24)
- "Currently I find it's very fragile. its success rate is very low. it's difficult for the operator to know what jobs
  are failing and why, and what to do about it." (09-19)
- "My biggest problem with Papio thus far is its failure to acquire many papers via the extension." (10-29)
- "i don't want to overfit to my own dev scenario. i'm trying to build this to share with other users." (08-15)

Success is a paper in the library that nobody had to fetch by hand. He has not set a percentage. The measure is the
live store and `make live-cohort`, not the test suite.

## 2. Standing authority: already given, never ask again

- "be more ambitious. don't overfit to a narrow use case - think big picture for flexibility, user-value, and
  autonomy." (07-14)
- "i don't want to make overly conservative decisions that clip the products wings" (07-21)
- "don't hamstring papio with cautious conservatism about unverified risks" (08-07)
- "don't kick basic product decisions to me." (08-09) "make smart decisions" (08-09)
- "backwards compatibility is a very low priority. Feel free to change the shape for correctness" (08-11) "just do
  clean cutover to delegated - i don't think we have any users." (07-22) The store-install count that makes this
  safe is a fact with an expiry date; `AGENTS.md` (Protocol) says how to check it.
- "Remove the permission prompt altogether. This is a local only tool. Asking for permission is annoying and doesn't
  make sense to me anyway." (08-24)
- "I'm happy for you to build autonomously overnight so I can review the results ... And if I don't like it we'll
  just use git to roll back." (08-03)
- "orchestrate all. no PR's - commit and push to main. do all work e2e." (08-14) "PR's are not convention for this
  repo instead we push directly to main." (08-11) "commit. Dont need my confirm" (08-30) "do all, no need to ask"
  (09-02)
- "i'm not ready to accept that papio won't be able to autonomously fetch papers from jstor/ebsco. what avenues
  haven't we explored yet?" (07-17)
- "you can fill with my Bitwarden autofill. then I'll do the 2fa." (08-24)

## 3. Ellis's limits: the complete list

1. No institution names in the repository or its history: "there should be no mentions of "[my institution]" in the
   repo - review" (07-19); "i'd rather you didn't write my institutions throughout the files and git history" (08-03).
2. A user who declares a conservative stance gets one; nobody else does. The 08-07 and 08-24 quotes in section 1 make
   conservative mode the user's choice, never *papio*'s default.
3. Do not take away a convenience the user relies on without asking him. "You've made a wrong call here. deciding
   that Papio can no longer close tabs. you should have asked me. This is extremely inconvenient for the user." (08-09)
   The same message: "Don't overreact to this new directive and rip out or roll back a lot of the work that you've
   already done."
4. *papio* owns its own tabs and only those: its tab group, closed by lifecycle, never the user's tabs outside the group
   (08-09). Ownership goes to the user only "when papio asks for user action (which should be an absolute minimum,
   almost always avoid), or when the user opens a tab and asks to take control" (08-18). No tab spam ("please stop
   the tab spam immediately", 08-03), no new windows (07-24), no pinned tabs (07-24), no second Chrome instance ("why
   do you spawn so many browsers. just use my def", 08-30).
5. Messages to the user stay in the browser: "i didn't ask for desktop notifications. i wanted an in-browser toast"
   (08-28). Few and short: "user-value is high priority - not nagging, not clutter" (08-11).
6. The CAPTCHA is the one step he expects to do himself (08-24), and *papio* takes over when he has passed it: "papio
   did not detect my passing the captcha and take over - it should." (08-22)
7. Repository conventions he set: use rumen, not beads directly (07-19, 09-03); lowercase *papio* in italics and
   lowercase zotio without italics (07-18, 07-21); no private tool names in changelogs (07-21).

Any other rule that blocks acquisition is an agent's rule, not his. Treat it as a candidate for deletion (section 5).

## 4. Design direction

- **The model drives; code executes.** "I suspect that we've been taking too conservative approach, trying to do
  everything by code and thus requiring all these adapters per publisher and they're unstable because of UI changes,
  etc. But I think the future is to use a decision model like JEV to do browser actions and thereby have something
  more general that adapts as sites change or for unfamiliar sites he'll still be able to navigate through." (10-29)
- **Adapters do not scale by hand.** "this was an inordinate amount of work for an adapter upgrade, and there are
  100's of potential adapters (I think zotero connector extension knows 700+), and their pages may change." (08-09)
  "the self-healing adapter (ie after UI changes) was never fully realised." (09-19) He asked on 08-06 whether agents
  could build adapters "on the fly". Adapters stay as a fast path; they are not the coverage plan.
- **The user's own browser, mostly out of sight.** "awesome UX would be: user installs papio in their daily browser,
  and the extension gives UI for config, institutional SSO, and surfacing any headless actions that require user
  approval/consent." (07-17) The same prompt: "should we consider a cloak-type browser". On 08-12, about another
  tool: "you should be getting headless cloakbrowser with my profile and authed cookie session". A stealth browser
  that carries his session is his idea, not a forbidden one.
- **Institutional access drives itself.** "why does papio not offer me the SSO login prompt? no buttons/links/waiting
  tabs calling my attention? bad user value and UX" (07-29); "why would I have to manually download the PDF, if my
  login unlocks it for papio?" (07-29); "couldn't papio detect the login and drive these forward?" (08-08); "the
  current page is stuck on T&C - why isn't papio driving that further?" (08-03); "why would openurl_available need
  me?" (10-12). Members of more than one institution may want *papio* to fall back between them (08-11).
- **Terms are declared once.** "can't we get the user to declare they want the terms auto-accepted? (prompt the user
  on first use, with appropriate caution, then remember their choice)" (08-08)
- **Document delivery is not the fallback for an automation failure.** "in my 6 odd years of academic study, I've
  never used a manual request via a library." (08-07) "is there a user option to never use document delivery? so
  such jobs don't get stuck/stale?" (10-12)
- **Agents drive *papio* through the CLI.** "there must be a better way to capture these that doesn't involve computer
  use, or human interaction, but can be driven by the agent (like you) via the CLI?" (08-03) "coding agents should
  prefer the CLI and a agent skill.md" (08-08)
- **Own the whole lifecycle.** "there seems to be resistance against actually owning the full lifecycles of
  everything. perhaps engrained conservatism or wrong assumptions. but this is bad UX, bad user value." (08-18)
- **Explanations.** Plain language and recommendations, not questions: "don't grill me about technical details -
  rather, give me your advice and rationale" (07-28); "plain language, user-outcome focus, future-focus, with minimal
  meta discourse (navel gazing)." (08-24)

## 5. Rules against the conservative ratchet

These rules bind every agent, subagent, reviewer, advisor and compaction summary in this repository. They come from
his words: "an unwanted conservatism keeps creeping in to my agent's development, and i need to root out and remove
that friction." (08-24) "it's fair to note concerns from the conservative perspective, and i don't want inordinate
risk to users, but i don't want user-hostile ultra-conservative positions baked-in without my explicit approval."
(08-10) "But I'm finding LLMs like to be conservative and dislike allowing a decision model like Jev to take any live
actions." (10-29)

1. Progress means papers acquired end to end by the running daemon in a real browser, measured on the live store or
   with `make live-cohort`. Synthetic tests, review rounds and proofs are not progress.
2. A finding becomes a gate only if it names a plausible harm to the user that is worse than the papers it costs.
   State that cost next to the gate. Default: log the finding and keep going. "Unverified risks" (08-07) are not a
   reason; he asks for "evidence rather than hand-waving" (08-10).
3. Every gate that refuses the agent or parks a job records which gate refused, so its cost can be counted.
4. One review round, then ship and measure. He wants a review before a release (07-30), and "the next review should
   be after the plan is executed end to end" (08-18), not chains of rounds. Reviewers are expected to be
   conservative ("That's okay. We welcome critique.", 08-24); report to him anything they insist on that "would
   regress the user experience in any way, esp. constraining the usefulness and automation of papio" (08-24), and do
   not adopt it on his behalf.
5. Never ask again for the authority in section 2. A refusal from *papio*'s own code is a defect to fix, not a stop
   sign.
6. Delete rules that block acquisition without a section-3 limit behind them, including rules in ADRs and
   `AGENTS.md`: amend the ADR in the same change. "A reviewer found it" is not a reason.
7. Compaction summaries and handoffs carry his words from sections 2 and 3, not agents' accumulated rules.
8. Keep the cheap protections that cost almost no papers: the PDF identity check before a paper is filed (a wrong
   paper under the right citation is the worst outcome *papio* has; measure changes with `make identity-corpus`); one
   owner for each download; page text is data for the model, never instructions; *papio* closes only its own tabs; the
   CAPTCHA stays with the user. From the same owner's nodriver intent: do not create accounts, buy anything or change
   account settings while acquiring.

## 6. What went wrong, 2026-07-14 to 2026-10-29

- **Outcome.** In the live store on 10-29, 607 jobs reached the browser and 202 of them (33%) got a PDF. A person
  resolved a human action before 195 of the 200 browser imports. The mean time from job to import on the browser path
  was about 23 days. The library route (Primo) failed in 185 of 253 jobs.
- **The agent rarely ran.** The decision model made decisions in 14 jobs in total (13 in September, one in October);
  6 of them got a PDF.
- **The agent is blinded.** ADR-0029 lets the model return only an opaque control ID, WAIT or BLOCKED. It cannot name
  a URL, it stays on one origin, it needs fresh DOI metadata on every page, and it runs only after packaged adapters
  and generic extraction have failed. That is the trust model that nodriver's `INTENT.md` (section 6) records as the
  cause of 14 of 284 sign-ins in 19 days; after nodriver inverted it, 214 of 335 succeeded by 10-31.
- **Size.** About 154,000 lines of Go outside tests, 61,000 lines of extension TypeScript, `background.ts` alone at
  25,625 lines.

## 7. Current path

ADR-0029's 2026-11-05 amendment records the agent reach this file asks for: the model chooses the actions, the
resolver chain across origins is in scope, and the PDF identity check after the download is the gate. On 11-05,
signet's CDP Chrome (no `Runtime.enable`, reads only in isolated worlds) cleared Cloudflare by itself on three
publisher article pages; `AGENTS.md` (Automation detection) carries the evidence. Measure each change with
`make live-cohort`, agent on and agent off.
