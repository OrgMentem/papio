// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package livecohort measures papio's UNATTENDED acquisition behaviour
// against a cohort of real works, through the operator's real daemon.
//
// It is the live counterpart to internal/bench, and the two answer different
// questions on purpose. bench is hermetic: ephemeral store, resolvers wired
// to httptest fixtures, never the network — it measures a resolver change as
// a delta with nothing else moving. livecohort is the opposite: the real
// daemon, the real resolvers, the real browser bridge, the operator's real
// institution. Nothing about a run is reproducible in the bench sense, and
// that is the point — it is the only instrument that can see a provider that
// changed, an adapter that stopped matching, or a queue that quiesced.
//
// One number is primary and unconditional: WRONG ACCEPTS, works papio filed
// an artifact for when the cohort's judge said it should not have. A change
// that raises it does not ship whatever it does to throughput. The
// throughput number — autonomous completions, works finished with no human
// asked at all — is compared only once wrong accepts are flat or down. This
// is dev/identity-corpus.md's discipline applied one layer up, and for the
// same reason: the thresholds it protects were once tuned against a
// measurement nobody saved.
//
// A run is UNATTENDED by construction. It never answers a human action,
// never opens a handoff tab, and never drives the browser. A job parked on a
// person is a settled measurement, recorded with the action kind that parked
// it. Grading an unattended run against a human-judged cohort is what turns
// "papio asks for too much" from an impression into a count.
//
// Side effects on the operator's store are real and deliberate: a run
// submits real jobs. It submits them with auto_import disabled so no
// measurement reaches the operator's library, and by default it cancels
// every job it created that did not produce an artifact, so the queue it
// measured is the queue it leaves behind.
package livecohort

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/api"
	"papio/internal/bench"
	"papio/internal/job"
	"papio/internal/protocol"
)

// Caller sends one RPC to the daemon. The interface exists so a test can
// drive Run without a daemon, and so the command wiring owns socket and
// timeout policy.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// CandidateInspector reports how many fetch candidates a job still had
// untried when it stopped.
//
// This is the measurement that separates "papio had nothing left to try"
// from "papio gave up holding a working candidate", and no RPC exposes it —
// jobs.get_v3 publishes events and actions, not the candidate table. An
// implementation reads the store read-only; a run without one reports the
// column as unfilled rather than as zero, because zero is a claim.
type CandidateInspector interface {
	UntriedCandidates(ctx context.Context, jobID string) (untried, total int, err error)
}

