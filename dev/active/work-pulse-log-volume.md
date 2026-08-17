# Work-pulse log volume — diagnosis (2026-08-17)

Status: fixed (2026-08-17); see Implementation section below.

## Symptom

`~/.local/share/papio/daemon.log` appears to receive work-pulse-related lines
continuously. The operator report was roughly **two lines per second** on an
idle system.

## Live log measurement (not assumed)

Commands run against `~/.local/share/papio/daemon.log` on 2026-08-17:

```text
grep -c -i pulse ~/.local/share/papio/daemon.log   → 5062 (5064 after a 10s wait)
wc -l ~/.local/share/papio/daemon.log              → 11852 total lines
papio browser sessions                             → one holder (6769b091fa5e), last sync 1s ago
```

**10-second live sample (idle, inbox/popup not verified open):** pulse count did
not increase (`delta=0` over 10s). The tail showed pairs on the same second
roughly once per minute:

```text
2026/08/17 12:52:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
2026/08/17 12:52:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
…
2026/08/17 12:53:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
2026/08/17 12:53:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
```

### Verdict on ~2/s

| Claim | Result |
|-------|--------|
| Sustained **~2/s on idle right now** | **Refuted** — ~2 **lines per minute** (two identical lines on the same second), aligned with the 1-minute MV3 alarm. |
| **Up to 2 lines in one calendar second** | **Confirmed** — duplicate pair on each tick is the dominant pattern in recent tail. |
| Historical peak | **32** pulse lines in any **60s wall-clock** window (2026-08-16 11:43–11:44) → **~0.53/s average** in that minute, not 2/s. |
| Dense mixed-log windows | Up to **~1.8/s** when pulse lines are counted against the last 60 *distinct timestamp seconds* in a busy log tail (metric is easy to misread as 2/s). |

Timestamp histogram over all 5062 pulse lines (second-granularity log format):

- **1.0s** gap between consecutive pulse lines: 2401 occurrences (bursty periods, often with inbox/popup polling — not current idle).
- **60.0s** gaps dominate the 2026-08-17 tail (keepalive alarm).

**Exact log line (5060/5062 matches):**

```text
papio: work pulse unavailable: pulse bucket algebra is inconsistent
```

**Level:** Go standard library `log.Printf` in `Bridge.unavailable` — effectively
**info** (no level facility; always appended to `daemon.log`). Not debug, not
a structured metric.

Two older lines: `work pulse unavailable: unable to open database file (14)`.

## What work pulse is for (vs resolver keepalive)

These are **different** mechanisms:

| Mechanism | Purpose | Interval / trigger |
|-----------|---------|-------------------|
| **`work_pulse_v1`** | Daemon read model of nonterminal work (in-flight / scheduled / waiting / …) for **popup and inbox UI**. Extension sends `work_pulse_request`; daemon answers `work_pulse_response` or structured `pulse_unavailable`. | See triggers below. |
| **`KEEPALIVE_ALARM`** (`papio-keepalive`) | **MV3 service worker liveness** and daemon reconnect so queued `job_offer`s reach a dormant worker (`background.ts:277–281`, `4706–4711`). Also refreshes triage counts and, when negotiated, **one** `requestWorkPulse()` per wake (`9814–9818`). | `KEEPALIVE_ALARM_MINUTES = 1` (`background.ts:281`). |
| **`KeepaliveManager`** (`keepalive.ts`) | **Institutional resolver session** — reload/probe resolver tabs, `SessionVerdict` / `ProbeOutcome`, emit **`session_evidence`** to release parked handoffs (`background.ts:11148–11172`, `keepalive.ts:677–694`). | Default cycle **4 minutes** (`DEFAULT_INTERVAL_MINUTES = 4`, `keepalive.ts:203–204`); probes throttled (`MIN_PROBE_START_SPACING_MS = 10_000`, `MIN_FOREGROUND_PROBE_SPACING_MS = 2_000`). |

Reducing **`work_pulse`** poll frequency does **not** replace **`KEEPALIVE_ALARM`** or resolver keepalive; conflating them risks breaking offer delivery or session release if the wrong timer is slowed.

## Why the daemon logs (mechanism with file:line)

