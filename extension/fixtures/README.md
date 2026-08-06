# Provider adapter fixtures

Captured HTML snapshots of real provider pages, used to fixture-test the
declarative adapters (`src/adapters/types.ts` → `interpret`) via the happy-dom
harness (`test/harness.ts`).

## Layout

```
fixtures/<provider>/<scenario>.html
```

- `<provider>` matches an `AdapterSpec.id` (for example `proquest`, `jstor`,
  `ebsco`, or `springer`).
- `<scenario>` is one of the capture registry's six states: `success`,
  `login-return`, `terms`, `no-entitlement`, `wrong-work`, or `drift`.

An enabled provider requires fixtures for every state observed live plus a
deliberate selector-drift failure. States that cannot be reached safely or do
not exist for that provider remain assisted rather than being fabricated as
automatable.

## File format

A fixture is one HTML file: line 1 is a `papio-fixture` header comment, then a
newline, then the full **sanitized** document HTML.

```html
<!-- papio-fixture provider="proquest" scenario="success" origin="https://www.proquest.com/pqdweb" captured="2026-07-14T12:34:56Z" -->
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
mv ~/Downloads/papio-fixtures/proquest/success.html \
   extension/fixtures/proquest/success.html
```

The header / newline / sanitized-HTML shape is unchanged by the move.

## Coverage and missing states

Committed live-sanitized fixtures activate their corresponding tests
immediately. `test/harness.ts#loadFixture` returns `null` when a provider state
was not safely reachable, and that state remains assisted. Every enabled
provider also carries a deterministic drift fixture so selector changes fail
closed instead of initiating a guessed download.

## Trying a spec against a capture

Editing the adapter registry, rebuilding the extension, reloading it, and
re-driving a live institutional handoff is far too slow a loop to repair an
adapter that matches a host and then fails to classify the page — that
failure mode alone accounted for 99 of 103 adapter failures measured on a
live install. Because `interpret` (`src/adapters/types.ts`) is pure and
DOM-only, a spec can instead be checked against a stored capture entirely
offline, with `tools/adapter-try.ts`:

```
bun run adapter:try -- fixtures/tandfonline/success.html --id tandfonline
bun run adapter:try -- fixtures/tandfonline/no-entitlement.html --id tandfonline --expect article
```

It prints the resolved verdict, then walks `spec.classify` rule by rule (in
declared order) showing exactly which `all`/`any` selectors matched and which
`textAny` needles were found — including for rules after the first full
match, so a losing rule still shows which single selector cost it the match.
When the verdict is `article` and the spec declares a `download` rule, it also
reports whether `download.selector` resolves and the URL that method would
produce. `--expect <kind>` exits 1 when the verdict differs, which makes the
command usable as a check while iterating.

A candidate spec that is not (yet) in the registry — drafted while repairing
or adding an adapter — can be tried the same way with `--spec` instead of
`--id`, pointing at a JSON file shaped like one entry of the `adapters` array:

```
bun run adapter:try -- fixtures/newprovider/success.html --spec draft-newprovider.json --expect article
```

`--title`/`--doi`/`--year` populate `AdapterContext.expected`, the same
wrong-work title-token check `interpret` runs live. Both a committed fixture
under `fixtures/<id>/<scenario>.html` and a raw capture retrieved with
`papio adapter captures` work as the positional `<captured.html>` argument.

## Privacy

Captured HTML must already be sanitized by the capture tool before it leaves the
tab. Never commit a fixture containing credentials, IdP URLs/tokens, cookies, or
session identifiers. When in doubt, re-capture — the capture tool fails closed on
residual secrets.
