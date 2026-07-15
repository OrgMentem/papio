# papio

**A local paper-acquisition broker.** Search scholarly works, create bounded acquisition jobs, validate candidate PDFs, and hand ready artifacts to your Zotero library — from the terminal or from a coding agent.

Finding a paper is easy; *legitimately acquiring* it and getting a validated PDF into your library is the tedious part. `papio` owns that glue: it discovers works on OpenAlex, resolves each request through open-access and licensed sources first, falls back to a visible institutional browser pass only when needed, validates every candidate before you trust it, and never writes to Zotero except through `zotio` behind a preview-and-confirmation boundary. It does **not** handle institution credentials, MFA, CAPTCHAs, or subscription crawling — those stay human decisions in your ordinary Chrome session.

## How it works

Each acquisition is a durable, bounded job. `papio` ranks candidates deterministically and resolves them in order — the broker never accepts the first URL it finds:

1. **Discover.** `papio search` returns bounded, read-only OpenAlex results, marking works already in your Zotio library.
2. **Acquire.** A capped batch (or one work) becomes durable jobs with stable request IDs, so reruns are idempotent.
3. **Resolve & fetch.** Open-access and explicitly licensed APIs run before institutional access; a bounded fetch quarantines each candidate.
4. **Validate.** Structure, identity, and a bounded OCR fallback gate every artifact before it is trusted; ambiguous identity parks in `needs_review`.
5. **Hand off.** Ready artifacts reach Zotero **only** through `zotio` — `papio zotio plan` previews an immutable mutation and `papio zotio apply` requires its exact confirmation SHA-256.

| Plane | Backend | Handles credentials? |
|---|---|---|
| **Discovery** | OpenAlex (read-only, bounded) | No |
| **Fetch — open** | arXiv · Europe PMC · Unpaywall · OpenAlex · CORE · Crossref TDM | No (API keys only where configured) |
| **Fetch — institutional** | OpenURL handoff in your ordinary Chrome session | No — login/MFA/CAPTCHA stay human |
| **Validation** | Local PDF structure + identity + bounded OCR (Poppler, Tesseract) | No |
| **Zotero writes** | `zotio` — preview (`plan`) then confirmed `apply` | No — `papio` never stores Zotero credentials |

`papio` runs in one of three access modes — `conservative`, `assisted`, or `maximal`. A fresh `papio init` chooses `conservative`; institutional handoff opens a browser only under `assisted`/`maximal`, and even then automation stays inside legitimate, user-authorized access.

## Quickstart

```bash
papio init                                                   # guided setup: config, data dir, DB, native host, doctor
papio doctor                                                 # verify readiness: sources, PDF tooling, Zotio boundary
papio search "appropriate reliance on AI" --limit 20 --year-from 2023
papio acquire 10.1371/journal.pone.0262026 --auto-import --wait
papio status --follow                                        # working / awaiting-human / needs-review / ready / failed
papio actions list                                           # open browser handoffs and identity reviews
```

Run [`papio doctor`](guide/troubleshooting.md#read-doctor-output) any time to see connectivity, PDF tooling, and Zotio-boundary readiness as stable `PASS`/`WARN`/`FAIL` rows.

New here? Start with the [user guide](guide/user-guide.md), then tune policy in the [configuration reference](reference/config-reference.md).

## Where to go next

<div class="grid cards" markdown>

- **[User guide](guide/user-guide.md)** — the research workflow: discover, acquire in batches, follow jobs, complete a browser pass, and resolve identity reviews.
- **[Configuration](reference/config-reference.md)** — every TOML key, default, constraint, and effect, from access mode to per-source budgets.
- **[Troubleshooting](guide/troubleshooting.md)** — extension reload, daemon recovery, reading `doctor`, and the stable Zotio-boundary error classes.
- **[Use in a coding agent](guide/agent-skill.md)** — drive papio over MCP (`papio mcp`): every `papio_*` tool, its safety semantics, and CLI equivalence.

</div>
