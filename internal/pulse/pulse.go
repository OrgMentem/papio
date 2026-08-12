// Copyright 2026 OrgMentem. Licensed under MIT.

// Package pulse projects daemon-owned liveness facts into the bounded,
// solicited work-pulse read model. It deliberately does not infer progress
// from activity prose or from broad job inventories.
package pulse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"papio/internal/batch"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/triage"
)

// Snapshot is the wire response without a caller-owned request identifier.
// Keeping the protocol shape here prevents the API and browser bridge from
// growing a second liveness vocabulary.
type Snapshot = protocol.WorkPulseResponsePayload

// DeliveryReader is reserved for store-backed delivery projections. The
// current delivery authority is read from the shared store transactionally;
// this marker keeps the service boundary open for the effect governor without
// coupling pulse to delivery's mutable request type.
type DeliveryReader interface{}
type deliveryReader = DeliveryReader

// SessionProbe is an optional daemon-owned capacity source. Implementations
// may expose either of the optional methods below; absence leaves that
// measurement Unknown rather than fabricating zero.
type SessionProbe interface{}

type effectCapacityReader interface {
	EffectCapacity(context.Context) (busy, limit, waiting int, err error)
}
type humanSurfaceCapacityReader interface {
	HumanSurfaceCapacity(context.Context) (busy, limit, waitingClaims int, err error)
}

// Service reads the authoritative stores used by the pulse projection.
type Service struct {
	Jobs           *job.Store
	Cohorts        *batch.Cohorts
	Delivery       deliveryReader
	EffectLimit    int
	BrowserSession SessionProbe
	Now            func() time.Time
}

const (
	maxCount       = int64(1_000_000)
	maxGates       = 16
	terminalStates = "('ready','imported','unavailable','failed','cancelled')"
)

func nowFor(s *Service) time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func ptr(v int64) *int64   { return &v }
func boolPtr(v bool) *bool { return &v }

func parseTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	return t, err == nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Read produces one bounded snapshot. A failed authority read returns the
// partially initialised snapshot together with the read error; callers must
// not turn that error into zero-valued liveness.
func (s *Service) Read(ctx context.Context) (result Snapshot, retErr error) {
	defer func() {
		if retErr != nil {
			result.ProjectionComplete = boolPtr(false)
		}
	}()
	now := nowFor(s)
	snap := Snapshot{Schema: 1, GeneratedAt: stamp(now)}
	if s == nil || s.Jobs == nil || s.Jobs.S == nil || s.Jobs.S.DB() == nil {
		return snap, errors.New("pulse job authority is unavailable")
	}
	db := s.Jobs.S.DB()

	rows, err := readNonterminalJobs(ctx, s.Jobs)
	if err != nil {
		return snap, err
	}
	actions, err := s.Jobs.ListHumanActions(ctx, true)
	if err != nil {
		return snap, err
	}
	attention, err := s.Jobs.CurrentHumanAttention(ctx)
	if err != nil {
		return snap, err
	}
	actionAuth, err := readActionAuth(ctx, db)
	if err != nil {
		return snap, err
	}
	deliveryStates, err := readDeliveryStates(ctx, db)
	if err != nil {
		return snap, err
	}
	gates, err := readSourceGates(ctx, db, now)
	if err != nil {
		return snap, err
	}
	jobSourceGates, err := readJobSourceGates(ctx, db, now)
	if err != nil {
		return snap, err
	}
	delivery, err := readDeliverySchedules(ctx, db, now)
	if err != nil {
		return snap, err
	}
	claims, err := readActiveClaims(ctx, db, now)
	if err != nil {
		return snap, err
	}
	grabWaiting, err := readJoblessPDFGrabs(ctx, db)
	if err != nil {
		return snap, err
	}

	openActions := make(map[string]int)
	for _, action := range actions {
		if action.JobID == "" {
			continue
		}
		auth := actionAuth[action.ID]
		attentionKind, mapped := triage.EffectiveAttention(action.Kind, auth, deliveryStates[action.JobID])
		if !mapped {
			// Unknown auth is not evidence of either actor.
			continue
		}
		if attentionKind == "required" {
			openActions[action.JobID]++
		}
	}
	gateJobs := make(map[string]bool)
	gateTurns := int64(0)
	for _, gate := range attention.Gates {
		member := false
		for _, id := range append(append([]string(nil), gate.DependentJobIDs...), gate.ClaimMemberJobIDs...) {
			if id != "" {
				gateJobs[id] = true
				member = true
			}
		}
		if member {
			gateTurns++
		}
	}
	activeClaimJobs := make(map[string]bool)
	for _, c := range claims {
		if c.JobID != "" {
			activeClaimJobs[c.JobID] = true
		}
	}
	var inFlight, scheduled, continuing, waiting, stalled int64
	projectionComplete := true
	var next *nextFact
	lastForward, lastFinished, err := readJobTimes(ctx, db)
	if err != nil {
		return snap, err
	}

	for _, row := range rows {
		if gateJobs[row.ID] {
			// Dependent siblings are represented by their one typed gate.
			continue
		}
		if sg, ok := jobSourceGates[row.ID]; ok && !row.LeaseActive(now) && !futureRetry(row, now) && (row.State == job.StateQueued || row.State == job.StateResolving || row.State == job.StateFetching || row.State == job.StateValidating) {
			scheduled++
			next = earlierNext(next, nextFact{at: sg.until, kind: "source_gate", source: sg.source, count: 1})
			continue
		}
		if activeClaimJobs[row.ID] && !row.LeaseActive(now) && !futureRetry(row, now) && openActions[row.ID] == 0 {
			// Institutional materialization is not an activity signal. Until a
			// current effect/operation authority exists, this item is unknown.
			projectionComplete = false
			continue
		}
		switch {
		case row.LeaseActive(now):
			inFlight++
		case futureRetry(row, now):
			scheduled++
			t := mustParseRetry(row.RetryAt)
			next = earlierNext(next, nextFact{at: t, kind: "retry", count: 1})
		case openActions[row.ID] > 0:
			waiting++
		case row.State == job.StateAwaitingHuman || row.State == job.StateNeedsReview:
			// A parked state without an effective action is an authority gap,
			// not proof that the researcher has no work.
			projectionComplete = false
		case row.State == job.StateQueued || row.State == job.StateResolving || row.State == job.StateFetching || row.State == job.StateValidating:
			continuing++
		default:
			projectionComplete = false
		}
		if d, ok := delivery[row.ID]; ok {
			if d.at.After(now) {
				next = earlierNext(next, nextFact{at: d.at, kind: d.kind, source: d.source, count: d.count})
			} else if !row.LeaseActive(now) && !futureRetry(row, now) && openActions[row.ID] == 0 {
				// A due poll with no worker is only stalled if a durable episode
				// names it. No such episode means an incomplete projection.
				projectionComplete = false
			}
		}
	}

	// Jobless PDF grabs are explicit turns, not daemon jobs.
	if grabWaiting > 0 {
		waiting += int64(grabWaiting)
	}
	if gateTurns > 0 {
		waiting += gateTurns
	}
	gateMemberCount := int64(len(gateJobs))

	// The five buckets are meaningful as a whole only when every item was
	// classified. Keep independently observed positive moving counts so a
	// caller can still truthfully render Moving during a partial projection.
	if projectionComplete {
		nonterminal := int64(len(rows)) - gateMemberCount + int64(grabWaiting) + gateTurns
		if nonterminal > maxCount || inFlight > maxCount || scheduled > maxCount || continuing > maxCount || waiting > maxCount || stalled > maxCount {
			return Snapshot{Schema: 1, GeneratedAt: stamp(now)}, errors.New("pulse counts exceed wire bound")
		}
		if inFlight+scheduled+continuing+waiting+stalled != nonterminal {
			return Snapshot{Schema: 1, GeneratedAt: stamp(now)}, errors.New("pulse bucket algebra is inconsistent")
		}
		snap.NonterminalTotal = ptr(nonterminal)
	}
	snap.ProjectionComplete = boolPtr(projectionComplete)
	if inFlight > 0 || projectionComplete {
		snap.InFlight = ptr(inFlight)
	}
	if scheduled > 0 || projectionComplete {
		snap.Scheduled = ptr(scheduled)
	}
	if waiting > 0 || projectionComplete {
		snap.WaitingRequired = ptr(waiting)
	}
	if continuing > 0 || projectionComplete {
		snap.Continuing = ptr(continuing)
	}
	if stalled > 0 || projectionComplete {
		snap.Stalled = ptr(stalled)
	}
	if lastForward != nil {
		snap.LastForwardAt = stamp(*lastForward)
	}
	if lastFinished != nil {
		snap.LastFinishedAt = stamp(*lastFinished)
	}
	if next != nil {
		count := next.count
		snap.NextAction = &protocol.WorkPulseNextAction{At: stamp(next.at), Kind: next.kind, Source: next.source, Count: &count}
	}

	if len(gates) > maxGates {
		sort.Slice(gates, func(i, j int) bool { return gates[i].source < gates[j].source })
		gates = gates[:maxGates]
		snap.GatesTruncated = boolPtr(true)
	} else if len(gates) > 0 {
		snap.GatesTruncated = boolPtr(false)
	}
	for _, g := range gates {
		snap.Gates = append(snap.Gates, protocol.WorkPulseGate{Kind: "source_budget", Source: g.source, Until: stamp(g.until), Count: g.count})
	}
	if len(gates) > 0 && snap.Gates == nil {
		snap.Gates = []protocol.WorkPulseGate{}
	}

	if r, ok := s.BrowserSession.(humanSurfaceCapacityReader); ok {
		b, l, w, e := r.HumanSurfaceCapacity(ctx)
		if e != nil {
			return snap, e
		}
		if b < 0 || l < 0 || w < 0 || b > l || int64(l) > maxCount || int64(b) > maxCount || int64(w) > maxCount {
			return snap, errors.New("invalid human surface capacity")
		}
		snap.HumanSurfaceCapacity = &protocol.WorkPulseHumanSurfaceCapacity{Busy: int64(b), Limit: int64(l), WaitingClaims: int64(w)}
	}

	if s.Cohorts != nil {
		p, e := s.Cohorts.Latest(ctx, now)
		if e != nil {
			return snap, e
		}
		if p != nil {
			snap.LatestBatch = latestBatch(p)
			if p.SettledAt != nil && (lastFinished == nil || p.SettledAt.After(*lastFinished)) {
				snap.LastFinishedAt = stamp(*p.SettledAt)
			}
		}
	}

	// Stall episodes are intentionally empty until the daemon's durable episode
	// authority is available. Elapsed time alone never creates Stalled.
	if b, _ := json.Marshal(snap); len(b) >= 32*1024 {
		return Snapshot{Schema: 1, GeneratedAt: stamp(now)}, errors.New("pulse response exceeds 32 KiB")
	}
	return snap, nil
}

