// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package hostsuppress holds the host-aware daemon fetch suppression rule
// (ADR-0031). It is pure bookkeeping over fetch-stage attempt rows: the app
// feeds it the same attempts.detail strings it persists, asks it whether a
// host's next daemon fetch should run, be skipped, or run as the revival
// probe, and paces 429s per host. It never suppresses a candidate, only the
// daemon's wire attempt; the app keeps every candidate on the browser and
// institutional routes. No I/O and no schema: the durable memory is the
// attempts table, replayed through Observe on startup.
package hostsuppress

import (
	"strings"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
)

// Row is one fetch-stage attempt as the store records it.
type Row struct {
	Outcome string    // attempts.outcome
	Detail  string    // attempts.detail
	At      time.Time // attempts.ended_at
}

// Decision is the verdict for a host's next daemon fetch.
type Decision int

const (
	// Fetch lets the attempt run normally.
	Fetch Decision = iota
	// Skip suppresses the attempt: no request is made.
	Skip
	// Probe lets the attempt run as the host's revival probe. Claim grants
	// it to exactly one caller per interval; Decide only reports it.
	Probe
)

// ProbeClaim identifies one granted probe, so a caller that cannot spend it
// (a cost or budget refusal before any request) can hand it back.
type ProbeClaim struct {
	host string
	at   time.Time
	prev time.Time
}

// ProbeMarker is appended to the detail of an attempt that ran as a probe,
// so a revival attempt is distinguishable from an ordinary fetch in the
// store. ParsePersistedDetail ignores unknown fields.
const ProbeMarker = "probe"

// Pooled wall kinds: a challenge and a plain 403 are one access-control
// refusal, so they count toward one threshold (ADR-0031 R11).
func wallKind(kind string) bool {
	return kind == fetch.FetchKindChallenge || kind == "http-403"
}

// rateLimitBase is the first per-host 429 backoff when the response names no
// Retry-After; it doubles per consecutive 429 up to the configured cap.
const rateLimitBase = time.Minute

type hostState struct {
	walls     []time.Time // wall rows this host answered, inside the window
	successes []time.Time // daemon fetch-stage successes this host answered
	// lastAttempt is the latest real wire attempt attributed to the host.
	lastAttempt time.Time
	// probeAdmitted is the latest probe admission, so a probe in flight holds
	// the host shut for every other candidate.
	probeAdmitted time.Time
	// redirectedAway counts rows where this host was the candidate and a
	// different host answered; responded counts rows where it answered.
	redirectedAway int
	responded      int
	// 429 pacing: never converts to suppression.
	backoffUntil   time.Time
	consecutive429 int
}

// Tracker is the live rule state. Safe for concurrent use.
type Tracker struct {
	mu      sync.Mutex
	cfg     config.HostSuppression
	exclude map[string]bool
	hosts   map[string]*hostState
}

// New builds a tracker for the configured rule.
func New(cfg config.HostSuppression) *Tracker {
	exclude := make(map[string]bool, len(cfg.ExcludedHosts))
	for _, host := range cfg.ExcludedHosts {
		if host = normalize(host); host != "" {
			exclude[host] = true
		}
	}
	return &Tracker{cfg: cfg, exclude: exclude, hosts: map[string]*hostState{}}
}

func normalize(host string) string { return strings.ToLower(strings.TrimSpace(host)) }

func (t *Tracker) host(name string) *hostState {
	st := t.hosts[name]
	if st == nil {
		st = &hostState{}
		t.hosts[name] = st
	}
	return st
}

func (t *Tracker) window() time.Duration {
	return time.Duration(t.cfg.WindowDays) * 24 * time.Hour
}

func (t *Tracker) probeInterval() time.Duration {
	return time.Duration(t.cfg.ProbeIntervalDays) * 24 * time.Hour
}

// Observe folds one fetch-stage attempt into the rule. Rows must arrive in
// time order (the startup rebuild sorts by ended_at). Legacy failure rows
// (bare "*fetch.Error", unknown kind) carry no host and are ignored;
// suppressed and cancelled rows, and hops deferred inside a 429 backoff,
// are not wire attempts and are ignored: counting one as an attempt would
// push the host's next probe a whole interval out for a request never sent.
// Success rows name the responding host; only they clear a wall. Validate
// accepts, browser accepts, and adoptions never reach this function.
func (t *Tracker) Observe(row Row) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch row.Outcome {
	case "suppressed", "cancelled", "":
		return
	case "success":
		host := successHost(row.Detail)
		if host == "" {
			return
		}
		st := t.host(host)
		st.successes = append(st.successes, row.At)
		st.responded++
		st.consecutive429 = 0
		st.backoffUntil = time.Time{}
		st.lastAttempt = later(st.lastAttempt, row.At)
		return
	}
	kind, candidate, final := fetch.ParsePersistedDetail(row.Detail)
	if kind == "" || kind == fetch.FetchKindBackoff {
		return
	}
	candidate, final = normalize(candidate), normalize(final)
	if candidate != "" && final != "" && final != candidate {
		t.host(candidate).redirectedAway++
	}
	responder := fetch.SuppressionHost(kind, candidate, final)
	if responder == "" {
		return
	}
	st := t.host(responder)
	st.lastAttempt = later(st.lastAttempt, row.At)
	if final != "" {
		st.responded++
		if kind != "http-429" {
			st.consecutive429 = 0
		}
	}
	if wallKind(kind) {
		st.walls = append(st.walls, row.At)
	}
}

