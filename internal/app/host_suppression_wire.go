// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"database/sql"
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
		s.suppressionLoaded = false
		s.suppressionCounted = nil
		return
	}
	s.suppression = hostsuppress.New(cfg)
	s.suppressionLoaded = false
	s.suppressionCounted = nil
}

// scanSuppressionRow reads one startup-rebuild row. It is a variable only so
// a test can fail the rebuild part-way through a scan, the one failure the
// store cannot produce on demand.
var scanSuppressionRow = func(rows *sql.Rows, dest ...any) error { return rows.Scan(dest...) }

// hostSuppression returns the live tracker, running the bounded startup
// rebuild once (R6): one query over fetch-stage rows inside the window, fed
// through the same Observe path live attempts use. Unknown-kind legacy rows
// never count. A failed rebuild leaves the tracker empty and live attempts
// unfolded (both suppress nothing, the safe direction); the next pass retries
// rebuild, which then counts every committed row exactly once. The rebuild
// records the SET of finished attempt ids it counted; a live fold for an id
// in that set is the same committed row and is skipped. Ids, not a maximum,
// are tracked because attempts FINISH out of order: 10 can start before 11
// while 11 finishes first, and a watermark at 11 would drop 10 silently.
func (s *Service) hostSuppression(ctx context.Context) *hostsuppress.Tracker {
	t := s.suppression
	if t == nil {
		return nil
	}
	s.suppressionMu.Lock()
	defer s.suppressionMu.Unlock()
	s.rebuildSuppressionLocked(ctx)
	return t
}

// rebuildSuppressionLocked runs the one-time startup rebuild. The caller must
// hold suppressionMu, which it does across the query as before, so a live
// fold under the same mutex cannot interleave between the read and the set
// write. The set stays bounded: one entry per finished fetch attempt inside
// the rebuild window, shrinking as live folds consume their own ids, and the
// rebuild never runs again.
func (s *Service) rebuildSuppressionLocked(ctx context.Context) {
	t := s.suppression
	if t == nil || s.suppressionLoaded {
		return
	}
	since := s.Now().UTC().Add(-t.Window())
	rows, err := s.Jobs.S.DB().QueryContext(ctx,
		`SELECT id, COALESCE(outcome,''), COALESCE(detail,''), ended_at FROM attempts
		  WHERE stage = 'fetch' AND ended_at IS NOT NULL AND ended_at >= ?
		  ORDER BY ended_at ASC, id ASC`, store.FormatTime(since))
	if err != nil {
		return
	}
	// Collect the whole window before observing any of it. A scan or
	// iteration error part-way through must leave the tracker untouched:
	// the next pass retries the rebuild from the start, and rows already
	// folded in would then count twice, so duplicated historical 403s could
	// suppress a host that never earned it.
	var history []hostsuppress.Row
	var ids []int64
	for rows.Next() {
		var id int64
		var outcome, detail, ended string
		if err := scanSuppressionRow(rows, &id, &outcome, &detail, &ended); err != nil {
			_ = rows.Close()
			return
		}
		at, err := time.Parse(time.RFC3339Nano, ended)
		if err != nil {
			continue
		}
		history = append(history, hostsuppress.Row{Outcome: outcome, Detail: detail, At: at.UTC()})
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return
	}
	for _, row := range history {
		t.Observe(row)
	}
	counted := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		counted[id] = struct{}{}
	}
	s.suppressionCounted = counted
	s.suppressionLoaded = true
}

