// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/hostsuppress"
	"papio/internal/resolver"
	"papio/internal/store"
)

// Host-aware daemon fetch suppression (ADR-0031). The rule itself is the
// pure internal/hostsuppress.Tracker; this file is its I/O: the bounded
// startup rebuild from attempts.detail (R6), the per-hop downloader gate
// (R3), the probe marker (R4), and the exhaustion summary (R10). The skip
// itself is recorded in fetchCandidates (R7, R8, R9).

// EnableHostSuppression wires the [host_suppression] section. A disabled
// section leaves the rule off: fetchCandidates then fetches everything.
// Production calls this from bootstrap; tests call it with their own config.
func (s *Service) EnableHostSuppression(cfg config.HostSuppression) {
	if !cfg.Enabled {
		s.suppression = nil
		return
	}
	s.suppression = hostsuppress.New(cfg)
}

// hostSuppression returns the live tracker, running the bounded startup
// rebuild once (R6): one query over fetch-stage rows inside the window, fed
// through the same Observe path live attempts use. Unknown-kind legacy rows
// never count. A failed rebuild leaves the tracker empty (suppresses
// nothing, the safe direction) and is retried on the next pass.
func (s *Service) hostSuppression(ctx context.Context) *hostsuppress.Tracker {
	t := s.suppression
	if t == nil {
		return nil
	}
	s.suppressionMu.Lock()
	defer s.suppressionMu.Unlock()
	if s.suppressionLoaded {
		return t
	}
	since := s.Now().UTC().Add(-t.Window())
	rows, err := s.Jobs.S.DB().QueryContext(ctx,
		`SELECT COALESCE(outcome,''), COALESCE(detail,''), ended_at FROM attempts
		  WHERE stage = 'fetch' AND ended_at IS NOT NULL AND ended_at >= ?
		  ORDER BY ended_at ASC, id ASC`, store.FormatTime(since))
	if err != nil {
		return t
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var outcome, detail, ended string
		if err := rows.Scan(&outcome, &detail, &ended); err != nil {
			return t
		}
		at, err := time.Parse(time.RFC3339Nano, ended)
		if err != nil {
			continue
		}
		t.Observe(hostsuppress.Row{Outcome: outcome, Detail: detail, At: at.UTC()})
	}
	if rows.Err() == nil {
		s.suppressionLoaded = true
	}
	return t
}

// observeFetchAttempt folds a just-recorded fetch attempt into the live
// rule, from the same outcome and detail the store now holds.
func (s *Service) observeFetchAttempt(ctx context.Context, outcome, detail string) {
	if t := s.hostSuppression(ctx); t != nil {
		t.Observe(hostsuppress.Row{Outcome: outcome, Detail: detail, At: s.Now().UTC()})
	}
}

// probePass is one fetch pass's probe state, carried into the downloader's
// per-hop gate through the fetch context. hosts are the hosts this pass
// holds a probe for (claimed on a candidate or on a redirect target); probed
// reports whether the attempt in flight touched one of them, so its row
// carries the probe marker. One pass runs on one goroutine: no locking.
type probePass struct {
	hosts  map[string]bool
	probed bool
}

type probePassKey struct{}

// HostGate is the per-hop downloader gate (R3): before each request hop,
// including every redirect target, it refuses a host under suppression. A
// host this pass already probes is let through; a due probe on a redirect
// target is claimed for the pass, so the job's later candidates on that host
// join it and the attempt is marked. Excluded hosts and pure redirectors
// never refuse. With the rule off it admits all.
func (s *Service) HostGate(ctx context.Context, host string) error {
	t := s.hostSuppression(ctx)
	if t == nil {
		return nil
	}
	host = strings.ToLower(host)
	pass, _ := ctx.Value(probePassKey{}).(*probePass)
	if pass != nil && pass.hosts[host] {
		pass.probed = true
		return nil
	}
	switch decision, _ := t.Claim(host, s.Now().UTC()); decision {
	case hostsuppress.Skip:
		return fetch.Suppressed(host)
	case hostsuppress.Probe:
		if pass != nil {
			pass.hosts[host] = true
			pass.probed = true
		}
	}
	return nil
}

