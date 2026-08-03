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
    papio adapter diagnose <job-id> --json
    ```

3. Open an issue or pull request and describe the provider, scenario, and capture you reviewed. Attach page content only after following the privacy check below. *papio* never uploads a capture or opens an issue automatically.

This is enough for a maintainer or coding agent to implement the adapter without asking you to reverse-engineer selectors.

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
3. Add classification and download assertions to `extension/test/adapters.test.ts`. Include every captured scenario and the provider's exact download method (`href`, `click`, `url`, `api`, or `meta`).
4. Run the focused checks:

    ```console
    cd extension
    bun run typecheck
    bun test test/adapters.test.ts test/capture.test.ts
    bun run build
    ```

5. In the pull request, state what was observed live, which fixture backs each rule, whether the PDF endpoint returned a real PDF, and which provider fronts remain intentionally assisted.

An adapter is not accepted from guessed selectors, generated vendor tables, or an uncaptured page. A new hostname needs its own capture when branding, routing, or DOM structure differs, even if the provider name is the same.
