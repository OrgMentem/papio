# CLI, MCP and IPC contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### CLI & MCP JSON output
- **Machine-readable output has ONE contract.** `internal/agentjson`
  (`Envelope`/`Truncate`/`Capped`) is shared by every CLI `--json` payload and the MCP
  resources — a list-shaped result is always `{"<name>": [...], "truncated": bool}`, never a
  bare array. A new list-shaped command must build its payload through the
  `printPage` helper (`agentjson.Envelope`), not marshal a bare slice.
  `internal/cli/conformance_test.go` walks the whole command tree against an
  explicit `commandClassification` registry (envelope/structured/none); a
  runnable command missing from that registry fails the test, so a new
  command cannot silently skip the envelope contract.
- **A new command joins the agent-facing MCP surface by DEFAULT, and the default is also
  "mutating".** `internal/mcpserver/cobratree` mirrors the whole Cobra tree: a command opts
  out with `mcp:hidden`, declares intent with `mcp:read-only`, and an **unannotated command
  is visible and treated as mutating**, so `papio_command_search` lists it and
  `papio_command_run` executes it. That default matters because papio's job is pulling
  attacker-influenced text (page titles, abstracts, triage items) into an agent's context,
  so every unannotated verb is reachable by prompt injection. `papio browser reload` shipped
  that way and had to be hidden after review: it can terminate the extension process and
  exists only for `make dev-deploy` and the `build.ts --reload` watcher, which invoke the CLI
  directly and do not care about MCP. Developer plumbing (`daemon`, `init`, `mcp`,
  `native-host`, `config`) is all hidden; copy that. The live tree is pinned by
  `internal/mcpserver/facade_test.go`'s hidden list — **not** by `cobratree_test.go`, which
  builds a synthetic tree and cannot see real commands. Hiding a command does NOT remove its
  `commandClassification` entry: that registry pins `--json` shape and RPC reachability,
  which a hidden command still has.
- **The daemon IPC layer is fail-closed too, so nothing on the wire is additive.**
  `internal/ipc`'s `decodeStrict`/`decodeJSON` both call `DisallowUnknownFields()`, and that
  covers the `Response` **envelope**, not just method params. Adding a field to `Response`
  (a tempting home for warnings) or changing any method's result shape therefore makes an
  older `papio` CLI reject *every* response from a newer daemon — and since one binary is
  CLI, daemon, and native host, skew is real whenever two copies are installed (see the brew
  vs `~/.local/bin` note in [Config and daemon](config-and-daemon.md)). Carry new daemon-side information either out of band (a log,
  or a new element in a list an existing result already contains — a `papio doctor` check is
  the cheap one) or behind a new method name; do not widen an existing result.

