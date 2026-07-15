# papio

Papio is a local paper-acquisition broker: it searches scholarly works, creates
validated provenance-tracked PDF jobs, and hands ready artifacts to
[zotio](https://github.com/orgmentem/zotio) through a preview-and-confirmation
boundary. It uses ordinary Chrome for human-authenticated access and never
handles credentials, MFA, CAPTCHAs, or subscription crawling.

## Quick start

```sh
papio init
papio doctor
papio search "appropriate reliance on AI" --limit 10
papio acquire 10.1371/journal.pone.0262026 --wait
papio status
papio actions list
papio jobs list
papio version
papio daemon status
papio daemon stop
```

## Documentation

- [User guide](docs/user-guide.md) — research workflow, browser pass, reports,
  watches, and identity reviews.
- [MCP agent guide](docs/agent-guide.md) — every `papio_*` tool, safety
  semantics, and CLI equivalence.
- [Configuration reference](docs/config-reference.md) — every TOML key,
  default, constraint, and effect.
- [Troubleshooting](docs/troubleshooting.md) — extension reload, daemon
  recovery, doctor output, and stable Zotio error classes.

License: MIT.
