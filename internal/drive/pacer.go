// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package drive

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/notify"
)

// Principal names the pacer in the handoff.opened audit event. It is an
// opener, not an acquisition principal: the pacer never submits a request.
const Principal = job.Principal(job.PacerPrincipal)

// Event kinds the pacer writes.
const (
	// PacedOpenEvent is written on the job, BEFORE the open, so a failed
	// write can never let the next pass open the same paper again.
	PacedOpenEvent = "drive.paced_open"
	// OpenDeclinedEvent settles a paced open the browser did not take: no
	// live session, or a job the offer loop will not express.
	OpenDeclinedEvent = "drive.open_declined"
	// PausedEvent and ResumedEvent are system events; the newest of the two
	// is the pause state, so it survives a daemon restart.
	PausedEvent  = "drive.paused"
	ResumedEvent = "drive.resumed"
)

// Pause reasons.
const (
	PauseOperator = "operator"
	PauseSignIn   = "sign_in_needed"
)

// Blockers, in the order Status reports them.
const (
	BlockDisabled      = "disabled"
	BlockPaused        = "paused"
	BlockQuietHours    = "quiet_hours"
	BlockHolderAbsent  = "holder_absent"
	BlockRateLimit     = "rate_limit"
	BlockInFlight      = "in_flight"
	BlockBrowserBusy   = "browser_busy"
	BlockSignInPending = "sign_in_pending"
	BlockNoEligibleJob = "no_eligible_job"
)

// Reasons a parked action was passed over. They are counted, never listed, so
// the status view cannot become a reading-history dump.
const (
	SkipAdvisory       = "advisory_untracked"
	SkipNotAwaiting    = "not_awaiting_human"
	SkipBackoff        = "job_backoff"
	SkipHostCooldown   = "host_cooldown"
	SkipNotRedrivable  = "not_redrivable"
	SkipOtherKind      = "not_openable"
	SkipDuplicateOnJob = "duplicate_action"
)

// challengeCooldown mirrors the extension's worker-side cooldown after a
// Cloudflare-style check it could not pass. That frame carries no host, so the
// daemon keys it on the job and the job's DOI prefix instead.
const challengeCooldown = 10 * time.Minute

const (
	handoffKind        = "openurl_handoff"
	manualDownloadKind = "manual_download"
	advisoryKind       = "openurl_available"
)

// settleKinds are the browser's answers to a drive. Any of them after a paced
// open means that open no longer occupies the browser.
var settleKinds = map[string]bool{
	"browser.provider_outcome":            true,
	"browser.download_complete":           true,
	"browser.institutional_effect_result": true,
	"browser.job_reject":                  true,
	"browser.error":                       true,
	job.ProviderCooldownEvent:             true,
	OpenDeclinedEvent:                     true,
}

// Pacer is the paced drive: a maintenance runner that opens at most one parked
// handoff at a time, oldest first, through OpenHandoffs. See ADR-0009's
// 2026-09-23 amendment for the operator decision that authorizes it and the
// bounds it must keep.
//
// It never accepts terms, submits a delivery request, or resolves an identity
// review: it only asks the holder browser to surface a handoff, and the
// extension's own settings decide everything that happens on that page.
type Pacer struct {
	Jobs     *job.Store
	Config   config.Config
	Browser  Focuser
	Notifier notify.Sink
	Now      func() time.Time

	// mu serializes a pass against pause/resume so an operator's pause can
	// never interleave between the pass's checks and its open.
	mu sync.Mutex
}

// Status is the pacer's read model, served by drive.status.
type Status struct {
	Enabled         bool           `json:"enabled"`
	Paused          bool           `json:"paused"`
	PausedReason    string         `json:"paused_reason,omitempty"`
	PausedAt        string         `json:"paused_at,omitempty"`
	HolderConnected bool           `json:"holder_connected"`
	OpensLastHour   int            `json:"opens_last_hour"`
	MaxOpensPerHour int            `json:"max_opens_per_hour"`
	InFlight        *InFlight      `json:"in_flight,omitempty"`
	Next            *Candidate     `json:"next,omitempty"`
	Blockers        []string       `json:"blockers"`
	Skipped         map[string]int `json:"skipped"`
}

// InFlight is the paced open still occupying the browser.
type InFlight struct {
	JobID    string `json:"job_id"`
	Action   string `json:"action"`
	OpenedAt string `json:"opened_at"`
}