func readNonterminalJobs(ctx context.Context, js *job.Store) ([]job.Row, error) {
	rows, err := js.S.DB().QueryContext(ctx, `SELECT id FROM jobs WHERE state NOT IN `+terminalStates+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]job.Row, 0, len(ids))
	for _, id := range ids {
		r, err := js.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}
func readActionAuth(ctx context.Context, db *sql.DB) (map[int64]*bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, requires_auth FROM human_actions WHERE status = 'open'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*bool)
	for rows.Next() {
		var id int64
		var auth sql.NullBool
		if err := rows.Scan(&id, &auth); err != nil {
			return nil, err
		}
		if auth.Valid {
			value := auth.Bool
			out[id] = &value
		} else {
			out[id] = nil
		}
	}
	return out, rows.Err()
}

func readDeliveryStates(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT job_id, state FROM delivery_requests ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var jobID, state string
		if err := rows.Scan(&jobID, &state); err != nil {
			return nil, err
		}
		out[jobID] = state
	}
	return out, rows.Err()
}

func readJobSourceGates(ctx context.Context, db *sql.DB, now time.Time) (map[string]sourceGate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.job_id, sb.source, MIN(sb.next_allowed_at)
		FROM candidates c
		JOIN source_budgets sb ON sb.source = c.source
		WHERE sb.next_allowed_at IS NOT NULL AND sb.next_allowed_at <> ''
		GROUP BY c.job_id, sb.source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]sourceGate)
	for rows.Next() {
		var jobID, source, raw string
		if err := rows.Scan(&jobID, &source, &raw); err != nil {
			return nil, err
		}
		until, ok := parseTime(raw)
		if !ok {
			return nil, fmt.Errorf("source gate %q has invalid next_allowed_at", source)
		}
		if !until.After(now) {
			continue
		}
		candidate := sourceGate{source: publicSource(source), until: until, count: 1}
		if old, exists := out[jobID]; !exists || until.Before(old.until) {
			out[jobID] = candidate
		}
	}
	return out, rows.Err()
}