// Options configures one run.
type Options struct {
	Cohort bench.Cohort
	Caller Caller
	// Inspector is optional; nil leaves the untried-candidate column unfilled.
	Inspector CandidateInspector
	// RunID namespaces this run's request ids. It must satisfy the
	// protocol's request_id charset; NewRunID builds a conforming one.
	RunID string
	// PerWorkBudget bounds how long one work may stay unsettled before it is
	// recorded as timed_out.
	PerWorkBudget time.Duration
	// Poll is the interval between settlement checks.
	Poll time.Duration
	// ParkSettle is how long a job observed in awaiting_human or needs_review
	// must stay there before the run believes it. Those states are not
	// terminal and the daemon advances out of them, so recording the first
	// sighting counts successes as human stops.
	ParkSettle time.Duration
	// Force submits even when the daemon already holds a live job for the
	// work. Without it such a work is skipped and disclosed, because
	// measuring a job this run did not create measures the operator's
	// history instead of papio's current behaviour.
	Force bool
	// Cleanup cancels every job this run created that produced no artifact.
	Cleanup bool
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

const (
	defaultPerWorkBudget = 5 * time.Minute
	defaultPoll          = 2 * time.Second
	// defaultParkSettle bounds the confirmation window for a parked
	// observation. The one late advance measured so far took 40s
	// (job_d0acd0940b8d3294c0d14281f8, 2026-09-19), so 60s covers it with
	// margin. No window proves a park is final — it bounds how wrong the
	// report can be, and the bound is stated rather than assumed away.
	// Windows overlap because a single loop drives every job, so confirming
	// every park in a thirty-work cohort costs one extra minute overall
	// rather than one per work.
	defaultParkSettle = time.Minute
)

// NewRunID derives a request-id-legal run stamp from a timestamp.
func NewRunID(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) validate() error {
	if o.Caller == nil {
		return errors.New("livecohort: a Caller is required")
	}
	if err := o.Cohort.Validate(); err != nil {
		return err
	}
	if !requestIDSafe(o.RunID) {
		return fmt.Errorf("livecohort: run id %q is not request-id safe", o.RunID)
	}
	return nil
}

// Run submits the cohort, waits for every work to settle unattended, grades
// the results, and returns the report.
//
// It returns an error only when the run could not be taken at all — a
// malformed cohort, an unreachable daemon on the first submission. Every
// per-work failure is a recorded outcome, because a report that omits the
// works that went wrong is the report that made the current situation
// invisible for twenty-three days.
func Run(ctx context.Context, opts Options) (Report, error) {
	if err := opts.validate(); err != nil {
		return Report{}, err
	}
	budget := opts.PerWorkBudget
	if budget <= 0 {
		budget = defaultPerWorkBudget
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	parkSettle := opts.ParkSettle
	if parkSettle <= 0 {
		parkSettle = defaultParkSettle
	}

	started := opts.now()
	report := Report{
		CohortID:      opts.Cohort.ID,
		RunID:         opts.RunID,
		StartedAt:     started.UTC().Format(time.RFC3339),
		PerWorkBudget: budget.String(),
		ParkSettle:    parkSettle.String(),
		Results:       make([]Result, 0, len(opts.Cohort.Works)),
	}

	pending := make([]*Result, 0, len(opts.Cohort.Works))
	for _, work := range opts.Cohort.Works {
		result := Result{Key: work.Key, Expected: work.ExpectedClass, Request: describeRequest(work.Request)}
		jobID, existing, err := submit(ctx, opts, work)
		switch {
		case err != nil:
			result.Outcome = SubmitFailed
			result.StopDetail = err.Error()
			result.Verdict = VerdictMissed
			report.Results = append(report.Results, result)
			continue
		case existing && !opts.Force:
			// Disclosed, never silently dropped: a skipped work changes what
			// every rate below it is a rate OF.
			report.Skipped = append(report.Skipped, Skip{Key: work.Key, Reason: "the daemon already holds a live job for this work; re-run with force to mint a new one", JobID: jobID})
			continue
		}
		result.JobID = jobID
		result.CreatedByRun = !existing || opts.Force
		result.submittedAt = opts.now()
		report.Results = append(report.Results, result)
		pending = append(pending, &report.Results[len(report.Results)-1])
	}

	settle(ctx, opts, pending, budget, poll, parkSettle)
	inspectCandidates(ctx, opts, pending)
	report.Cancelled, report.Kept = cleanup(ctx, opts, pending)

	for i := range report.Results {
		row := &report.Results[i]
		if row.Verdict == "" {
			row.Verdict = grade(row.Expected, row.Outcome)
		}
	}
	report.FinishedAt = opts.now().UTC().Format(time.RFC3339)
	report.summarize()
	return report, nil
}

// submit sends one work and returns its job id.
func submit(ctx context.Context, opts Options, work bench.Work) (string, bool, error) {
	params := submitParams{
		Request:    workRequest(work, opts.RunID),
		AutoImport: new(false),
		Force:      opts.Force,
	}
	var result api.SubmitV2Result
	if err := opts.Caller.Call(ctx, "acquire.submit_v2", params, &result); err != nil {
		return "", false, err
	}
	return result.JobID, result.Existing, nil
}

// submitParams mirrors the ratified acquire.submit_v2 params. auto_import is
// always sent and always false: a measurement must not write to the
// operator's library, and relying on the daemon's default would make that
// guarantee a configuration question.
type submitParams struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
	Force      bool                 `json:"force,omitempty"`
}

func workRequest(work bench.Work, runID string) protocol.WorkRequest {
	req := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     requestID(runID, work.Key),
		Title:         work.Request.Title,
		Authors:       work.Request.Authors,
		Year:          work.Request.Year,
	}
	if work.Request.DOI != "" || work.Request.ArXiv != "" || work.Request.PMID != "" {
		req.Identifiers = &protocol.Identifiers{DOI: work.Request.DOI, ArXiv: work.Request.ArXiv, PMID: work.Request.PMID}
	}
	return req
}