// suppressionSummary counts one pass's skips per host for the R10
// exhaustion detail. It is pass-local: concurrent workers never share it.
type suppressionSummary map[string]int

func (m suppressionSummary) add(host string) {
	if host != "" {
		m[host]++
	}
}

// merge copies the summary into a transition detail map under
// "suppressed_hosts" (hosts and counts, sorted keys in JSON). Detail-map keys
// are additive-safe; no IPC struct field changes.
func (m suppressionSummary) merge(detail map[string]any) map[string]any {
	if len(m) == 0 {
		return detail
	}
	if detail == nil {
		detail = map[string]any{}
	}
	hosts := make([]string, 0, len(m))
	for host := range m {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	entries := make([]map[string]any, 0, len(hosts))
	for _, host := range hosts {
		entries = append(entries, map[string]any{"host": host, "kind": "wall", "count": m[host]})
	}
	detail["suppressed_hosts"] = entries
	return detail
}

// suppressionSummaryKey carries a pass's summary to exhaustedCandidates,
// which builds its transition detail maps in several branches.
type suppressionSummaryKey struct{}

func withSuppressionSummary(ctx context.Context, m suppressionSummary) context.Context {
	return context.WithValue(ctx, suppressionSummaryKey{}, m)
}

// withSuppressionDetail merges the pass summary carried by ctx, if any.
func withSuppressionDetail(ctx context.Context, detail map[string]any) map[string]any {
	m, _ := ctx.Value(suppressionSummaryKey{}).(suppressionSummary)
	return m.merge(detail)
}

// suppressedDetail renders the attempt detail of a skipped fetch: kind
// "wall" (the pooled challenge + 403 refusal), the candidate host, and the
// suppressed host that refused (the candidate host itself, or a redirect
// target the per-hop gate refused).
func suppressedDetail(rawURL, suppressedHost string) string {
	detail := "*fetch.Error(wall)"
	if host := candidateHost(rawURL); host != "" {
		detail += " host=" + host
	}
	return detail + " suppressed=" + suppressedHost
}

// withProbeMarker tags the detail of an attempt that ran as a probe.
func withProbeMarker(detail string, probe bool) string {
	if !probe {
		return detail
	}
	return detail + " " + hostsuppress.ProbeMarker
}

// candidateHost is the lowercased hostname of a candidate URL, or "".
func candidateHost(rawURL string) string {
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	if i := strings.Index(lower, "://"); i >= 0 {
		lower = lower[i+3:]
	}
	if i := strings.IndexAny(lower, "/?#"); i >= 0 {
		lower = lower[:i]
	}
	if i := strings.LastIndex(lower, "@"); i >= 0 {
		lower = lower[i+1:]
	}
	if strings.HasPrefix(lower, "[") {
		if i := strings.Index(lower, "]"); i >= 0 {
			return lower[1:i]
		}
	}
	if i := strings.Index(lower, ":"); i >= 0 {
		lower = lower[:i]
	}
	return lower
}

// suppressionVerdict decides a candidate's fetch before cost reservation or
// budget admission: skip it (R8), let it run as the host's probe (R4), or
// defer it inside a 429 backoff (O3, never a suppression). A probe is one
// pass: once a host's probe is admitted, every other candidate of the same
// job on that host in the same pass runs as part of it (passProbes), because
// the operator store showed a wall host serving one source's URL a second
// after refusing a sibling source's URL for the same work.
type suppressionVerdict struct {
	skip         bool
	probe        bool
	host         string
	backoffUntil time.Time
	// claim is set when this verdict itself claimed the host's probe; a
	// caller that refuses the fetch before any request must release it.
	claim   hostsuppress.ProbeClaim
	claimed bool
}

func (s *Service) candidateSuppression(ctx context.Context, candidate resolver.Candidate, passProbes map[string]bool) suppressionVerdict {
	t := s.hostSuppression(ctx)
	if t == nil {
		return suppressionVerdict{}
	}
	host := candidateHost(candidate.URL)
	if host == "" {
		return suppressionVerdict{}
	}
	now := s.Now().UTC()
	if until, ok := t.BackedOff(host, now); ok {
		return suppressionVerdict{host: host, backoffUntil: until}
	}
	if passProbes[host] {
		return suppressionVerdict{probe: true, host: host}
	}
	switch decision, claim := t.Claim(host, now); decision {
	case hostsuppress.Skip:
		return suppressionVerdict{skip: true, host: host}
	case hostsuppress.Probe:
		passProbes[host] = true
		return suppressionVerdict{probe: true, host: host, claim: claim, claimed: true}
	}
	return suppressionVerdict{host: host}
}

// releaseUnspentProbe hands back a probe this verdict claimed when the fetch
// is refused before any request (cost ceiling, budget gate, local error), so
// the host is not left shut for a whole interval with nothing revivable.
func (s *Service) releaseUnspentProbe(v suppressionVerdict, passProbes map[string]bool) {
	if !v.claimed || s.suppression == nil {
		return
	}
	s.suppression.Release(v.claim)
	delete(passProbes, v.host)
}

// observeRateLimit starts the per-host 429 backoff after a 429 answer.
func (s *Service) observeRateLimit(ctx context.Context, err error) {
	t := s.hostSuppression(ctx)
	if t == nil {
		return
	}
	var fe *fetch.Error
	if !errors.As(err, &fe) || fe.HTTPStatus != 429 || fe.FinalHost == "" {
		return
	}
	t.Observe429(fe.FinalHost, fe.RetryAfter, s.Now().UTC())
}

// reviveSuppressionProbe hands a due probe something to fetch (R9). A
// suppressed candidate row is `skipped`, which ResetCandidates never
// revives, and a re-resolved identical URL collides with it under
// candidates_dedupe, so without this the probe rule would have no row to
// run. Before the pending-queue loop, it resets at most one row per host to
// pending: a row of this job whose latest fetch attempt was `suppressed`
// and whose host is owed its probe. Rows skipped for any other reason
// (landing pages, cost ceilings, stale bearer URLs) are never touched. The
// row then meets candidateSuppression, which claims it as the probe. Only a
// row this pass can fetch is revived: its url_key must be in live. A stale
// row the resolvers no longer return would be skipped again unfetched and
// would shadow the live row on the same host.
func (s *Service) reviveSuppressionProbe(ctx context.Context, jobID string, live map[string]resolver.Candidate) error {
	t := s.hostSuppression(ctx)
	if t == nil {
		return nil
	}
	rows, err := s.Jobs.S.DB().QueryContext(ctx, `
		SELECT c.id, c.url_redacted, c.url_key FROM candidates c
		 WHERE c.job_id = ? AND c.status = 'skipped'
		   AND (SELECT a.outcome FROM attempts a
		         WHERE a.candidate_id = c.id AND a.stage = 'fetch'
		         ORDER BY a.id DESC LIMIT 1) = 'suppressed'
		 ORDER BY c.rank, c.id`, jobID)
	if err != nil {
		return err
	}
	type skippedRow struct {
		id   int64
		host string
	}
	var skipped []skippedRow
	for rows.Next() {
		var row skippedRow
		var redacted, key string
		if err := rows.Scan(&row.id, &redacted, &key); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := live[key]; !ok {
			continue
		}
		row.host = candidateHost(redacted)
		skipped = append(skipped, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	now := s.Now().UTC()
	revived := map[string]bool{}
	for _, row := range skipped {
		if row.host == "" || revived[row.host] || t.Decide(row.host, now) != hostsuppress.Probe {
			continue
		}
		if err := s.Jobs.MarkCandidate(ctx, row.id, "pending"); err != nil {
			return err
		}
		revived[row.host] = true
	}
	return nil
}