// Candidate is the next job the pacer would open.
type Candidate struct {
	JobID        string `json:"job_id"`
	ActionID     int64  `json:"action_id"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	WaitingSince string `json:"waiting_since"`
	revision     int64
}

// evaluation is Status plus the facts RunDue acts on.
type evaluation struct {
	Status
	signInStale *job.HumanGateObservation
}

func (p *Pacer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Status reports what the pacer would do on its next pass, without doing it.
func (p *Pacer) Status(ctx context.Context) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	eval, err := p.evaluate(ctx, p.now())
	return eval.Status, err
}

// Pause stops paced opens until Resume. An operator pause is never lifted by
// the pacer itself.
func (p *Pacer) Pause(ctx context.Context) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	paused, _, err := p.pauseState(ctx)
	if err != nil {
		return Status{}, err
	}
	if !paused {
		if err := p.Jobs.S.RecordSystemEvent(ctx, PausedEvent, map[string]any{
			"reason": PauseOperator, "paused_at": now.UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return Status{}, err
		}
	}
	eval, err := p.evaluate(ctx, now)
	return eval.Status, err
}

// Resume lifts any pause. A sign-in that still needs a person pauses the
// pacer again on its next pass, with a fresh notification.
func (p *Pacer) Resume(ctx context.Context) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	paused, reason, err := p.pauseState(ctx)
	if err != nil {
		return Status{}, err
	}
	if paused {
		if err := p.Jobs.S.RecordSystemEvent(ctx, ResumedEvent, map[string]any{"reason": PauseOperator, "paused_reason": reason}); err != nil {
			return Status{}, err
		}
	}
	eval, err := p.evaluate(ctx, now)
	return eval.Status, err
}

// RunDue is one pacer pass: at most one paced open.
func (p *Pacer) RunDue(ctx context.Context) error {
	if p == nil || p.Jobs == nil || !p.Config.Drive.Enabled {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	eval, err := p.evaluate(ctx, now)
	if err != nil {
		return err
	}
	switch {
	case eval.signInStale != nil && !eval.Paused:
		return p.pauseForSignIn(ctx, now, *eval.signInStale)
	case eval.signInStale == nil && eval.Paused && eval.PausedReason == PauseSignIn:
		if err := p.Jobs.S.RecordSystemEvent(ctx, ResumedEvent, map[string]any{"reason": "sign_in_cleared"}); err != nil {
			return err
		}
		if eval, err = p.evaluate(ctx, now); err != nil {
			return err
		}
	}
	if len(eval.Blockers) > 0 || eval.Next == nil {
		return nil
	}
	return p.open(ctx, now, *eval.Next)
}

func (p *Pacer) open(ctx context.Context, now time.Time, next Candidate) error {
	if err := p.Jobs.RecordEvent(ctx, next.JobID, PacedOpenEvent, map[string]any{
		"action_id": next.ActionID, "action": next.Action, "reason": next.Reason,
		"opened_at": now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}
	if next.Action == manualDownloadKind {
		if _, err := p.Jobs.RedriveInstitutionalHandoff(ctx, next.JobID, next.revision,
			p.Config.OpenURLBaseFor, isOAHandoff, false, app.InstitutionalOpenURLHandoffDetail); err != nil {
			// The check and the redrive share one precondition set, so this is
			// a race with the browser or the operator. The paced open event
			// already backs the job off; settle it so the browser is free.
			p.decline(ctx, next.JobID, "redrive_refused")
			if errors.Is(err, job.ErrConflict) {
				return nil
			}
			return err
		}
	}
	queued, live, err := OpenHandoffs(ctx, p.Browser, p.Jobs, []string{next.JobID}, Principal)
	switch {
	case err != nil:
		p.decline(ctx, next.JobID, "error")
		return err
	case !live:
		p.decline(ctx, next.JobID, "holder_absent")
	case queued == 0:
		p.decline(ctx, next.JobID, "not_offerable")
	}
	return nil
}

func (p *Pacer) decline(ctx context.Context, jobID, reason string) {
	if err := p.Jobs.RecordEvent(ctx, jobID, OpenDeclinedEvent, map[string]any{"reason": reason}); err != nil {
		log.Printf("papio: drive: recording declined open for %s: %v", jobID, err)
	}
}

func (p *Pacer) pauseForSignIn(ctx context.Context, now time.Time, gate job.HumanGateObservation) error {
	if err := p.Jobs.S.RecordSystemEvent(ctx, PausedEvent, map[string]any{
		"reason": PauseSignIn, "gate_type": string(gate.GateType), "paused_at": now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}
	if p.Notifier == nil {
		return nil
	}
	// The pause event is durable before routing, so a restart never repeats
	// the notice: the next pass reads the pause and does not come back here.
	message := "papio paused its paced drive: an institutional sign-in is waiting for you. Sign in; the drive resumes by itself (papio drive status)."
	window := 5 * time.Minute
	if policy, err := notify.ResolvePolicy(p.Config.Notify); err == nil {
		if configured := policy.For(notify.CategoryDecisionOpened).Window; configured > 0 {
			window = configured
		}
	}
	jobID := ""
	if members := append(append([]string(nil), gate.DependentJobIDs...), gate.ClaimMemberJobIDs...); len(members) > 0 {
		jobID = members[0]
	}
	intent := notify.Intent{
		EventKind: PausedEvent, Category: notify.CategoryDecisionOpened, Phase: notify.PhaseOpened,
		AggregateKey: "drive:paused:" + now.UTC().Format(time.RFC3339Nano),
		WindowStart:  now.UTC().Truncate(window), JobID: jobID, HappenedAt: now.UTC(),
		Message: message, Detail: notify.Event{Kind: PausedEvent, Message: message, Count: 1},
	}
	if err := p.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		log.Printf("papio: drive: routing the sign-in pause notice: %v", err)
	}
	return nil
}

func (p *Pacer) pauseState(ctx context.Context) (paused bool, reason string, err error) {
	latest, found, err := p.Jobs.LatestSystemEvent(ctx, PausedEvent, ResumedEvent)
	if err != nil || !found || latest.Kind != PausedEvent {
		return false, "", err
	}
	reason, _ = latest.Detail["reason"].(string)
	return true, reason, nil
}

// Paused reports whether the newest drive pause/resume event is a pause. The
// notification router's revalidation reads it so a held sign-in notice is not
// delivered after the pause already cleared.
func Paused(ctx context.Context, jobs *job.Store) (bool, error) {
	latest, found, err := jobs.LatestSystemEvent(ctx, PausedEvent, ResumedEvent)
	return found && latest.Kind == PausedEvent, err
}

func (p *Pacer) evaluate(ctx context.Context, now time.Time) (evaluation, error) {
	cfg := p.Config.Drive
	eval := evaluation{Status: Status{
		Enabled: cfg.Enabled, MaxOpensPerHour: cfg.EffectiveMaxOpensPerHour(),
		Blockers: []string{}, Skipped: map[string]int{},
	}}
	block := func(blocker string) { eval.Blockers = append(eval.Blockers, blocker) }
	if !cfg.Enabled {
		block(BlockDisabled)
	}

	latest, found, err := p.Jobs.LatestSystemEvent(ctx, PausedEvent, ResumedEvent)
	if err != nil {
		return eval, err
	}
	if found && latest.Kind == PausedEvent {
		eval.Paused = true
		eval.PausedReason, _ = latest.Detail["reason"].(string)
		eval.PausedAt = eventTime(latest, "paused_at").UTC().Format(time.RFC3339)
	}

	// Sign-in gates. One sign-in slot serves every paper at an institution,
	// so an open gate anywhere blocks the next open; one older than the wait
	// bound means nobody is completing it and a person is needed.
	attention, err := p.Jobs.CurrentHumanAttention(ctx)
	if err != nil {
		return eval, err
	}
	signInPending := false
	for i, gate := range attention.Gates {
		if gate.GateType != job.HumanGateLogin && gate.GateType != job.HumanGateMFA && gate.GateType != job.HumanGateCaptchaOrSecurity {
			continue
		}
		signInPending = true
		since, parseErr := time.Parse(time.RFC3339Nano, gate.UpdatedAt)
		if parseErr != nil || now.Sub(since) >= cfg.EffectiveSignInWait() {
			eval.signInStale = &attention.Gates[i]
			break
		}
	}
	if eval.Paused || (eval.signInStale != nil && cfg.Enabled) {
		block(BlockPaused)
	}

	if quiet, err := notify.ParseQuietHours(p.Config.Notify.QuietHours, time.Local); err == nil && quiet.Contains(now) {
		block(BlockQuietHours)
	}

	if p.Browser != nil {
		// An empty focus request is a pure query: it answers whether a
		// compatible holder is live and queues nothing.
		_, live, err := p.Browser.FocusHandoffs(ctx, nil)
		if err != nil {
			return eval, err
		}
		eval.HolderConnected = live
	}
	if !eval.HolderConnected {
		block(BlockHolderAbsent)
	}

	opens, err := p.Jobs.EventsOfKind(ctx, PacedOpenEvent, 1000)
	if err != nil {
		return eval, err
	}
	backedOff := map[string]bool{}
	for _, open := range opens {
		age := now.Sub(eventTime(open, "opened_at"))
		if age < time.Hour {
			eval.OpensLastHour++
		}
		if age < cfg.EffectiveJobBackoff() {
			backedOff[open.JobID] = true
		}
	}
	if eval.OpensLastHour >= eval.MaxOpensPerHour {
		block(BlockRateLimit)
	}
	if len(opens) > 0 {
		inFlight, err := p.inFlight(ctx, now, opens[0])
		if err != nil {
			return eval, err
		}
		if inFlight != nil {
			eval.InFlight = inFlight
			block(BlockInFlight)
		}
	}

	claims, permits, err := p.Jobs.LiveBrowserWork(ctx, now)
	if err != nil {
		return eval, err
	}
	if claims > 0 || permits > 0 {
		block(BlockBrowserBusy)
	}
	if signInPending && eval.signInStale == nil {
		block(BlockSignInPending)
	}

	next, err := p.nextCandidate(ctx, now, backedOff, eval.Skipped)
	if err != nil {
		return eval, err
	}
	eval.Next = next
	if next == nil {
		block(BlockNoEligibleJob)
	}
	return eval, nil
}

// inFlight reports whether the newest paced open still occupies the browser:
// it queued a surface, the browser has not answered it, the job is still
// parked on the action that was opened, and the settle bound has not passed.
func (p *Pacer) inFlight(ctx context.Context, now time.Time, open job.EventRecord) (*InFlight, error) {
	openedAt := eventTime(open, "opened_at")
	if now.Sub(openedAt) >= p.Config.Drive.EffectiveSettle() {
		return nil, nil
	}
	after, err := p.Jobs.JobEventsAfter(ctx, open.JobID, open.Seq)
	if err != nil {
		return nil, err
	}
	for _, event := range after {
		if settleKinds[event.Kind] {
			return nil, nil
		}
	}
	row, err := p.Jobs.Get(ctx, open.JobID)
	if err != nil || row.State != job.StateAwaitingHuman {
		// A job that disappeared or moved on no longer holds the browser.
		return nil, nil
	}
	actions, err := p.Jobs.ListOpenHumanActionsForJobs(ctx, []string{open.JobID})
	if err != nil {
		return nil, err
	}
	stillOpen := false
	for _, action := range actions {
		if action.Kind == handoffKind {
			stillOpen = true
		}
	}
	if !stillOpen {
		return nil, nil
	}
	action, _ := open.Detail["action"].(string)
	return &InFlight{JobID: open.JobID, Action: action, OpenedAt: openedAt.UTC().Format(time.RFC3339)}, nil
}

// nextCandidate walks open actions oldest first and returns the first one the
// pacer may open. skipped counts what it passed over on the way.
func (p *Pacer) nextCandidate(ctx context.Context, now time.Time, backedOff map[string]bool, skipped map[string]int) (*Candidate, error) {
	actions, err := p.Jobs.ListHumanActions(ctx, true)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(actions, func(a, b job.HumanAction) int {
		if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
			return c
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	openPerJob := map[string]int{}
	for _, action := range actions {
		openPerJob[action.JobID]++
	}
	cooled, err := p.cooldowns(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		switch action.Kind {
		case handoffKind, manualDownloadKind:
		case advisoryKind:
			// A conservative-mode advisory on a terminal job. It has no
			// tracked surface: `actions open` hands it to the OS launcher,
			// which opens outside papio's work window. The pacer never does.
			skipped[SkipAdvisory]++
			continue
		default:
			skipped[SkipOtherKind]++
			continue
		}
		if openPerJob[action.JobID] > 1 {
			// Two open actions on one job is exactly the ambiguity
			// `actions open --job` refuses to resolve by picking one.
			skipped[SkipDuplicateOnJob]++
			continue
		}
		if backedOff[action.JobID] {
			skipped[SkipBackoff]++
			continue
		}
		row, err := p.Jobs.Get(ctx, action.JobID)
		if err != nil || row.State != job.StateAwaitingHuman {
			skipped[SkipNotAwaiting]++
			continue
		}
		if cooled.covers(action.JobID, row.Work.DOI) {
			skipped[SkipHostCooldown]++
			continue
		}
		candidate := &Candidate{
			JobID: action.JobID, ActionID: action.ID, Action: action.Kind,
			Reason: "oldest_handoff", WaitingSince: action.CreatedAt, revision: action.Revision,
		}
		if action.Kind == manualDownloadKind {
			if err := p.Jobs.CheckRedriveInstitutionalHandoff(ctx, action.JobID, action.Revision, p.Config.OpenURLBaseFor, isOAHandoff); err != nil {
				if !errors.Is(err, job.ErrConflict) {
					return nil, err
				}
				skipped[SkipNotRedrivable]++
				continue
			}
			candidate.Reason = "redrive_manual_download"
		}
		return candidate, nil
	}
	return nil, nil
}

// cooldown is the set of jobs and DOI prefixes behind a provider host that
// is refusing this browser right now.
type cooldown struct {
	jobs     map[string]bool
	prefixes map[string]bool
}

func (c cooldown) covers(jobID, doi string) bool {
	if c.jobs[jobID] {
		return true
	}
	prefix := doiPrefix(doi)
	return prefix != "" && c.prefixes[prefix]
}

// cooldowns builds the skip set. A handoff's final provider is unknown until
// the resolver redirects, so a cooled host is mapped back to the papers that
// have been there (their events name the host) and to those papers' DOI
// prefixes: a publisher serves every DOI under its prefix, so while
// www.sciencedirect.com refuses this browser no 10.1016 paper is opened.
func (p *Pacer) cooldowns(ctx context.Context, now time.Time) (cooldown, error) {
	out := cooldown{jobs: map[string]bool{}, prefixes: map[string]bool{}}
	addJob := func(jobID string) error {
		if jobID == "" || out.jobs[jobID] {
			return nil
		}
		out.jobs[jobID] = true
		row, err := p.Jobs.Get(ctx, jobID)
		if err != nil {
			// A vanished job contributes no prefix; it cannot be opened.
			return nil
		}
		if prefix := doiPrefix(row.Work.DOI); prefix != "" {
			out.prefixes[prefix] = true
		}
		return nil
	}
	cooldowns, err := p.Jobs.EventsOfKind(ctx, job.ProviderCooldownEvent, 500)
	if err != nil {
		return out, err
	}
	hosts := map[string]bool{}
	for _, event := range cooldowns {
		until, _ := event.Detail["until"].(string)
		host, _ := event.Detail["host"].(string)
		deadline, err := time.Parse(time.RFC3339Nano, until)
		if err != nil || !deadline.After(now) || host == "" {
			continue
		}
		hosts[strings.ToLower(host)] = true
		if err := addJob(event.JobID); err != nil {
			return out, err
		}
	}
	for host := range hosts {
		ids, err := p.Jobs.JobsWithEventHost(ctx, host)
		if err != nil {
			return out, err
		}
		for _, id := range ids {
			if err := addJob(id); err != nil {
				return out, err
			}
		}
	}
	errorsSeen, err := p.Jobs.EventsOfKind(ctx, "browser.error", 500)
	if err != nil {
		return out, err
	}
	for _, event := range errorsSeen {
		if code, _ := event.Detail["code"].(string); code != "challenge_blocked" || now.Sub(event.At) >= challengeCooldown {
			continue
		}
		if err := addJob(event.JobID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// eventTime reads the pacer's own clock reading from an event it wrote,
// falling back to the store's timestamp. The pacer's windows (hour, backoff,
// settle) are measured on its clock, and the store stamps wall time.
func eventTime(event job.EventRecord, key string) time.Time {
	if value, ok := event.Detail[key].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	}
	return event.At
}

func doiPrefix(doi string) string {
	doi = strings.ToLower(strings.TrimSpace(doi))
	prefix, _, ok := strings.Cut(doi, "/")
	if !ok || !strings.HasPrefix(prefix, "10.") {
		return ""
	}
	return prefix
}

func isOAHandoff(detail string) bool {
	_, ok := app.OABrowserHandoffURL(detail)
	return ok
}

// String renders a one-line human summary of a status.
func (s Status) String() string {
	state := "enabled"
	switch {
	case !s.Enabled:
		state = "disabled"
	case s.Paused:
		state = "paused (" + s.PausedReason + ")"
	}
	line := fmt.Sprintf("paced drive %s; %d of %d opens in the last hour", state, s.OpensLastHour, s.MaxOpensPerHour)
	if s.InFlight != nil {
		line += "; in flight: " + s.InFlight.JobID
	}
	if s.Next != nil {
		line += "; next: " + s.Next.JobID + " (" + s.Next.Reason + ")"
	}
	if len(s.Blockers) > 0 {
		line += "; blocked by: " + strings.Join(s.Blockers, ", ")
	}
	return line
}
