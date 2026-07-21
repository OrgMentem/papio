# *papio*

**A local tool that finds scholarly papers and files validated PDFs into your Zotero library.** Search for works, queue them for acquisition, check every PDF is the paper you asked for, and hand it to your library — from the terminal or from a coding agent.

Finding a paper is easy; *legitimately getting* it and landing a validated PDF in your library is the tedious part. `papio` handles that: it finds works on OpenAlex, tries open-access and licensed sources first, falls back to a visible pass in your own browser only when needed, checks every candidate before you trust it, and writes to Zotero only through `zotio`, which shows you a preview first. It does **not** handle institution logins, two-factor codes, CAPTCHAs, or bulk-downloading from subscription databases — those stay your decisions in your ordinary browser.

## How it works

Every request becomes a job. `papio` ranks the possible sources and tries them in order — it never just grabs the first URL it finds:

![papio acquisition pipeline: you or an agent drive papio's jobs; open sources run before your own browser via the papio extension (installed once), where login, MFA, and CAPTCHA stay human; both paths converge in quarantine and PDF validation, producing a validated bundle with provenance that reaches the Zotero library through zotio preview-then-apply](assets/architecture.svg#only-light)
![papio acquisition pipeline: you or an agent drive papio's jobs; open sources run before your own browser via the papio extension (installed once), where login, MFA, and CAPTCHA stay human; both paths converge in quarantine and PDF validation, producing a validated bundle with provenance that reaches the Zotero library through zotio preview-then-apply](assets/architecture-dark.svg#only-dark)

1. **Discover.** `papio search` returns read-only OpenAlex results and marks works already in your zotio library.
2. **Acquire.** A batch (up to 50 works) or a single work becomes jobs, each with a stable ID, so running the same request again is safe and won't duplicate.
3. **Find & download.** Open-access and licensed sources are tried before institutional access; each candidate is downloaded under strict size and time limits, then held in quarantine.
4. **Validate.** Every PDF must pass checks on its structure, its identity, and — if needed — a text scan before it is trusted; anything ambiguous waits in `needs_review`.
5. **Hand off.** Finished PDFs reach Zotero **only** through `zotio` — `papio zotio plan` shows you exactly what will change, and `papio zotio apply` only runs after you confirm it.

| Stage | Source / tooling | Handles credentials? |
|---|---|---|
| **Discovery** | OpenAlex (read-only) | No |
| **Download — open** | arXiv · Europe PMC · Unpaywall · OpenAlex · CORE · Crossref TDM | No (API keys only where configured) |
| **Download — institutional** | OpenURL handoff in your ordinary browser session | No — login/2FA/CAPTCHA stay human |
| **Validation** | Local PDF structure + identity + OCR (Poppler, Tesseract) | No |
| **Zotero writes** | `zotio` — preview (`plan`) then confirmed `apply` | No — `papio` never stores Zotero credentials |

`papio` runs in one of three access modes — `conservative`, `assisted`, or `maximal`. A fresh `papio init` chooses `conservative`; institutional handoff opens a browser only under `assisted`/`maximal`, and even then automation stays inside legitimate, user-authorized access.

## Quickstart

```bash
papio init                                                   # guided setup: config, data folder, database, browser connector, health check
papio doctor                                                 # verify readiness: sources, PDF tools, zotio
papio search "appropriate reliance on AI" --limit 20 --year-from 2023
papio acquire 10.1371/journal.pone.0262026 --auto-import --wait
papio status --follow                                        # working / awaiting-human / needs-review / ready / failed
papio actions list                                           # open browser handoffs and identity reviews
```

Run [`papio doctor`](guide/troubleshooting.md#version-mismatches-and-updates) any time to see readiness across *papio*, the browser extension, its connector, and zotio.

New here? Start with the [user guide](guide/user-guide.md), then tune policy in the [configuration reference](reference/config-reference.md).

## Where to go next

<div class="grid cards" markdown>

- **[Getting started](guide/getting-started.md)** — prerequisites, `papio init`, and your first acquisition end to end.
- **[User guide](guide/user-guide.md)** — the research workflow: discover, acquire in batches, follow jobs, complete a browser pass, and resolve identity reviews.
- **[Use in a coding agent](guide/agent-skill.md)** — drive *papio* over MCP (`papio mcp`): the canonical acquisition loop and its safety semantics.
- **[Access modes & safety](concepts/access-modes.md)** — `conservative` / `assisted` / `maximal` and the non-negotiable product and safety boundaries.
- **[Acquisition pipeline](concepts/acquisition-pipeline.md)** — the order *papio* tries sources, how candidates are ranked, job states, and download limits.
- **[Browser handoff](concepts/browser-handoff.md)** — the ordinary-browser extension, its local connector, the minimized work window, and why *papio* never uses an automated browser.
- **[Validation & provenance](concepts/validation-and-provenance.md)** — PDF structure, identity, OCR gates, and the permanent acquisition bundle.
- **[Command reference](reference/commands.md)** — every `papio` command and its flags.
- **[MCP tools](reference/mcp-tools.md)** — the `papio_command_search` and `papio_command_run` command facade, the `papio_acquire_batch` and `papio_batch_wait` composite tools, and read resources, with parameters and boundaries.
- **[Configuration](reference/config-reference.md)** — every TOML key, default, constraint, and effect.
- **[Troubleshooting](guide/troubleshooting.md)** — extension reload, version mismatches, `doctor`, and the stable zotio error classes.

</div>
