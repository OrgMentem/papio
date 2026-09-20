# Bounded Jev decision trials

`extension/tools/jev-trial.ts` sends one sanitized observation to TypeSafe and
returns one choice among observed controls, `BLOCKED`, and `WAIT`. It is a
development tool. It neither controls the browser nor changes papio's runtime
authority model.

Use a private directory under `dev/scratch/`. Obtain permission for live calls
before running them. The client reads the macOS Keychain service
`typesafe-api-key` in memory and sends requests only to the fixed TypeSafe API
endpoint. Do not put the key in arguments, files, logs, or shell history.

```sh
bun run extension/tools/jev-trial.ts \
  --snapshot dev/scratch/my-trial/snapshot.json \
  --run-dir dev/scratch/my-trial/calls \
  --max-calls 20
```

The snapshot has this shape:

```json
{
  "goal": "Download the requested main article PDF to local disk",
  "page": {
    "url": "https://example.test/article",
    "title": "Article title",
    "text": "Sanitized visible context, including access or consent boundaries"
  },
  "controls": [{"id": "c1", "role": "button", "label": "Download PDF"}],
  "provenance": {"kind": "native-accessibility", "step": 1}
}
```

Review the snapshot before sending it. URL credentials, queries, and fragments
are removed, and extra object fields are omitted. Prose is **not** a general
personal-data scrubber. Exclude account details, cookies, form values, private
reading history, and signed download URLs. Preserve enough surrounding context
to distinguish the main article from references and recommended articles.

Reuse one run directory for the whole experiment. The default budget is 40
attempts and 500,000 request bytes; failed attempts count. There are no automatic
retries. Private receipts record hashes, model version, latency, usage, and
bounded response bodies. Missing usage after a failed call remains unknown.
The provider does not return a dollar charge in these receipts.

The snapshot hash binds the result to its input observation, not to the current
browser. Before executing a choice, independently check the page, control, and
authority again. Native accessibility indexes can change between observations.
Probability and confidence values do not establish permission or safety.

For provider acceptance checks, use native computer interaction and the real
authenticated browser. Keep debugger and WebDriver attachments away from those
tabs. Log explicit Open actions, navigation, sign-ins, model-selected actions,
and operator interventions separately. A supervised trial is not unattended
product behavior. Stop at the user's approval boundaries.

`jev-snapshot.ts` provides a separate offline screen over sanitized HTML
fixtures. It filters task-related controls and reports omissions. Captured DOM
does not establish live visibility. Grade extraction coverage separately from
selection: a missing candidate is an observation failure. Review expected
outcomes from capture contents, not scenario filenames alone.

Count acquisition success only after a fresh isolated job adopts and validates
the actual download. Inspect its first page and page count. A correct file left
in Downloads is a download success and an adoption failure. Keep trial files
private, preserve original jobs, close only trial-owned tabs, and restore the
normal native host and daemon before ending an isolated experiment.

The unit tests mock the API and never read Keychain or make paid requests.