// observeFetchAttempt folds a just-recorded fetch attempt into the live
// rule, from the same outcome and detail the store now holds. attemptID is
// the FinishAttempt row the call just committed, in production order. The
// check and the fold hold suppressionMu with the rebuild, so a rebuild that
// counted the row between the commit and this call recorded the id first and
// the fold is skipped: one committed row counts once. While the rebuild
// has not loaded, nothing is folded live; the committed row is already
// visible to the next successful rebuild.
func (s *Service) observeFetchAttempt(ctx context.Context, attemptID int64, outcome, detail string) {
	t := s.suppression
	if t == nil {
		return
	}
	s.suppressionMu.Lock()
	defer s.suppressionMu.Unlock()
	if !s.suppressionLoaded {
		s.rebuildSuppressionLocked(ctx)
		if !s.suppressionLoaded {
			return
		}
	}
	if attemptID != 0 {
		if _, dup := s.suppressionCounted[attemptID]; dup {
			delete(s.suppressionCounted, attemptID)
			return
		}
	}
	t.Observe(hostsuppress.Row{Outcome: outcome, Detail: detail, At: s.Now().UTC()})
}

// probePass is one fetch attempt's probe state, carried into the
// downloader's per-hop gate through the fetch context. hosts are the hosts
// this pass holds a probe for (claimed on a candidate or on a redirect
// target), shared across the pass; probed reports whether this attempt
// touched one of them, so its row carries the probe marker; admitted
// reports whether the gate let any hop of this attempt through, so a
// refusal with admitted false means no request was sent at all. One pass
// runs on one goroutine: no locking.
type probePass struct {
	hosts    map[string]bool
	probed   bool
	admitted bool
}

type probePassKey struct{}

// HostGate is the per-hop downloader gate (R3): before each request hop,
// including every redirect target, it refuses a host under suppression. A
// host this pass already probes is let through; a due probe on a redirect
// target is claimed for the pass, so the job's later candidates on that host
// join it and the attempt is marked. Excluded hosts and pure redirectors
// never refuse. A host inside its 429 backoff is deferred on every hop
// (O3), never suppressed. With the rule off it admits all.
func (s *Service) HostGate(ctx context.Context, host string) error {
	t := s.hostSuppression(ctx)
	if t == nil {
		return nil
	}
	host = strings.ToLower(host)
	now := s.Now().UTC()
	// A 429 backoff outranks everything, a probe this pass holds included:
	// the host asked papio to slow down, and a redirect into it inside the
	// cooldown is exactly the request it asked papio not to send.
	if until, ok := t.BackedOff(host, now); ok {
		return fetch.BackedOff(host, until.Sub(now))
	}
	pass, _ := ctx.Value(probePassKey{}).(*probePass)
	if pass != nil && pass.hosts[host] {
		pass.probed = true
		pass.admitted = true
		return nil
	}
	switch decision, _ := t.Claim(host, now); decision {
	case hostsuppress.Skip:
		return fetch.Suppressed(host)
	case hostsuppress.Probe:
		if pass != nil {
			pass.hosts[host] = true
			pass.probed = true
		}
	}
	if pass != nil {
		pass.admitted = true
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
// and whose refused host is owed its probe. The refused host is the one the
// skip recorded (`suppressed=` in the attempt detail), not the candidate's
// own host: a DOI-redirected candidate is refused at the publisher it
// redirects into, and keying on the candidate host would never revive it.
// Rows skipped for any other reason
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
		SELECT c.id, c.url_redacted, c.url_key, COALESCE(a.detail,'') FROM candidates c
		  JOIN attempts a ON a.id = (SELECT MAX(a2.id) FROM attempts a2
		                              WHERE a2.candidate_id = c.id AND a2.stage = 'fetch')
		 WHERE c.job_id = ? AND c.status = 'skipped' AND a.outcome = 'suppressed'
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
		var redacted, key, detail string
		if err := rows.Scan(&row.id, &redacted, &key, &detail); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := live[key]; !ok {
			continue
		}
		row.host = suppressedHostOf(detail)
		if row.host == "" {
			row.host = candidateHost(redacted)
		}
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

// suppressedHostOf reads the refused host back out of a suppressed
// attempt's detail (the `suppressed=` field suppressedDetail writes), or "".
func suppressedHostOf(detail string) string {
	for _, field := range strings.Fields(detail) {
		if host, ok := strings.CutPrefix(field, "suppressed="); ok {
			return strings.ToLower(host)
		}
	}
	return ""
}
