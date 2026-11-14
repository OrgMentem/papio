# PDF identity measurement

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### PDF identity rules (measure before you tune)
- **Every window and threshold in `internal/pdf/identity.go` is a measured tradeoff, and
  the measurement is now runnable: `make identity-corpus`.** It reads your Zotero library
  (read-only, through a temp copy of `zotero.sqlite`, safe while Zotero is open), scores
  every paper against its own metadata and against every other paper's, and reports the
  **wrong-accept** count — a wrong paper filed under the right citation being the worst
  outcome papio has. Run it before and after touching those rules and compare wrong
  accepts first, then the correct-pair pass rate; see `dev/identity-corpus.md`. The comments
  citing "1560 mismatched pairs" come from a one-off run that was never saved, which is why
  two false accepts survived review in a change that read as obviously safe. The report
  names your own library — never paste a run into a commit, an issue, or the CHANGELOG.

