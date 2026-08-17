# Click-adapter download vs armed PDF grab — surviving classification race

Status: **characterization only** (2026-08-17). No code change in this note.

## Summary

Two Chrome download listeners (`onCreated`, `onDeterminingFilename`) plus three
classification strategies (exact job binding, armed grab, inferred tab/host job)
can disagree about the same `DownloadItem`. Commit `c3590b3` fixed the **wrong-paper**
case where a **papio-started** job download (exact URL binding via
`pendingDownloadURLs`) shared a route with an armed grab. The **pre-existing**
grab-before-job ordering in `onDownloadCreated` was already present before the
30444d5 delivery work; a reviewer mis-attributed it to that branch — the git
evidence below is definitive.

What remains is narrower: **click-adapter** downloads never register an exact URL
binding, so an armed grab that matches the same download route still wins both
listeners over an **inferred** job correlation. That starves the acquisition job
and feeds the grab pipeline instead. That is usually a **workflow stall** or grab
**review park**, not a silent misfile — unless identity validation is also wrong.
A second, rarer class is **listener skew** around the narrow `pendingDownloadURLs`
window and `track.ids.add` timing after `await downloads.download()`.

---

## Provenance (pre-existing; not introduced by recent grab work)

Independent verification (required by assignment):

```text
$ git show 30444d5^:extension/src/background.ts | grep -n 'pendingGrabFor'
9400:  private pendingGrabFor(item: DownloadItemLike): PdfGrabTrack | undefined {
9452:          ? this.pendingGrabFor(item)
15983:    const pendingGrab = this.pendingGrabFor(item);
```

At parent of `30444d5`, `onDownloadCreated` already ran **`pendingGrabFor` first**
and **returned early**, before any exact job binding:

```typescript
// 30444d5^:extension/src/background.ts (~15983)
const pendingGrab = this.pendingGrabFor(item);
if (pendingGrab !== undefined) {
  pendingGrab.ids.add(item.id);
  this.grabDownloads.set(this.trackedGrabFor(item.id) ?? "", pendingGrab);
  return;
}
const earlyJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
// ...
```

`c3590b3` reordered this block so exact job binding runs first, with an explicit
comment that the old order misfiled click-adapter/job bytes into a grab when routes
collided (`extension/src/background.ts:16297-16303`, `16304-16320`).

`30444d5` separately changed **`onDeterminingFilename`** so a live grab outranks
**inferred** job correlation (not exact binding) — fixing handoff-tab reuse where
the viewer Download was steered into the first paper's job directory while a grab
for the document actually being read went unfulfilled (`extension/src/background.ts:9760-9767`; commit message defect 3).

Neither commit removes the click-adapter vs grab overlap: click effects never
populate `pendingDownloadURLs` (`extension/src/background.ts:15907-15938`).

---

## The three claimants (what each listener consults)

| Claimant | Mechanism | Exact vs inferred |
|----------|-----------|-------------------|
| **1. Exact job binding** | `trackedJobFor(item.id)` or `pendingJobFor(item)` — URL/route match against `pendingDownloadURLs` while papio's own `downloads.download()` is in flight, or ID already in `this.downloads[job].ids` | **Exact** (extension started this download for that job) |
| **2. Armed grab** | `trackedGrabFor(item.id)` or `pendingGrabFor(item)` — route match against `pendingGrabDownloadURLs` via `sameDownloadRoute` (origin + pathname; query ignored) | **Explicit** researcher Send PDF / armed `awaiting_viewer` correlation |
| **3. Inferred job** | `correlate(item)` — tab ownership (`findByTab`), then host + `download_initiated` / manual host rules | **Inferred** (tab used to hold something) |

Helpers: `pendingJobFor` / `pendingGrabFor` at `extension/src/background.ts:9673-9714`; `correlate` at `16225-16266`.

**Click-adapter path:** `dl.method === "click"` calls `claimDownloadInitiated` and
executes the page click but **does not** call `chrome.downloads.download` and **does
not** set `pendingDownloadURLs` (`15907-15938`). The browser's native download is
classified only via (3), unless (1) applies from some other papio download on the
same item.

**URL / API adapter path:** sets `pendingDownloadURLs.set(url, jobID)` immediately
before `await downloads.download(...)` (`15944-15958`, and similarly
`5337-5364`, `12066-12125`, `13986-14007`, `15475-15531`).

---

## Chrome event order (grounded in harness)

The extension tests document emission order for adapter-initiated downloads:

```text
// extension/test/background.test.ts:4465-4466
// Chrome fires onCreated (still the requested URL), then onDeterminingFilename
```

So for a single item, **`onDownloadCreated`'s synchronous prefix runs before
`onDeterminingFilename`**, unless the harness reorders manually.

Inside `onDownloadCreated`, classification before `await this.ready` is
synchronous (`16296-16321`). `onDeterminingFilename` is a synchronous listener
(`9744-9781`).

---

## Decision order in each listener (current tree)

### `onDeterminingFilename` (`9744-9780`)

