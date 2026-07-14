# Provider adapter fixtures

Captured HTML snapshots of real provider pages, used to fixture-test the
declarative adapters (`src/adapters/types.ts` → `interpret`) via the happy-dom
harness (`test/harness.ts`).

## Layout

```
fixtures/<provider>/<scenario>.html
```

- `<provider>` matches an `AdapterSpec.id` (e.g. `proquest`, `jstor`, `ebsco`).
- `<scenario>` is one of the six required states per provider:
  `article`, `login`, `terms`, `no_entitlement`, `wrong_work`, `selector_drift`.

Each new provider requires all six before it can be enabled (plan Phase 3).

## File format

A fixture is one HTML file: line 1 is a `papio-fixture` header comment, then a
newline, then the full **sanitized** document HTML.

```html
<!-- papio-fixture provider="proquest" scenario="article" origin="https://www.proquest.com/pqdweb" captured="2026-07-14T12:34:56Z" -->
<!DOCTYPE html>
<html>…</html>
```

The header carries only scheme + host + path in `origin` (never query or
fragment). The harness parses the whole file with happy-dom; the header comment
becomes an inert comment node and `interpret` ignores it.

## Capture → repo workflow

Fixtures are produced by the in-extension capture tool (`src/capture.ts`, popup
"Capture fixture" panel), which sanitizes the live DOM (strips scripts, inline
event handlers, credential/IdP-bearing nodes, and page-identifying secrets) and
downloads the result via Chrome to:

```
~/Downloads/papio-fixtures/<provider>/<scenario>.html
```

The download basename is deliberately `papio-fixtures/` — NOT this directory —
so a stray capture never lands directly in the source tree. To use a capture as
a test fixture, review it and move it here:

```
mv ~/Downloads/papio-fixtures/proquest/article.html \
   extension/fixtures/proquest/article.html
```

The header / newline / sanitized-HTML shape is unchanged by the move.

## Skip-when-missing

No real fixtures are committed yet — they are captured per provider during
Phase 3. `test/harness.ts#loadFixture` returns `null` when a file is absent, and
fixture-backed tests gate on it with `test.skipIf(doc === null)(…)`, so the
suite stays green before any capture exists. A committed fixture immediately
activates its test.

## Privacy

Captured HTML must already be sanitized by the capture tool before it leaves the
tab. Never commit a fixture containing credentials, IdP URLs/tokens, cookies, or
session identifiers. When in doubt, re-capture — the capture tool fails closed on
residual secrets.