// requestID builds a request_id ([A-Za-z0-9_-]{8,128}) that names the run and
// the cohort key, so a job this tool created is identifiable in the store
// long after the run's report is gone.
func requestID(runID, key string) string {
	var b strings.Builder
	b.WriteString("livecohort-")
	b.WriteString(runID)
	b.WriteByte('-')
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	id := b.String()
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

func requestIDSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// settle polls every unsettled job until it reaches a measurable stop or
// exhausts its budget.
//
// One ticker drives every job rather than one goroutine each: the cohort is
// tens of works, the daemon schedules them itself, and a shared loop keeps
// the per-work elapsed times comparable instead of interleaving them with
// scheduler noise this tool introduced.
//
// A TERMINAL state is recorded immediately. A PARKING state is held for
// ParkSettle first, because awaiting_human and needs_review are not
// terminal: the daemon can advance out of them, and it does. Measured on
// the first real backlog run, job_d0acd0940b8d3294c0d14281f8 was observed
// parked on an openurl_handoff at 324s and reached ready 40s later, so the
// report counted a success as part of the human wall it exists to measure.
// An instrument that misreports in the direction of its own thesis is worse
// than no instrument, so the parked observation is confirmed before it is
// believed.
func settle(ctx context.Context, opts Options, pending []*Result, budget, poll, parkSettle time.Duration) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		remaining := 0
		for _, row := range pending {
			if row.Outcome != "" {
				continue
			}
			now := opts.now()
			detail, err := jobDetail(ctx, opts, row.JobID)
			switch {
			case err != nil && ctx.Err() != nil:
				row.Outcome = TimedOut
				row.StopDetail = "run cancelled before this job settled"
				continue
			case err != nil:
				// A transient read failure is not a result. Keep polling; the
				// budget below is what ends an unsettled job.
				remaining++
			default:
				outcome, settled := settledOutcome(detail)
				switch {
				case !settled:
					// Left a parking state on its own, or never reached one.
					row.parkedSince = time.Time{}
					remaining++
				case !parking(detail.Job.State):
					row.finish(now, outcome, detail)
					continue
				default:
					if row.parkedSince.IsZero() {
						row.parkedSince = now
					}
					if now.Sub(row.parkedSince) >= parkSettle {
						row.finish(now, outcome, detail)
						continue
					}
					remaining++
				}
			}
			if now.Sub(row.submittedAt) >= budget {
				row.Outcome = TimedOut
				row.ElapsedSeconds = int64(now.Sub(row.submittedAt).Seconds())
				if detail != nil && detail.Job != nil {
					row.StopDetail = "still " + detail.Job.State + " at the budget"
					row.FinalState = detail.Job.State
				} else {
					row.StopDetail = "unreadable at the budget: " + errText(err)
				}
				remaining--
			}
		}
		if remaining == 0 {
			return
		}
		select {
		case <-ctx.Done():
			for _, row := range pending {
				if row.Outcome == "" {
					row.Outcome = TimedOut
					row.StopDetail = "run cancelled before this job settled"
				}
			}
			return
		case <-ticker.C:
		}
	}
}

// parking names the two states a job can leave without a human, and so the
// two an observation must be confirmed in before it is recorded.
func parking(state string) bool {
	return state == job.StateAwaitingHuman || state == job.StateNeedsReview
}