// Observe429 paces a host after a 429: Retry-After when named, otherwise a
// doubling backoff from rateLimitBase, capped at the configured ceiling. It
// never writes a suppression; the host is only deferred.
func (t *Tracker) Observe429(host string, retryAfter time.Duration, now time.Time) {
	host = normalize(host)
	if host == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.host(host)
	st.consecutive429++
	delay := rateLimitBase << min(st.consecutive429-1, 20)
	if retryAfter > delay {
		delay = retryAfter
	}
	if ceiling := time.Duration(t.cfg.RateLimitBackoffCapSecs) * time.Second; ceiling > 0 && delay > ceiling {
		delay = ceiling
	}
	st.backoffUntil = later(st.backoffUntil, now.Add(delay))
}

// BackedOff reports whether the host is inside its 429 backoff and until
// when. Always false while the rule is disabled.
func (t *Tracker) BackedOff(host string, now time.Time) (time.Time, bool) {
	if !t.cfg.Enabled {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.hosts[normalize(host)]
	if st == nil || !now.Before(st.backoffUntil) {
		return time.Time{}, false
	}
	return st.backoffUntil, true
}

// Decide reports the verdict for the host's next daemon fetch without
// claiming anything. Use it to ask; use Claim to act.
func (t *Tracker) Decide(host string, now time.Time) Decision {
	host = normalize(host)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.decideLocked(host, now)
}

// Claim decides and, when the host's probe is due, grants it to this caller
// in the same locked step, so of any number of concurrent callers exactly
// one receives Probe; the rest receive Skip until the interval elapses again
// or the claim is released.
func (t *Tracker) Claim(host string, now time.Time) (Decision, ProbeClaim) {
	host = normalize(host)
	t.mu.Lock()
	defer t.mu.Unlock()
	decision := t.decideLocked(host, now)
	if decision != Probe {
		return decision, ProbeClaim{}
	}
	st := t.host(host)
	claim := ProbeClaim{host: host, at: now, prev: st.probeAdmitted}
	st.probeAdmitted = now
	return Probe, claim
}

// Release hands back a probe the caller never spent. A claim superseded by a
// newer one is ignored, so a late release never frees someone else's probe.
func (t *Tracker) Release(claim ProbeClaim) {
	if claim.host == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.hosts[claim.host]; st != nil && st.probeAdmitted.Equal(claim.at) {
		st.probeAdmitted = claim.prev
	}
}

func (t *Tracker) decideLocked(host string, now time.Time) Decision {
	if !t.cfg.Enabled || host == "" || t.exclude[host] {
		return Fetch
	}
	st := t.hosts[host]
	if st == nil {
		return Fetch
	}
	cutoff := now.Add(-t.window())
	st.walls = prune(st.walls, cutoff)
	st.successes = prune(st.successes, cutoff)
	if len(st.successes) > 0 || len(st.walls) < t.cfg.FailureThreshold {
		return Fetch
	}
	// A persistent wall spans time; a burst of refusals inside one span
	// shorter than MinWallSpanHours is an intermittent host, which the
	// operator store showed serving minutes after such a burst.
	if span := time.Duration(t.cfg.MinWallSpanHours) * time.Hour; span > 0 && wallSpan(st.walls) < span {
		return Fetch
	}
	// A pure redirector (it only ever handed the request to another host)
	// never suppresses: its walls, if any, belong to whoever answered.
	if st.redirectedAway > 0 && st.responded == 0 {
		return Fetch
	}
	base := later(st.lastAttempt, st.probeAdmitted)
	if !now.Before(base.Add(t.probeInterval())) {
		return Probe
	}
	return Skip
}

// Excluded reports whether the host is on the never-suppress list.
func (t *Tracker) Excluded(host string) bool { return t.exclude[normalize(host)] }

// Enabled reports whether the rule is on.
func (t *Tracker) Enabled() bool { return t.cfg.Enabled }

// Window is the trailing window the startup rebuild must cover.
func (t *Tracker) Window() time.Duration { return t.window() }

// wallSpan is the time between the earliest and latest wall, independent of
// arrival order.
func wallSpan(times []time.Time) time.Duration {
	if len(times) == 0 {
		return 0
	}
	first, last := times[0], times[0]
	for _, at := range times[1:] {
		if at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	return last.Sub(first)
}

func prune(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, at := range times {
		if !at.Before(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// successHost extracts the responding host from a success detail
// ("bytes=.. mime=.. host=<h>").
func successHost(detail string) string {
	for _, field := range strings.Fields(detail) {
		if host, ok := strings.CutPrefix(field, "host="); ok {
			return normalize(host)
		}
	}
	return ""
}