func futureRetry(row job.Row, now time.Time) bool {
	t, ok := parseTime(row.RetryAt)
	return ok && t.After(now)
}
func mustParseRetry(raw string) time.Time { t, _ := parseTime(raw); return t }

type nextFact struct {
	at           time.Time
	kind, source string
	count        int64
}

func earlierNext(current *nextFact, candidate nextFact) *nextFact {
	if candidate.at.IsZero() {
		return current
	}
	if current == nil || candidate.at.Before(current.at) || (candidate.at.Equal(current.at) && candidate.kind < current.kind) {
		v := candidate
		return &v
	}
	return current
}

type sourceGate struct {
	source string
	until  time.Time
	count  int64
}

func readSourceGates(ctx context.Context, db *sql.DB, now time.Time) ([]sourceGate, error) {
	rows, err := db.QueryContext(ctx, `SELECT source, MIN(next_allowed_at), COUNT(*) FROM source_budgets WHERE next_allowed_at IS NOT NULL AND next_allowed_at <> '' GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceGate
	for rows.Next() {
		var source, raw string
		var count int64
		if err := rows.Scan(&source, &raw, &count); err != nil {
			return nil, err
		}
		t, ok := parseTime(raw)
		if !ok {
			return nil, fmt.Errorf("source gate %q has invalid next_allowed_at", source)
		}
		if t.After(now) {
			out = append(out, sourceGate{source: publicSource(source), until: t, count: clampCount(count)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].until.Equal(out[j].until) {
			return out[i].source < out[j].source
		}
		return out[i].until.Before(out[j].until)
	})
	return out, nil
}

func publicSource(source string) string {
	source = strings.TrimSpace(source)
	if i := strings.IndexAny(source, "/?#@\\"); i >= 0 {
		source = source[:i]
	}
	if len([]rune(source)) > 64 {
		source = string([]rune(source)[:64])
	}
	return source
}

type deliveryFact struct {
	at           time.Time
	kind, source string
	count        int64
}

func readDeliverySchedules(ctx context.Context, db *sql.DB, now time.Time) (map[string]deliveryFact, error) {
	rows, err := db.QueryContext(ctx, `SELECT job_id, next_check_at, provider FROM delivery_requests WHERE state IN ('submitted','pending') AND next_check_at IS NOT NULL AND next_check_at <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]deliveryFact)
	for rows.Next() {
		var id, raw, provider string
		if err := rows.Scan(&id, &raw, &provider); err != nil {
			return nil, err
		}
		t, ok := parseTime(raw)
		if !ok {
			return nil, fmt.Errorf("delivery request %q has invalid next_check_at", id)
		}
		if old, exists := out[id]; !exists || t.Before(old.at) {
			out[id] = deliveryFact{at: t, kind: "delivery_poll", source: publicSource(provider), count: 1}
		}
	}
	return out, rows.Err()
}

type claimFact struct{ JobID, Phase string }