1. Extension → native host → `browser.sync` → `Bridge.workPulse` (`internal/browser/bridge.go:3718–3724`).
2. `pulse.Service.Read` (`internal/pulse/pulse.go`) builds bucket counts. When projection is complete but buckets do not sum to `nonterminal`, it returns **`pulse bucket algebra is inconsistent`** (`pulse.go:289–290`).
3. `workPulse` passes that error to `unavailable`, which **logs once per failed request** (`bridge.go:3613–3615`) and returns a wire-safe `pulse_unavailable` error frame (extension shows “temporarily unavailable”, not the algebra detail).

So the flood is **not** a healthy heartbeat being logged at info. It is **every UI poll hitting a deterministic read-model failure**, logged at info on each attempt.

### Extension send paths (not duplicate log sites)

Single log site: `bridge.go:3615`. Duplicate **lines** mean duplicate **`work_pulse_request`** frames (two calls in the same second), not two `Printf`s on one call.

| Trigger | Constant | Location | Approx rate when active |
|---------|----------|----------|-------------------------|
| MV3 keepalive alarm | `KEEPALIVE_ALARM_MINUTES = 1` | `background.ts:281`, `4709–4710`, `9818` | 1×/min (+ duplicate pair observed) |
| Inbox visible poll | `COUNTS_POLL_INTERVAL_MS = 15000` | `inbox.ts:2159`, `2228` | 1×/15s |
| Popup open refresh | `POPUP_REFRESH_INTERVAL_MS = 5_000` | `popup.ts:2611`, `3843` | 1×/5s |
| Runtime message | `papio.work.pulse` | `background.ts:17666–17674` | On demand from inbox/popup |

Native host idle poll is **`pollInterval = 2 * time.Second`** (`internal/nativehost/host.go:46`) for **daemon-initiated** frames only; it does **not** generate `work_pulse_request`.

### Ruling out other hypotheses

| Hypothesis | Evidence |
|------------|----------|
| Short timer sends pulse every ~500ms | **No** such constant on the work-pulse path; alarm is 1 min, inbox 15s, popup 5s. |
| One pulse logged twice (multiple log sites) | **No** — one `log.Printf` in `unavailable` (`bridge.go:3613–3615`). |
| Two browser sessions each pulsing | **No** today — `papio browser sessions` shows **one holder**, no pending poller. Pairs at the same second remain **two requests on one session** (e.g. alarm + surface poll overlap). |
| Wrong log level on success path | Failures only; successful `workPulse` does not log (`bridge.go:3733–3737`). |
| Operational heartbeat should be debug | N/A — current lines are **error-path** logging for a broken pulse projection. |

## Root cause (two layers)

1. **Log policy:** `unavailable` logs **every** `pulse_unavailable` at info with full cause (`bridge.go:3605–3615`), including repetitive algebra failures.
2. **Product bug:** `pulse.Read` returns **`pulse bucket algebra is inconsistent`** for this daemon’s job/grab state — fixing log level alone hides a real inconsistency that should be tracked separately (pulse workstream / triage).

## Daemon log rotation

**None.** Autostart opens `LogPath` with `O_CREATE|O_WRONLY|O_APPEND` only
(`internal/daemon/autostart.go:177–178`, `207–210`). Default path:
`dirname(socket)/daemon.log` (typically `~/.local/share/papio/daemon.log`).

Pulse lines are **~43%** of this file today (5062/11852). At idle failure rate
(~2 lines/min) that is manageable; with inbox + popup open and the same error,
order **~4–16 pulse log lines per minute** (15s + 5s polls, plus alarm), and
historical bursts reached **~32/min**. That **does** drown `daemon.log` as a
first-read diagnostic when operators tail during UI use — the exact failure mode
for “host died mid-session” debugging alongside `native-host.log`.

## Recommendations (ranked by risk to session liveness)

| Rank | Change | Liveness / behaviour risk | Notes |
|------|--------|---------------------------|-------|
| **1 (first choice)** | **Throttle or downgrade logging** for repeated identical `work pulse unavailable: …` (e.g. log at debug, or log once then “suppressed N identical errors in 5m” at info). | **None** — wire behaviour unchanged; UI still gets `pulse_unavailable`. | Smallest safe change; matches `unavailable`’s intent (“logged rather than sent”, `bridge.go:3611–3612`). |
| 2 | **Fix pulse bucket algebra** in `internal/pulse/pulse.go` (projection vs job/grab/permit state). | None for liveness; fixes UI pulse too. | Correct root cause; not a logging tweak. |
| 3 | Log summary on **state change only** (first failure + recovery). | None. | Slightly more code than (1). |
| 4 | **Reduce pulse request frequency** (inbox/popup intervals). | **Medium** — staler progress UI; does not replace MV3 alarm. | Do not slow `KEEPALIVE_ALARM` to “fix logs”; that risks offer latency and worker sleep (`background.ts:277–279`). |
| 5 | Deduplicate log sites | **N/A** — only one site. | No benefit. |

