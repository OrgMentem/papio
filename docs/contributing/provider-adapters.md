# Contribute a provider adapter

*papio* adapters are small, declarative rules backed by captured provider pages. They classify a page, identify a specific PDF control or endpoint, and stop when the evidence is incomplete. They do not inject provider-specific scripts or click a likely-looking button.

## The short path

When *papio* reaches a provider it does not understand, the extension waits for one bounded render window and attempts to save a sanitized diagnostic locally. The inbox then shows **No adapter yet** instead of driving the same page indefinitely.

1. Find the retained capture:

    ```console
    papio adapter captures
    ```

2. Build a job-scoped support report if you have the job ID:

    ```console
    papio adapter diagnose <job-id>
    ```

3. Open an issue or pull request and describe the provider, scenario, and capture you reviewed. Attach page content only after following the privacy check below. *papio* never uploads a capture or opens an issue automatically.

This is enough for a maintainer or coding agent to implement the adapter without asking you to reverse-engineer selectors.

For a known adapter that stops matching, the extension records a `drift`
capture under the adapter's canonical name. The daemon marks it as independent
evidence only after a separate provider outcome confirms the failure.
Unknown providers keep `observed` captures for development.

To inspect a repair proposal:

```console
papio adapter repair <capture-id-or-path>
```

The tool checks every rule of the requested kind through the production planner
before limiting the results. A working rule does not justify repairing an unused
sibling rule. Source patches edit only the literal fields in the verified
proposal, preserving other rules and comments. The tool excludes redacted
identifiers from selectors. A PDF-control
proposal cannot replace a separate access or identity check and unlock a
source patch. Article proposals require a PDF-specific affordance and rank
explicit PDF downloads ahead of viewer tabs. Stable control classes combined
with bounded PDF path segments rank above positional selectors; document IDs,
filenames, and query values do not supply those segments. Generic full-text links and
citation exports cannot qualify on their own. Controls labelled as previews,
samples, abstracts, supplements, or full issues cannot qualify as the requested
article PDF, even when page metadata matches. A complete plan proves neither
entitlement nor PDF bytes.
Review the access checks before testing a proposal in an isolated development
checkout. Verify the live file before promoting the repair.
A wrong resolver destination or missing PDF control can require a route
change instead of a selector change.

The JSON result reports `outcome: "proposal"` when reviewable source and test
patches exist, or `outcome: "blocked"` when they do not. A blocked workspace
contains the capture and an explanation, with no apply commands or partial test
patch. Each run gets a separate directory so an earlier proposal cannot survive
inside a later failed analysis.

Generated fixtures use a content-hash filename and leave existing scenario
fixtures intact. Their regression tests require the file to exist, resolve links
against its captured origin, and require a complete download plan for articles.
CI exercises the capture-store → proposal → generated test → source patch cycle
in a disposable checkout: the test fails before the patch and passes afterward.
This controlled check does not replace live PDF download, adoption and identity
validation after a real provider repair.

Connecting an extension with a newer adapter revision can automatically retry
eligible jobs parked by the older revision. During isolated testing, record the
normal daemon's jobs before reconnecting it: the upgraded browser can trigger
that recovery there too. These retries remain part of the normal job history;
they are not new isolated probes or proof that a file was acquired.

## Capture a specific scenario

A code contribution normally needs at least an entitled `success` page. Login, terms, and no-entitlement states need separate captures when the adapter distinguishes them.

```console
papio adapter capture https://provider.example/article/123 \
  --provider provider-id \
  --scenario success
papio adapter captures
```

The command uses the connected extension and your real browser session. It opens a governed tab, waits for the requested settle period, sanitizes the rendered document, stores the result under *papio*'s local capture directory, and closes the tab. If the extension reports that the host is not permitted, grant that exact provider origin from the popup and retry.

For an unpacked development extension, the popup also exposes **Capture fixture (dev)** on the active provider tab. That path uses the `activeTab` permission from your click and sends the same sanitized capture to the daemon.

## Privacy check before sharing

A capture has already had query strings, form values, comments, script and style bodies, and token-shaped values removed or masked. That is a safety floor, not permission to publish the remaining page.

Before attaching or committing a capture:

- read the complete sanitized HTML;
- remove account labels, institution-specific text, article body text, and unrelated page regions that the adapter does not need;
- keep the first `papio-fixture` provenance comment intact;
- do not include cookies, credentials, signed URLs, personal names, email addresses, or screenshots of authenticated account pages; and
- confirm that the smallest remaining fixture still reproduces the relevant provider state.

See [Privacy](../privacy.md) for the storage and disclosure boundary.

## Submit adapter code

1. Copy the reviewed capture to `extension/fixtures/<adapter-id>/<scenario>.html`.
2. Add one `AdapterSpec` to `extension/src/adapters/types.ts`. Use stable IDs, paths, and provider-owned data attributes from the fixture. Do not classify from URL query parameters: fixture sanitization removes them.
3. Add classification and download assertions to `extension/test/adapters.test.ts`. Include every captured scenario and the provider's exact download method (`href`, `click`, `url`, `api`, `meta`, or `post`). The `post` method accepts only an empty URL-encoded HTML POST form with an explicit same-origin HTTPS `.pdf` action. It downloads through the browser API without opening the form's target window; forms containing fields remain assisted.
4. Run the focused checks:

    ```console
    cd extension
    bun run typecheck
    bun test test/adapters.test.ts test/capture.test.ts
    bun run build
    ```

5. In the pull request, state what was observed live, which fixture backs each rule, whether the PDF endpoint returned a real PDF, and which provider fronts remain intentionally assisted.

An adapter can declare `excludedHosts` for a separate platform under one of its
provider domains. Exclusions include subdomains and apply wherever the browser
selects an adapter; they do not change the job’s permitted hosts. Such a platform
follows the missing-adapter path until its own captured rules exist.

A classification rule can pair `textAny` with `textSelector` to read a specific
status heading instead of the whole page. The selector must match exactly one
element; missing, invalid, or ambiguous matches leave the rule unsatisfied.
This prevents an abstract that quotes an access message from deciding access.
The render wait and repair proposals preserve the same scope.

An adapter is not accepted from guessed selectors, generated vendor tables, or an uncaptured page. A new hostname needs its own capture when branding, routing, or DOM structure differs, even if the provider name is the same.