func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// settledOutcome reports the outcome of a job that has stopped moving, and
// false while it is still working.
//
// "Stopped moving" is single-sourced on job.Terminal plus the two parking
// states, the same condition the CLI's own --wait uses. A second hand-kept
// list of working states would be one more place to forget a new state, and
// it would be redundant: retry_wait is not terminal and is not a parking
// state, so a job backing off keeps being polled rather than crediting a
// retry that never happened.
func settledOutcome(detail *api.JobDetailV3) (Outcome, bool) {
	if detail == nil || detail.Job == nil {
		return "", false
	}
	state := detail.Job.State
	if !job.Terminal(state) && state != job.StateAwaitingHuman && state != job.StateNeedsReview {
		return "", false
	}
	outcome, err := classify(detail)
	if err != nil {
		return "", false
	}
	return outcome, true
}

func jobDetail(ctx context.Context, opts Options, jobID string) (*api.JobDetailV3, error) {
	var detail api.JobDetailV3
	if err := opts.Caller.Call(ctx, "jobs.get_v3", map[string]string{"job_id": jobID}, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// inspectCandidates fills the untried-candidate column where an inspector is
// available. A failure leaves the column unfilled and is disclosed on the
// result, never reported as zero.
func inspectCandidates(ctx context.Context, opts Options, pending []*Result) {
	if opts.Inspector == nil {
		return
	}
	for _, row := range pending {
		if row.JobID == "" {
			continue
		}
		untried, total, err := opts.Inspector.UntriedCandidates(ctx, row.JobID)
		if err != nil {
			row.CandidateNote = err.Error()
			continue
		}
		row.UntriedCandidates = &untried
		row.TotalCandidates = &total
	}
}

// cleanup cancels every job this run created that produced no artifact, and
// returns the ids it cancelled and the ids it could not.
//
// A job that reached ready or imported is spared, and not as a courtesy:
// those states are terminal, so jobs.cancel refuses them outright
// ("was already ready; nothing to cancel"). papio has no verb that discards
// an acquired paper, so a measurement cannot undo one either.
//
// That makes reporting them mandatory rather than tidy. A public cohort
// acquires papers the operator never asked for, they land permanently in
// the ready queue, and a run that left them there silently would have made
// the queue less legible — the exact complaint this instrument exists to
// measure.
func cleanup(ctx context.Context, opts Options, pending []*Result) (cancelled, kept []string) {
	if !opts.Cleanup {
		return nil, nil
	}
	for _, row := range pending {
		if !row.CreatedByRun || row.JobID == "" {
			continue
		}
		if row.Outcome.ready() {
			kept = append(kept, row.JobID)
			continue
		}
		switch row.Outcome {
		case Unavailable, Failed, Cancelled, SubmitFailed:
			continue
		}
		var result map[string]any
		if err := opts.Caller.Call(ctx, "jobs.cancel", map[string]string{"job_id": row.JobID}, &result); err != nil {
			row.CleanupNote = err.Error()
			continue
		}
		// Cancel is a successful no-op for terminal jobs. A browser download
		// can complete after measurement, so read the committed state rather
		// than reporting the earlier parked observation as a cancellation.
		detail, err := jobDetail(ctx, opts, row.JobID)
		if err != nil {
			row.CleanupNote = err.Error()
			continue
		}
		if detail.Job.State == job.StateReady || detail.Job.State == job.StateImported {
			kept = append(kept, row.JobID)
			continue
		}
		if detail.Job.State != job.StateCancelled {
			row.CleanupNote = fmt.Sprintf("job is %s after cancellation", detail.Job.State)
			continue
		}
		row.CleanedUp = true
		cancelled = append(cancelled, row.JobID)
	}
	return cancelled, kept
}

func describeRequest(r bench.Request) string {
	switch {
	case r.DOI != "":
		return "doi:" + r.DOI
	case r.ArXiv != "":
		return "arxiv:" + r.ArXiv
	case r.PMID != "":
		return "pmid:" + r.PMID
	default:
		return r.Title
	}
}