### Smallest safe change (explicit)

**Downgrade or rate-limit the `log.Printf` in `Bridge.unavailable` when
`surface == "work pulse"` and `cause` is stable** (same error string within a
window). Zero change to extension timers, `KEEPALIVE_ALARM`, or resolver
keepalive. Pair with a separate ticket to fix `pulse bucket algebra is
inconsistent`.

## Follow-up (out of scope here) — closed 2026-08-17

### Double `work_pulse_request` per minute (single holder)

**Live evidence checked 2026-08-17:**

```text
grep -c 'work_pulse_request' ~/.local/share/papio/native-host.log   → 0
grep 'work_pulse_request' ~/.local/share/papio/native-host.log | tail -20   → (no matches)
```

`native-host.log` records host lifecycle and stderr only (`internal/nativehost/host.go`); it does **not** log frame types. Pulse volume is visible in `daemon.log` via `Bridge.unavailable` (`bridge.go:3613–3615`).

Daemon tail (idle holder, inbox/popup not open):

```text
2026/08/17 13:12:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
2026/08/17 13:12:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
2026/08/17 13:13:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
2026/08/17 13:13:39 papio: work pulse unavailable: pulse bucket algebra is inconsistent
```

Analysis over the last 120 pulse lines: **median gap 60s** between unique seconds; **~93% of lines arrive in same-second pairs**. That cadence matches `KEEPALIVE_ALARM_MINUTES = 1`, not inbox (15s) or popup (5s).

| Hypothesis | Verdict |
|------------|---------|
| MV3 alarm + separate interval poll both pulsing on idle | **No** — interval is 60s, not 15s/5s; only `onKeepaliveAlarm` and `papio.work.pulse` call `requestWorkPulse()` in `background.ts`. |
| One alarm delivered twice (re-create on every worker start) | **Yes** — `start()` always called `alarms.create("papio-keepalive")` (`background.ts:4740–4742`), resetting a persisted alarm; Chrome can deliver the wake twice in the same second. |
| Two surfaces (inbox + popup) each polling | **No on idle** — would produce 5s/15s cadence, not 60s pairs; not correct-by-design for a single holder with no UI open. |
| State-change event + timer | **No** — `hello_ack` does not call `requestWorkPulse()`; runtime `papio.work.pulse` is inbox/popup-only. |

**Fix (extension, session liveness unchanged):**

1. `ensureKeepaliveAlarm()` — create the alarm only when `chrome.alarms.get` reports it missing (alarms persist across MV3 worker death; re-create was resetting the schedule).
2. `onKeepaliveAlarm()` dedupe — in-flight guard plus `KEEPALIVE_ALARM_DEDUPE_MS` (55s) so a duplicate same-minute callback cannot issue a second `work_pulse_request`.

Tests: `duplicate keepalive alarm delivery issues one work pulse request per interval` and `worker restart does not re-create an existing keepalive alarm` in `extension/test/background.test.ts`.

Remaining open item from the original follow-up: reproduce/fix any residual **`pulse bucket algebra is inconsistent`** product bug (Layer 2 above) — that is separate from duplicate request delivery.

## Implementation (2026-08-17)

**Layer 1 — log volume:** `Bridge.logUnavailable` rate-limits identical
`surface+cause` pairs: first failure logs at info; repeats within 5 minutes are
suppressed; when the window elapses, a summary line
`suppressed N identical errors in 5m` precedes the next info log. Wire behaviour
unchanged (`pulse_unavailable` still returned every request).

**Layer 2 — bucket algebra:** `pulse.Service.Read` subtracted
`len(gateJobs)` from `nonterminal_total` even when `gateJobs` included terminal
sibling ids from `CurrentHumanAttention` (ready/cancelled members still listed
on open human gates). Only nonterminal gate members are skipped in the bucket
loop, so three terminal ids in one live login gate made
`in_flight+…+stalled` exceed `nonterminal_total` by three. Fix: build `gateJobs`
and `gateTurns` only from ids present in the nonterminal job rows.
`TestReadTypedGateAlgebraIgnoresTerminalMembers` pins the shape.
