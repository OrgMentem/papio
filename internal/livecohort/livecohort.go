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
	"papio/internal/ipc"
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

// JobLookup answers which job the daemon committed for a work request id,
// terminal jobs included.
//
// It exists because the submit RPC cannot answer that question safely. The
// daemon deduplicates a request id against LIVE jobs only, so asking again
// discovers a live job and duplicates a terminal one. A lookup reads the
// committed association instead of asking for a second acquisition, and it
// is what lets a lost submit response and an interrupted earlier run both
// be reconciled rather than repeated.
//
// JobForConsumer answers the same question by the acquire.submit_v3 consumer
// tag a force submission carried. The daemon replaces the supplied request
// id with a generated one before storing on the force path, so the request
// id alone cannot name that job back; the consumer tag is stored on the job
// row itself and survives the replacement, terminal states included.
type JobLookup interface {
	JobForRequest(ctx context.Context, requestID string) (jobID, state string, found bool, err error)
	JobForConsumer(ctx context.Context, consumer string) (jobID, state string, found bool, err error)
}

// Options configures one run.
type Options struct {
	Cohort bench.Cohort
	Caller Caller
	// Inspector is optional; nil leaves the untried-candidate column unfilled.
	Inspector CandidateInspector
	// Lookup resolves a request id to the job the daemon committed for it.
	// Without one, a submission whose response was lost stays an explicit
	// ambiguity: this run never resubmits on a guess, because the daemon
	// would mint a second acquisition once the hidden job is terminal.
	Lookup JobLookup
	// Journal durably associates this run's cohort works with the request
	// ids it submitted, so a LATER run can reconcile an unaccounted
	// submission instead of asking for the same paper again. Optional; a
	// run without one cannot protect a rerun.
	Journal Journal
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

var (
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
	// reconcileTimeout bounds the read that names the job a lost submit
	// response may have committed.
	reconcileTimeout = 30 * time.Second
	// reconcilePoll is how often that read is repeated while its window is
	// open, because the commit and the lost response race.
	reconcilePoll = 500 * time.Millisecond
	// cleanupTimeout bounds the whole best-effort cancellation pass. It is
	// independent of the measurement context, which is already cancelled
	// on exactly the path that needs cleanup most.
	cleanupTimeout = 60 * time.Second
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

	// A journal this run cannot read cannot protect it: every unaccounted
	// submission it holds would become a duplicate acquisition. The run
	// refuses before it submits anything rather than measuring on a
	// safety record it could not consult.
	prior, err := unresolvedByWork(opts)
	if err != nil {
		return Report{}, err
	}

	// intentErr remembers a journal intent this run could not write. The work
	// is recorded as unsubmitted and the run continues so earlier submissions
	// are still settled, cleaned up, and reported; the error is returned with
	// the report at the end.
	var intentErr error
	pending := make([]*Result, 0, len(opts.Cohort.Works))
	for _, work := range opts.Cohort.Works {
		result := Result{Key: work.Key, Expected: work.ExpectedClass, Request: describeRequest(work.Request)}
		req := workRequest(work, opts.RunID)
		result.RequestID = req.RequestID
		if ctx.Err() != nil {
			result.Outcome = TimedOut
			result.StopDetail = "run cancelled before this work was submitted"
			result.Verdict = VerdictMissed
			report.Results = append(report.Results, result)
			continue
		}
		// An earlier run that lost a submit response left this work's
		// request id unaccounted for. Submitting now would ask for the same
		// paper a second time, because the daemon only deduplicates against
		// a LIVE job and the earlier one may already be terminal. Resolve
		// the earlier submission first, and disclose it.
		if entry, ok := prior[work.Key]; ok && !opts.Force {
			skip, settled := resolvePriorSubmission(ctx, opts, entry)
			report.Skipped = append(report.Skipped, skip)
			if settled {
				report.noteJournalFailure(noteJournalResolved(opts, entry.RequestID))
			}
			continue
		}
		// The intent is durable BEFORE the RPC, because an association
		// recorded after a lost response is one that was never recorded.
		// An intent this run cannot write is a submission it must not make.
		// The failure must not abandon the jobs earlier works already
		// submitted: they still need settlement, cleanup, and a report, so
		// the work is recorded as unsubmitted and the run continues.
		// A force intent carries the consumer tag the committed job will
		// store even as the daemon replaces the request id, so a lost
		// response and a later run both reconcile by that tag.
		intent := JournalEntry{CohortID: opts.Cohort.ID, WorkKey: work.Key, RequestID: req.RequestID, RunID: opts.RunID}
		if opts.Force {
			intent.Consumer = forceConsumer(req)
		}
		if err := noteJournal(opts, intent); err != nil {
			if intentErr == nil {
				intentErr = err
			}
			result.Outcome = SubmitFailed
			result.StopDetail = err.Error()
			result.Verdict = VerdictMissed
			report.noteJournalFailure(err)
			report.Results = append(report.Results, result)
			continue
		}
		jobID, existing, err := submit(ctx, opts, work, req)
		switch {
		case err != nil:
			var ambiguous *ambiguousSubmitError
			if errors.As(err, &ambiguous) {
				// The response was lost after the request was sent, so the
				// daemon may already hold a committed job this process
				// cannot name. Read the committed association rather than
				// resubmitting: a resubmission finds a live job and mints a
				// SECOND acquisition once the hidden one is terminal.
				recovered, rerr := reconcileLostSubmit(ctx, opts, req)
				if rerr != nil {
					result.Outcome = SubmitAmbiguous
					result.StopDetail = ambiguousDetail(ambiguous, rerr)
					result.Verdict = VerdictMissed
					report.Results = append(report.Results, result)
					continue
				}
				jobID, existing, err = recovered, false, nil
			} else {
				result.Outcome = SubmitFailed
				result.StopDetail = err.Error()
				result.Verdict = VerdictMissed
				report.noteJournalFailure(noteJournalResolved(opts, req.RequestID))
				report.Results = append(report.Results, result)
				continue
			}
		case existing && !opts.Force:
			// Disclosed, never silently dropped: a skipped work changes what
			// every rate below it is a rate OF.
			report.Skipped = append(report.Skipped, Skip{Key: work.Key, Reason: "the daemon already holds a live job for this work; re-run with force to mint a new one", JobID: jobID})
			report.noteJournalFailure(noteJournalResolved(opts, req.RequestID))
			continue
		}
		result.JobID = jobID
		result.CreatedByRun = !existing || opts.Force
		result.submittedAt = opts.now()
		completed := JournalEntry{CohortID: opts.Cohort.ID, WorkKey: work.Key, RequestID: req.RequestID, JobID: jobID, RunID: opts.RunID}
		if opts.Force {
			completed.Consumer = forceConsumer(req)
		}
		if committed := committedRequestID(ctx, opts, jobID, req.RequestID); committed != req.RequestID {
			// With force the daemon stores a generated request id instead of
			// the supplied one. The committed entry goes down FIRST and the
			// intent is retired only after: a crash between the two then leaves
			// both entries unresolved — recoverable by consumer tag — while the
			// reverse order would leave no entry at all and invite a duplicate
			// acquisition on the next run.
			result.RequestID = committed
			completed.RequestID = committed
			if nerr := noteJournal(opts, completed); nerr != nil {
				report.noteJournalFailure(nerr)
			} else {
				report.noteJournalFailure(noteJournalResolved(opts, req.RequestID))
			}
		} else {
			report.noteJournalFailure(noteJournal(opts, completed))
		}
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
		// An outcome the report NAMES is accounted for: it carries its job
		// id, so a later run may measure the work again. An unresolved
		// ambiguity deliberately stays in the journal.
		if row.Outcome != SubmitAmbiguous && row.RequestID != "" {
			report.noteJournalFailure(noteJournalResolved(opts, row.RequestID))
		}
	}
	report.FinishedAt = opts.now().UTC().Format(time.RFC3339)
	report.summarize()
	if intentErr != nil {
		return report, fmt.Errorf("%w; %d work(s) already submitted are settled and reported", intentErr, len(pending))
	}
	return report, nil
}

// submit sends one work and returns its job id.
//
// Non-force submissions use the ratified acquire.submit_v2 unchanged: the
// daemon preserves the supplied request id, so the request-id lookup names
// the committed job. Force submissions use acquire.submit_v3 with the
// request id as the consumer tag, because the daemon replaces the supplied
// request id with a generated one before storing on the force path and the
// request id alone cannot name that job back. The consumer tag is stored on
// the job row itself and is what the force recovery path reconciles by.
func submit(ctx context.Context, opts Options, work bench.Work, req protocol.WorkRequest) (string, bool, error) {
	if opts.Force {
		params := submitV3Params{
			Request:    req,
			AutoImport: new(false),
			Force:      true,
			Consumer:   forceConsumer(req),
		}
		var result api.SubmitV2Result
		if err := opts.Caller.Call(ctx, "acquire.submit_v3", params, &result); err != nil {
			if isUnknownMethod(err) {
				return "", false, fmt.Errorf("livecohort: daemon predates acquire.submit_v3, upgrade to run with -force: %w", err)
			}
			if isRefusal(err) {
				return "", false, err
			}
			return "", false, &ambiguousSubmitError{requestID: req.RequestID, cause: err}
		}
		return result.JobID, result.Existing, nil
	}
	params := submitParams{
		Request:    req,
		AutoImport: new(false),
	}
	var result api.SubmitV2Result
	if err := opts.Caller.Call(ctx, "acquire.submit_v2", params, &result); err != nil {
		if isRefusal(err) {
			return "", false, err
		}
		return "", false, &ambiguousSubmitError{requestID: req.RequestID, cause: err}
	}
	return result.JobID, result.Existing, nil
}

// forceConsumer is the acquire.submit_v3 attribution a force submission
// carries. It is the work's own request id: unique per run and work, drawn
// from the request-id charset which is a subset of the consumer charset, so
// it is always a legal consumer and always names exactly this submission.
func forceConsumer(req protocol.WorkRequest) string { return req.RequestID }

// isUnknownMethod reports whether the daemon answered that it has no such
// method, so the caller can name the skew instead of misreading it as a
// refusal or a lost response.
func isUnknownMethod(err error) bool {
	var remote *ipc.RemoteError
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.Code {
	case "unknown_method", "method_not_found", "unsupported_method":
		return true
	default:
		return false
	}
}

// ambiguousSubmitError says the submission RPC failed after the request
// was sent, so the run cannot prove the daemon did not commit a job.
type ambiguousSubmitError struct {
	requestID string
	cause     error
}

func (e *ambiguousSubmitError) Error() string {
	base := "livecohort: submission may have committed before its response was lost"
	if e != nil && e.requestID != "" {
		base += " for request " + e.requestID
	}
	if e == nil || e.cause == nil {
		return base
	}
	return base + ": " + e.cause.Error()
}

func (e *ambiguousSubmitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// isRefusal reports whether the daemon answered and refused the work.
// A classified RemoteError is an answer, even when it fails the run: the
// daemon produced it instead of a job, so no hidden acquisition can sit
// behind it. Any other error may have struck after the commit, on a
// response the caller never saw.
func isRefusal(err error) bool {
	var remote *ipc.RemoteError
	return errors.As(err, &remote)
}

// committedRequestID names the work_request_id the daemon actually stored
// for jobID. Without force it is the supplied id. With force the daemon
// replaces the supplied id with a generated one before storing
// (internal/job/job.go createRequest), so the caller must read the committed
// row back rather than assuming the id it sent. An unreadable detail keeps
// the supplied id: the journal keeps a handle the operator can correct by
// hand rather than losing the association entirely.
func committedRequestID(ctx context.Context, opts Options, jobID, supplied string) string {
	if !opts.Force || jobID == "" || supplied == "" {
		return supplied
	}
	detail, err := jobDetail(ctx, opts, jobID)
	if err != nil || detail == nil || detail.Job == nil || detail.Job.WorkRequestID == "" {
		return supplied
	}
	return detail.Job.WorkRequestID
}

// reconcileLostSubmit names the job a lost submission may have committed,
// by READING the committed association rather than resubmitting.
//
// It never resubmits. The daemon deduplicates a request id against live
// jobs only (internal/job/job.go createRequest), so a resubmission that
// arrives after the hidden job turned terminal creates a SECOND
// acquisition for the same paper — the duplicate this whole path exists
// to prevent. The read finds the job in every state, including terminal.
//
// Non-force submissions reconcile by request id, which the daemon preserves.
// Force submissions reconcile by the submit_v3 consumer tag, which the daemon
// stores on the job row itself even as it replaces the supplied request id
// with a generated one. Without a lookup the ambiguity stands: the caller
// reports it with its request id rather than guessing.
func reconcileLostSubmit(ctx context.Context, opts Options, req protocol.WorkRequest) (string, error) {
	if opts.Lookup == nil {
		return "", errors.New("livecohort: no store lookup available, so the submitted job cannot be named; resolve request " + req.RequestID + " by hand")
	}
	lookup := func(rctx context.Context) (string, error) {
		if opts.Force {
			jobID, _, found, err := opts.Lookup.JobForConsumer(rctx, forceConsumer(req))
			switch {
			case err != nil:
				return "", err
			case found && jobID != "":
				return jobID, nil
			default:
				return "", errors.New("no committed job for consumer " + forceConsumer(req))
			}
		}
		jobID, _, found, err := opts.Lookup.JobForRequest(rctx, req.RequestID)
		switch {
		case err != nil:
			return "", err
		case found && jobID != "":
			return jobID, nil
		default:
			return "", errors.New("no committed job for request " + req.RequestID)
		}
	}
	timeout := reconcileTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// The recovery runs on its own clock: the measurement context is
	// cancelled on exactly the interrupt that needs recovering.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// The daemon commits before it answers, but the commit and the lost
	// response race, so the read is retried until the window closes.
	ticker := time.NewTicker(reconcilePoll)
	defer ticker.Stop()
	var lastErr error
	for {
		if jobID, err := lookup(rctx); err == nil {
			return jobID, nil
		} else {
			lastErr = err
		}
		select {
		case <-rctx.Done():
			return "", lastErr
		case <-ticker.C:
		}
	}
}

// resolvePriorSubmission reports what became of an earlier run's
// unaccounted submission, and whether it is now accounted for.
//
// A tagged force entry resolves by its consumer tag alone: the supplied
// request id names nothing on the force path, and a request-id row — if
// one exists — belongs to a different acquisition than the tag does, so
// consulting it first could attribute an older job. An untagged entry
// resolves by request id alone and is never guessed by consumer.
func resolvePriorSubmission(ctx context.Context, opts Options, entry JournalEntry) (Skip, bool) {
	skip := Skip{Key: entry.WorkKey, JobID: entry.JobID}
	jobID := entry.JobID
	state := ""
	if opts.Lookup != nil {
		if entry.Consumer != "" {
			if found, foundState, ok, err := opts.Lookup.JobForConsumer(ctx, entry.Consumer); err == nil && ok {
				jobID, state = found, foundState
			}
		} else if found, foundState, ok, err := opts.Lookup.JobForRequest(ctx, entry.RequestID); err == nil && ok {
			jobID, state = found, foundState
		}
	}
	if jobID == "" {
		skip.Reason = "run " + entry.RunID + " lost the response to request " + entry.RequestID +
			" and no job can be found for it; submitting again could duplicate a real acquisition, so check `papio jobs list` and re-run with force"
		return skip, false
	}
	skip.JobID = jobID
	if state == "" {
		state = "of unknown state"
	}
	skip.Reason = "run " + entry.RunID + " already submitted this work as request " + entry.RequestID +
		"; its job " + jobID + " is " + state + ", so this run did not ask for the paper again — re-run with force to mint a new job"
	return skip, true
}

func ambiguousDetail(cause *ambiguousSubmitError, reconcileErr error) string {
	detail := cause.Error()
	if reconcileErr != nil && reconcileErr.Error() != "" {
		detail += "; reconcile: " + reconcileErr.Error()
	}
	return detail
}

// unresolvedByWork indexes the journal entries no run has accounted for.
//
// A journal this run cannot read is a failure, never an empty journal: the
// entries it holds are what keep this run from submitting a paper an
// earlier run already asked for, so reading them is a precondition of
// submitting at all.
func unresolvedByWork(opts Options) (map[string]JournalEntry, error) {
	if opts.Journal == nil {
		return nil, nil
	}
	entries, err := opts.Journal.Unresolved(opts.Cohort.ID)
	if err != nil {
		return nil, fmt.Errorf("livecohort: reading the submission journal: %w", err)
	}
	byWork := make(map[string]JournalEntry, len(entries))
	for _, entry := range entries {
		byWork[entry.WorkKey] = entry
	}
	return byWork, nil
}

// noteJournal records one association. Its error is the caller's to act on:
// an association that was not written cannot protect the next run.
func noteJournal(opts Options, entry JournalEntry) error {
	if opts.Journal == nil {
		return nil
	}
	if err := opts.Journal.Note(entry); err != nil {
		return fmt.Errorf("livecohort: recording request %s in the submission journal: %w", entry.RequestID, err)
	}
	return nil
}

// noteJournalResolved marks one request accounted for and reports a write
// it could not make, because an entry that stayed unresolved silently would
// refuse the work on every later run.
func noteJournalResolved(opts Options, requestID string) error {
	if opts.Journal == nil || requestID == "" {
		return nil
	}
	if err := opts.Journal.Resolve(requestID); err != nil {
		return fmt.Errorf("livecohort: marking request %s accounted for in the submission journal: %w", requestID, err)
	}
	return nil
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

// submitV3Params mirrors acquire.submit_v3, which carries the consumer
// attribution submit_v2 cannot. Force submissions use it so the committed
// job carries the run/work tag the recovery lookup reads back.
type submitV3Params struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
	Force      bool                 `json:"force,omitempty"`
	Consumer   string               `json:"consumer,omitempty"`
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
	// An interrupted run still cancels: the measurement context is
	// already done on exactly the path that needs cleanup most, so this
	// pass runs on its own bounded clock instead of inheriting it.
	timeout := cleanupTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	for _, row := range pending {
		if !row.CreatedByRun || row.JobID == "" {
			continue
		}
		if row.Outcome.ready() {
			kept = append(kept, row.JobID)
			continue
		}
		switch row.Outcome {
		case Unavailable, Failed, Cancelled, SubmitFailed, SubmitAmbiguous:
			continue
		}
		var result map[string]any
		if err := opts.Caller.Call(cctx, "jobs.cancel", map[string]string{"job_id": row.JobID}, &result); err != nil {
			row.CleanupNote = "cancellation unconfirmed for " + row.JobID + ": " + err.Error()
			continue
		}
		// Cancel is a successful no-op for terminal jobs. A browser download
		// can complete after measurement, so read the committed state rather
		// than reporting the earlier parked observation as a cancellation.
		detail, err := jobDetail(cctx, opts, row.JobID)
		if err != nil {
			row.CleanupNote = "cancellation of " + row.JobID + " unconfirmed: " + err.Error()
			continue
		}
		if detail.Job.State == job.StateReady || detail.Job.State == job.StateImported {
			kept = append(kept, row.JobID)
			continue
		}
		if detail.Job.State != job.StateCancelled {
			row.CleanupNote = row.JobID + " is " + detail.Job.State + " after cancellation, so it may still be running"
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
