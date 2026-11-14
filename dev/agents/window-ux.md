# Work-window behavior

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Windows / UX
- Work-window mode (`papio_work_window_v1`, default on) puts handoff tabs in a **minimized
  background window**; provider SPAs may under-render while hidden — a per-adapter
  `requiresVisible` fallback is the intended fix if one stalls. Toggle off in options to debug.