1. `exactJobID = trackedJobFor(id) ?? pendingJobFor(item)` → `exactJob`
2. `grab = tracked grab OR pendingGrabFor(item)`
3. If `grab` defined, filename non-empty, and **`exactJob === undefined`** →
   `suggest({ filename: grab.steeringPath + base })` and **return** (grab beats
   inferred job only)
4. Else `job = exactJob ?? correlate(item)` → suggest `papio/${job.job_id}/${base}`

### `onDownloadCreated` (after `c3590b3`, `16296-16400`)

1. `earlyJobID = trackedJobFor(id) ?? pendingJobFor(item)`
2. If `earlyJobID` → add id to job `DownloadTrack` (**no early return**)
3. **Else** if `pendingGrabFor(item)` → add id to grab track, **`return`** (skip
   job correlation entirely)
4. `await this.ready`, optional manual-delivery guards, then
   `exactJobID ?? correlate` and add to job track again

**Asymmetry:** When the job wins via (1), `onDownloadCreated` still falls through
to correlate; when grab wins via (3), it **never** binds the job.

---

## Exact interleavings that still misbehave

### A. Fixed by `c3590b3` — papio URL job + armed grab, same route

**Setup:** Job adapter uses `method !== "click"` (or delivery/direct/generic
download). `pendingDownloadURLs` holds the job id. Grab armed for the same
`downloadRoute` (signed query differs — `sameDownloadRoute` still matches).

| Step | Event | State read | Winner |
|------|-------|------------|--------|
| 1 | `onCreated` (old) | `pendingGrabFor` matches first | **Grab** track; early return — job never bound |
| 2 | `onDeterminingFilename` | `pendingJobFor` still set → `exactJob` | **Job** steering path |

**Result (pre-fix):** File steered under `papio/<job_id>/…` while `onDownloadChanged`
follows **grab** ownership → grab pipeline / job adoption split; wrong-paper risk
and stuck grab. **Current tree:** step 1 binds **job** first; both listeners agree on
exact job.

### B. Surviving — click-adapter job + armed grab, same route (typical)

**Setup:** Handoff job on tab T; adapter click sets `download_initiated` (`15908`).
Researcher arms grab on route R (Send PDF / `awaiting_viewer`).
Click produces a download whose URL matches R (same pathname as armed grab).

| Step | Event | `exactJob` | Grab | Inferred job | Winner |
|------|-------|------------|------|--------------|--------|
| 1 | `onCreated` sync | undefined (no `pendingDownloadURLs`) | `pendingGrabFor` | would be tab job | **Grab** — early return |
| 2 | `onDeterminingFilename` | undefined | matches | `correlate` would pick tab job | **Grab** steering (`9768`) |

**Both listeners agree: grab.** The acquisition job **never** receives
`this.downloads[job_id].ids` for this id.

**Researcher-visible impact:**

- **Job path:** handoff stays in assisted / awaiting download; no
  `download_complete` for that job from this click.
- **Grab path:** bytes land under daemon `steering_path`; extension notifies
  `papio.pageBulk.grabState` → identifying / terminal states (`16468-16474`,
  `8351-8372`). Wrong identity → daemon validation **parks** (project rule:
  declining beats guessing) — **review cost**, not silent wrong bind.
- **Not** "wrong Downloads folder" in the Chrome sense if both agree on grab; the
  severity is **wrong pipeline** (job starved, grab consumes bytes) or **park**
  on identity mismatch.

**Reachability:** Requires **simultaneous live job click effect and armed grab**
on the **same download route**.

- **Effect governor:** click path **releases** the governor before the native
  download appears (`15930-15931`), so Send PDF can arm a grab while the click
  download is still starting.
- **Same route:** common for signed CDN PDFs where `downloadRoute` ignores query
  (`1467-1474`, `8673-8685`) — job click and workspace grab on the same viewer
  PDF pathname.
- **Different tabs:** grab tab id is stored on the grab track but **`pendingGrabFor`
  does not require tab match** — route-only match is enough (`9705-9711`). A job
  click on tab T can lose to a grab armed from workspace tab T' if routes collide.
- **Likelihood:** low in calm use; **material** when operator runs handoff
  acquisition and page-bulk Send PDF on the same publisher PDF without waiting
  for the job to finish.

### C. Surviving — narrow window: exact binding dropped between listeners

**Setup:** Papio calls `downloads.download()` for a **job**; grab also armed for
same route.

| Step | Event | Condition | Winner |
|------|-------|-----------|--------|
| 1 | `onCreated` | `pendingJobFor` true | Job track updated |
| 2 | `finally` on download initiator | `pendingDownloadURLs.delete` | Exact URL latch cleared |
| 3 | `onDeterminingFilename` | Before `track.ids` visible to `trackedJobFor` | If latch gone and id not in set yet → **`exactJob` undefined**, grab can steer |
| 4 | `onCreated` continuation | Later `ids.add` | Job track may still update in async tail |

