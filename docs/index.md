---
template: home.html
hide:
  - navigation
  - toc
---

Search for works, queue them for acquisition, check every PDF is the paper you asked for, and offer it toward your library — from the terminal or from a coding agent.

Zotero users get preview-first import through [zotio](https://github.com/OrgMentem/zotio). Users of papis, Calibre, plain folders, or custom scripts get a best-effort handoff through a one-line [`on_ready` hook](guide/hooks.md); hook failures never fail or retry the acquisition job.

Finding citations is easy; getting the PDFs, legitimately, is the tedious part. *papio* retrieves PDFs through open, licensed, or user-authorized institutional sources. It verifies each PDF and offers it to your library without handling your university credentials.

## How it works

Every request becomes a job. `papio` ranks the possible sources and tries them in order; it does not accept the first URL it finds:

![papio pipeline: discover works, acquire from open or institutional sources, validate PDFs, and file them in Zotero or another destination](assets/architecture.svg#only-light)
![papio pipeline: discover works, acquire from open or institutional sources, validate PDFs, and file them in Zotero or another destination](assets/architecture-dark.svg#only-dark)

1. **Discover.** `papio search` returns read-only OpenAlex results and, when zotio or a configured `library.sources` authority is available, marks works already in your library; without either, results are unowned/unclassified.
2. **Acquire.** A batch (up to 50 works) or a single work becomes jobs, each with a stable ID, so running the same request again is safe and won't duplicate.
3. **Find & download.** Open-access and licensed sources are tried before institutional access; each candidate is downloaded under strict size and time limits, then held in quarantine.
4. **Validate.** Every PDF must pass checks on its structure, its identity, and — if needed — a text scan before it is trusted; anything ambiguous waits in `needs_review`.
5. **File.** Validated PDFs use Zotero's `zotio` preview-and-confirmation path. Filing anywhere else — papis, Calibre, a plain folder, your own script — is a best-effort [`on_ready` hook](guide/hooks.md) handoff; hook failures never fail or retry the acquisition job.

| Stage | Source / tooling | Handles credentials? |
|---|---|---|
| **Discovery** | OpenAlex (read-only) | No |
| **Download — open** | arXiv · Europe PMC · Unpaywall · OpenAlex · CORE · Crossref TDM | No (API keys only where configured) |
| **Download — institutional** | OpenURL handoff in your ordinary browser session | No — login/2FA/CAPTCHA stay human |
| **Validation** | Local PDF structure + identity + OCR (Poppler, Tesseract) | No |
| **Library filing** | `zotio` — preview (`plan`) then confirmed `apply` for Zotero · best-effort `on_ready` handoff for papis, a folder, or your own script; hook failures never fail or retry the job | No — `papio` never stores Zotero credentials |

`papio` runs in one of three access modes — `conservative`, `assisted`, or `delegated`. A fresh `papio init` chooses `conservative`; institutional handoff opens a browser only under `assisted`/`delegated`, and even then automation stays inside legitimate, user-authorized access.

## Quickstart

```bash
papio init                                                   # guided setup: config, data folder, database, browser connector, health check
papio doctor                                                 # verify readiness: sources, PDF tools, zotio
papio search "appropriate reliance on AI" --limit 20 --year-from 2023
papio acquire 10.1371/journal.pone.0262026 --auto-import --wait
papio acquire --batch refs.bib                               # or RIS, CSL-JSON, NBIB — start from the library you already have
papio status --follow                                        # working / awaiting-human / needs-review / ready / failed
papio actions list                                           # open browser handoffs and identity reviews
```

Run [`papio doctor`](guide/troubleshooting.md#version-mismatches-and-updates) any time to see readiness across *papio*, the browser extension, its connector, and zotio.

New here? Start with the [user guide](guide/user-guide.md), then tune policy in the [configuration reference](reference/config-reference.md).

## Where to go next

<div class="grid cards" markdown>

- **[Getting started](guide/getting-started.md)** — prerequisites, `papio init`, and your first acquisition end to end.
- **[User guide](guide/user-guide.md)** — the research workflow: discover, acquire in batches, follow jobs, complete a browser pass, and resolve identity reviews.
- **[Filing: Zotero, papis & more](guide/hooks.md)** — where validated PDFs are offered: the zotio/Zotero preview boundary, and the best-effort `on_ready` hook for papis, Calibre, a plain folder, or your own script.
- **[Use in a coding agent](guide/agent-skill.md)** — drive *papio* from an agent: the `SKILL.md` that runs the CLI directly (preferred), `papio mcp` for MCP-only hosts, the canonical acquisition loop, and its safety semantics.
- **[Access modes & safety](concepts/access-modes.md)** — `conservative` / `assisted` / `delegated` and the non-negotiable product and safety boundaries.
- **[Acquisition pipeline](concepts/acquisition-pipeline.md)** — the order *papio* tries sources, how candidates are ranked, job states, and download limits.
- **[Browser handoff](concepts/browser-handoff.md)** — the ordinary-browser extension, its local connector, the minimized work window, and why *papio* never uses an automated browser.
- **[Validation & provenance](concepts/validation-and-provenance.md)** — PDF structure, identity, OCR gates, and the permanent acquisition bundle.
- **[Command reference](reference/commands.md)** — every `papio` command and its flags.
- **[MCP tools](reference/mcp-tools.md)** — the `papio_command_search` and `papio_command_run` command facade, the `papio_acquire_batch` and `papio_batch_wait` composite tools, and read resources, with parameters and boundaries.
- **[Configuration](reference/config-reference.md)** — every TOML key, default, constraint, and effect.
- **[Troubleshooting](guide/troubleshooting.md)** — extension reload, version mismatches, `doctor`, and the stable zotio error classes.

</div>