func readActiveClaims(ctx context.Context, db *sql.DB, now time.Time) ([]claimFact, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.job_id, m.phase, COALESCE(m.lease_until,'') FROM materialization_claims m JOIN browser_candidates c ON c.id=m.candidate_id WHERE m.phase IN ('claimed','bound','route_issued','navigated')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimFact
	for rows.Next() {
		var id, phase, lease string
		if err := rows.Scan(&id, &phase, &lease); err != nil {
			return nil, err
		}
		if lease != "" {
			t, ok := parseTime(lease)
			if ok && t.Before(now) {
				continue
			}
		}
		out = append(out, claimFact{JobID: id, Phase: phase})
	}
	return out, rows.Err()
}

func readJoblessPDFGrabs(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pdf_grabs WHERE state='parked_no_identifier' AND (job_id IS NULL OR job_id='')`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func readJobTimes(ctx context.Context, db *sql.DB) (*time.Time, *time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT at, detail_json FROM events WHERE kind='job.transition' ORDER BY at`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var forward, finished *time.Time
	for rows.Next() {
		var raw, detail string
		if err := rows.Scan(&raw, &detail); err != nil {
			return nil, nil, err
		}
		t, ok := parseTime(raw)
		if !ok {
			continue
		}
		var d map[string]any
		if json.Unmarshal([]byte(detail), &d) != nil {
			continue
		}
		from, _ := d["from"].(string)
		to, _ := d["to"].(string)
		reason, _ := d["reason"].(string)
		if to == job.StateResolving && (reason == "crash_recovery" || from == job.StateFetching || from == job.StateValidating) {
			continue
		}
		if forward == nil || t.After(*forward) {
			v := t
			forward = &v
		}
		if job.Terminal(to) && (finished == nil || t.After(*finished)) {
			v := t
			finished = &v
		}
	}
	return forward, finished, rows.Err()
}

func latestBatch(p *batch.Projection) *protocol.WorkPulseLatestBatch {
	if p == nil {
		return nil
	}
	label := p.Label
	if len([]rune(label)) > 256 {
		label = string([]rune(label)[:256])
	}
	out := &protocol.WorkPulseLatestBatch{BatchID: p.BatchID, Label: label, StartedAt: stamp(p.StartedAt), Membership: p.Membership}
	if p.SettledAt != nil {
		out.SettledAt = stamp(*p.SettledAt)
	}
	out.ProjectionComplete = boolPtr(p.ProjectionComplete)
	if p.Membership != "partial" {
		out.Total, out.Settled, out.NonterminalTotal, out.InFlight, out.Scheduled, out.Continuing, out.WaitingRequired, out.Stalled, out.Unavailable = ints(p.Total), ints(p.Settled), ints(p.NonterminalTotal), ints(p.InFlight), ints(p.Scheduled), ints(p.Continuing), ints(p.WaitingRequired), ints(p.Stalled), ints(p.Unavailable)
	}
	return out
}
func ints(v *int) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}
func clampCount(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxCount {
		return maxCount
	}
	return v
}

// PrimaryLabel applies the plan's display precedence. It returns Unknown when
// the snapshot does not support an exact whole-work conclusion.
func PrimaryLabel(s Snapshot) string {
	fresh := s.ProjectionComplete != nil && *s.ProjectionComplete
	moving := (s.InFlight != nil && *s.InFlight > 0) || (s.Continuing != nil && *s.Continuing > 0)
	if moving {
		return "Moving"
	}
	if s.WaitingRequired != nil && *s.WaitingRequired > 0 && !moving {
		return "Waiting on you"
	}
	if !moving && s.WaitingRequired != nil && *s.WaitingRequired == 0 && s.Stalled != nil && *s.Stalled > 0 && len(s.StallEpisodes) > 0 {
		return "Stalled"
	}
	if fresh && !moving && (s.WaitingRequired == nil || *s.WaitingRequired == 0) && s.Stalled != nil && *s.Stalled == 0 && s.Scheduled != nil && *s.Scheduled > 0 {
		return "Scheduled"
	}
	if fresh && s.NonterminalTotal != nil && *s.NonterminalTotal == 0 {
		return "Idle"
	}
	return "Unknown"
}