Harness comment at `9690-9691` acknowledges ID-before-filename ordering; initiators
usually add ids **before** clearing `pendingDownloadURLs` (e.g. `15952-15958`), which
shrinks the window. **`startDeliveryDownload`** adds ids before `finally` (`5345-5364`).
**Grab-initiated** papio download adds ids **after** `await` (`8714-8720`) — separate
known bug (interrupted before track, `8729-8737`), not click-adapter but same
`ids.add`-after-await pattern.

**Impact if skew occurs:** disk path from (3) vs `onDownloadChanged` owner from (4)
— **cardinal misfile class** if job dir on disk and grab owns id (or vice versa).

### D. Inferred job would win steering pre-`30444d5`; grab wins now

Handoff tab opened for paper A, reused to read paper B; viewer Download while grab
armed for B. **Before** grab-outrank fix, `onDeterminingFilename` could steer to job A
via `correlate` while grab starved. **Now** grab wins when `exactJob === undefined`
(`9768`). `onDownloadCreated` still assigns grab on route match (case B). Observed
failure mode in commit message: bytes validated against wrong job → **review park**,
grab unsettled — not silent success.

---

## Why `c3590b3` ordering is not sufficient alone

1. **Click downloads are never exact-bound** — reordering helps URL-initiated papio
   downloads only. Click-adapter still hits `pendingGrabFor` in the `else` branch
   (`16314-16319`).
2. **Two listeners, two code paths** — no shared "classification record" for a
   given download id; steering (`onDeterminingFilename`) and ownership
   (`onDownloadCreated` / `onDownloadChanged`) can theoretically diverge in window C.
3. **`sameDownloadRoute` is intentional** — signed query must not be a steering key
   (`1465-1474`, `c3590b3` commit message); any fix must not reintroduce bearer
   material into storage or break CDN normalization.
4. **`pendingGrabFor` is route-global** — not scoped to grab `tabID` (`9705-9711`),
   so grab can claim a click download that originated on another tab if routes match.

---

## Firefox

- `isFirefox()` ≡ `onDeterminingFilename === undefined` (`5167-5168`).
- `correlate()` **returns immediately** without tab/host inference (`16229-16230`).
- Click adapters are **`isFirefoxClickDownload`** — treated as human-assisted;
  correlate refuses them (`16234-16235`, `14227-14239`).
- PDF grab availability requires `onDeterminingFilename` (`8330-8336`); armed grab /
  viewer adoption paths decline on Firefox (`8694-8700`, `5433-5436`).
- **This Chrome-only race does not apply verbatim on Firefox.** The surface is
  "cannot steer / cannot grab" rather than grab-vs-job steering collision.

---

## Fix options (ranked; prefer park over pick)

| Rank | Approach | Effect | Risk |
|------|----------|--------|------|
| **1 (recommended)** | **Ambiguity → park both sides** — if `download_initiated` job on item tab (or `claimDownloadInitiated` latch) **and** armed grab matches route, refuse to classify: do not steer, do not attach id to grab; surface "conflict — finish job or cancel grab" | Matches standing rule: ambiguous → park, never guess | Requires UI/copy; job and grab may both stall until human clears one |
| 2 | **Single classification snapshot** per download id computed once (first listener), both listeners read it | Eliminates listener skew (C) | Must define lifecycle + MV3 restart; careful with sync Chrome callbacks |
| 3 | **Exact binding for click** — on `claimDownloadInitiated` for `method === "click"`, register tab-scoped (or route-less) pending job token like `pendingDownloadURLs` | Click downloads become exact job in both listeners | Must not treat every host click as exact when user manually downloads; tab reuse still hard |
| 4 | **Narrow `pendingGrabFor`** — require `item.tabId === grab.tabID` (and maybe `awaiting_viewer` only) | Reduces cross-tab route collision | Breaks intentional route-only arming (`8688-8690` comment); may leave grabs unfulfilled when tab id missing |
| 5 | **Grab claim token at arm time** — mint nonce; only downloads presenting token correlate | Strong ownership | Wire/protocol or storage change; heavy |
| 6 | **Accept race** | No engineering cost | Job starvation + grab parks remain |

**First choice:** **#1 (ambiguity park)** — when both a delegated click-adapter job
(`download_initiated` on correlating tab) and an armed grab match the same item,
**neither** listener should steal the bytes. That is strictly safer than grab winning
(case B) when the researcher was driving acquisition.

Secondary structural improvement: **#2** to close window **C** for papio-initiated
downloads.

---

## References (file:line, current tree)

- Listeners bound: `9738-9781`
- Grab/job helpers: `9673-9721`
- Click adapter effect: `15907-15938`
- `pendingDownloadURLs` comment (exact adapter steering): `2146-2149`
- `sameDownloadRoute` / `downloadRoute`: `1467-1497`
- Grab arm + route pending: `8673-8685`
- Firefox correlate disabled: `16229-16230`
- Pre-fix ordering evidence: `30444d5^` line `15983` via git command above
