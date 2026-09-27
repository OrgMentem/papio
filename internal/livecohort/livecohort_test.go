// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/bench"
	"papio/internal/ipc"
	"papio/internal/job"
)

type committedJob struct {
	id    string
	state string
}

// fakeDaemon answers the three methods a run uses, marshalling through JSON
// the way the real ipc client does so a shape mismatch fails here too.
type fakeDaemon struct {
	// states[jobID] is the sequence of job rows successive polls observe;
	// the last entry repeats once exhausted.
	states map[string][]api.JobDetailV3
	// requestOf[jobID] is the committed work_request_id the ownership
	// read returns; unset means the job owns the request id the run
	// used to submit it.
	requestOf map[string]string
	// consumerOf[jobID] is the submit_v3 consumer tag the job was stored
	// with; only force submissions carry one.
	consumerOf map[string]string
	polls      map[string]int
	existing   map[string]bool
	cancelled  []string
	submitted  []string
	// submittedVia records the RPC method per submitted job id, so tests
	// can pin force onto submit_v3 and non-force onto submit_v2.
	submittedVia map[string]string
	submitErr    map[string]error
	// submitFails counts remaining submit failures per request id, so a
	// test can lose the response the daemon's commit already outran.
	submitFails map[string]int
	// committed[requestID] is the job the daemon committed for a request,
	// which the store lookup reads back. A lost response leaves it set and
	// the caller ignorant, which is the whole defect under test.
	committed map[string]committedJob
	// byConsumer[consumer] is the job the daemon committed for a submit_v3
	// consumer tag, terminal ones included. Force replaces the request id
	// before storing, so this — not committed — is what names a forced job
	// back after a lost response.
	byConsumer map[string]committedJob
	// loseWithoutCommit marks requests whose submit fails before any job
	// is committed, so the lookup truthfully finds nothing.
	loseWithoutCommit map[string]bool
	// refuseCancel makes jobs.cancel fail, to prove the failure is named.
	refuseCancel bool
	// cancelSawLiveCtx records whether jobs.cancel arrived on a live
	// context, which is the interrupted-run regression.
	cancelSawLiveCtx bool
	nextID           int
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		states:            map[string][]api.JobDetailV3{},
		requestOf:         map[string]string{},
		consumerOf:        map[string]string{},
		polls:             map[string]int{},
		existing:          map[string]bool{},
		submittedVia:      map[string]string{},
		submitErr:         map[string]error{},
		submitFails:       map[string]int{},
		committed:         map[string]committedJob{},
		byConsumer:        map[string]committedJob{},
		loseWithoutCommit: map[string]bool{},
	}
}

// JobForRequest is the read-only store lookup: it finds the committed job
// for a request id in EVERY state, terminal ones included.
func (f *fakeDaemon) JobForRequest(_ context.Context, requestID string) (string, string, bool, error) {
	entry, ok := f.committed[requestID]
	if !ok {
		return "", "", false, nil
	}
	return entry.id, entry.state, true, nil
}

// JobForConsumer is the force-path store lookup: it finds the committed job
// for a submit_v3 consumer tag in EVERY state, terminal ones included.
func (f *fakeDaemon) JobForConsumer(_ context.Context, consumer string) (string, string, bool, error) {
	entry, ok := f.byConsumer[consumer]
	if !ok {
		return "", "", false, nil
	}
	return entry.id, entry.state, true, nil
}

func (f *fakeDaemon) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		var decoded submitParams
		if err := roundTrip(params, &decoded); err != nil {
			return err
		}
		return f.submitWork(decoded.Request.RequestID, "", decoded.Force, decoded.AutoImport, "acquire.submit_v2", result)
	case "acquire.submit_v3":
		var decoded submitV3Params
		if err := roundTrip(params, &decoded); err != nil {
			return err
		}
		return f.submitWork(decoded.Request.RequestID, decoded.Consumer, decoded.Force, decoded.AutoImport, "acquire.submit_v3", result)
	case "jobs.get_v3":
		var getParams map[string]string
		if err := roundTrip(params, &getParams); err != nil {
			return err
		}
		id := getParams["job_id"]
		seq := f.states[id]
		if len(seq) == 0 {
			return errors.New("no such job " + id)
		}
		i := f.polls[id]
		if i >= len(seq) {
			i = len(seq) - 1
		}
		f.polls[id] = i + 1
		got := seq[i]
		if req, ok := f.requestOf[id]; ok && got.Job != nil {
			got.Job.WorkRequestID = req
		}
		return roundTrip(got, result)
	case "jobs.cancel":
		f.cancelSawLiveCtx = f.cancelSawLiveCtx || ctx.Err() == nil
		if f.refuseCancel {
			return errors.New("daemon unavailable for cancel")
		}
		var decoded map[string]string
		if err := roundTrip(params, &decoded); err != nil {
			return err
		}
		f.cancelled = append(f.cancelled, decoded["job_id"])
		if seq := f.states[decoded["job_id"]]; len(seq) > 0 && !job.Terminal(seq[len(seq)-1].Job.State) {
			f.states[decoded["job_id"]] = []api.JobDetailV3{detail(job.StateCancelled, "")}
		}
		return roundTrip(map[string]bool{"cancelled": true}, result)
	}
	return errors.New("unexpected method " + method)
}

// submitWork models the daemon commit for one submit RPC. Force replaces the
// supplied request id with a generated one before storing, so the committed
// request-id association lives under a key the caller never sent; the v3
// consumer tag rides on the job row itself and is what names a forced job
// back. The job id stays derived from the supplied key so tests can seed
// states ahead of time.
func (f *fakeDaemon) submitWork(key, consumer string, force bool, autoImport *bool, method string, result any) error {
	commitKey := key
	if force {
		commitKey = "forced-" + key
	}
	if err := f.submitErr[key]; err != nil {
		return err
	}
	commit := func() (string, string) {
		id := f.idFor(key)
		state := job.StateQueued
		if seq := f.states[id]; len(seq) > 0 {
			state = seq[len(seq)-1].Job.State
		} else {
			f.states[id] = []api.JobDetailV3{detailFor(id, state, "")}
		}
		f.requestOf[id] = commitKey
		f.committed[commitKey] = committedJob{id: id, state: state}
		if consumer != "" {
			f.consumerOf[id] = consumer
			f.byConsumer[consumer] = committedJob{id: id, state: state}
		}
		return id, state
	}
	if n := f.submitFails[key]; n > 0 {
		f.submitFails[key] = n - 1
		if f.loseWithoutCommit[key] {
			return context.DeadlineExceeded
		}
		// The daemon commits the acquisition BEFORE it answers, so a
		// lost response leaves a real job the caller cannot name.
		commit()
		return context.DeadlineExceeded
	}
	if autoImport == nil || *autoImport {
		return errors.New("a measurement must submit with auto_import false")
	}
	f.nextID++
	id, _ := commit()
	f.submitted = append(f.submitted, id)
	f.submittedVia[id] = method
	existing := f.existing[key] && !force
	return roundTrip(api.SubmitV2Result{JobID: id, Existing: existing}, result)
}

// idFor maps a request id onto a stable job id so a test can seed states
// before the run submits.
func (f *fakeDaemon) idFor(requestID string) string {
	parts := strings.Split(requestID, "-")
	return "job_" + parts[len(parts)-1]
}

func roundTrip(from, into any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

func detail(state, terminalReason string, actions ...job.HumanAction) api.JobDetailV3 {
	return detailFor("job", state, terminalReason, actions...)
}

func detailFor(id, state, terminalReason string, actions ...job.HumanAction) api.JobDetailV3 {
	rows := make([]api.ActionRow, 0, len(actions))
	for _, action := range actions {
		rows = append(rows, api.ActionRow{HumanAction: action})
	}
	return api.JobDetailV3{
		Job:     &api.JobRow{Row: job.Row{ID: id, State: state, TerminalReason: terminalReason}},
		Actions: rows,
	}
}

func cohortOf(works ...bench.Work) bench.Cohort {
	return bench.Cohort{SchemaVersion: bench.CohortSchemaVersion, ID: "test", Works: works}
}

func work(key string, expected bench.ExpectedClass) bench.Work {
	return bench.Work{Key: key, Request: bench.Request{DOI: "10.1/" + key}, ExpectedClass: expected}
}

func runWith(t *testing.T, daemon *fakeDaemon, cohort bench.Cohort, mutate func(*Options)) Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := Options{
		Cohort:        cohort,
		Caller:        daemon,
		Lookup:        daemon,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		ParkSettle:    time.Microsecond,
		Poll:          time.Microsecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	report, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// A job that reaches ready without ever opening a human action is the only
// outcome papio gets full credit for. One that opens an action and finishes
// anyway is recorded separately, because the prompt was the defect.
func TestReadyWithoutAnActionIsAutonomousAndWithOneIsAssisted(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_quiet"] = []api.JobDetailV3{detail(job.StateReady, "")}
	daemon.states["job_asked"] = []api.JobDetailV3{
		detail(job.StateReady, "", job.HumanAction{Kind: "manual_download", Status: "resolved"}),
	}

	report := runWith(t, daemon, cohortOf(
		work("quiet", bench.AutonomousReady),
		work("asked", bench.AutonomousReady),
	), nil)

	if got := report.Results[0].Outcome; got != AutonomousReady {
		t.Fatalf("quiet outcome = %q, want %q", got, AutonomousReady)
	}
	if got := report.Results[1].Outcome; got != AssistedReady {
		t.Fatalf("asked outcome = %q, want %q", got, AssistedReady)
	}
	if report.AutonomousReady != 1 {
		t.Fatalf("autonomous_ready = %d, want 1 — an assisted finish must not be counted as autonomous", report.AutonomousReady)
	}
	if report.Results[1].Verdict != VerdictMissed {
		t.Fatalf("assisted verdict = %q, want %q against an autonomous_ready expectation", report.Results[1].Verdict, VerdictMissed)
	}
}

// Filing an artifact for a work judged unavailable, or one judged to need a
// human identity decision, is the primary gate. It must outrank the plain
// miss it also is.
func TestFilingAgainstANonReadyExpectationIsAWrongAccept(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected bench.ExpectedClass
	}{
		{"judged unavailable", bench.HonestUnavailable},
		{"judged to need identity review", bench.IdentityReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeDaemon()
			daemon.states["job_w"] = []api.JobDetailV3{detail(job.StateImported, "")}

			report := runWith(t, daemon, cohortOf(work("w", tc.expected)), nil)

			if got := report.Results[0].Verdict; got != VerdictWrongAccept {
				t.Fatalf("verdict = %q, want %q", got, VerdictWrongAccept)
			}
			if report.WrongAccepts != 1 {
				t.Fatalf("wrong_accepts = %d, want 1", report.WrongAccepts)
			}
		})
	}
}

// An unattended run stops at the human boundary by design, so reaching it is
// what a ready_after_human_boundary expectation is satisfied by.
func TestHumanBoundarySatisfiesTheBoundaryExpectationAndNotTheAutonomousOne(t *testing.T) {
	daemon := newFakeDaemon()
	parked := detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"})
	daemon.states["job_boundary"] = []api.JobDetailV3{parked}
	daemon.states["job_wanted"] = []api.JobDetailV3{parked}

	report := runWith(t, daemon, cohortOf(
		work("boundary", bench.ReadyAfterHumanBoundary),
		work("wanted", bench.AutonomousReady),
	), nil)

	if got := report.Results[0].Verdict; got != VerdictMet {
		t.Fatalf("boundary verdict = %q, want %q", got, VerdictMet)
	}
	if got := report.Results[1].Verdict; got != VerdictMissed {
		t.Fatalf("autonomous-expected verdict = %q, want %q", got, VerdictMissed)
	}
	if got := report.Results[0].StopDetail; got != "openurl_handoff" {
		t.Fatalf("stop detail = %q, want the open action kind", got)
	}
}

// A run must wait through the working states rather than reading the first
// poll as a result; retry_wait is working, not settled.
func TestPollingWaitsThroughWorkingStatesIncludingRetryWait(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_slow"] = []api.JobDetailV3{
		detail(job.StateQueued, ""),
		detail(job.StateResolving, ""),
		detail("retry_wait", ""),
		detail(job.StateUnavailable, "no legal candidates"),
	}

	report := runWith(t, daemon, cohortOf(work("slow", bench.HonestUnavailable)), nil)

	if got := report.Results[0].Outcome; got != Unavailable {
		t.Fatalf("outcome = %q, want %q — an earlier poll was read as settled", got, Unavailable)
	}
	if got := report.Results[0].StopDetail; got != "no legal candidates" {
		t.Fatalf("stop detail = %q, want the terminal reason", got)
	}
	if report.Results[0].Verdict != VerdictMet {
		t.Fatalf("verdict = %q, want met", report.Results[0].Verdict)
	}
}

// A work still working at its budget is a result about papio, not an error
// to drop: dropping it would flatter every rate in the report.
func TestAWorkStillWorkingAtItsBudgetIsRecordedAsTimedOut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_stuck"] = []api.JobDetailV3{detail(job.StateFetching, "")}

	report := runWith(t, daemon, cohortOf(work("stuck", bench.AutonomousReady)), func(o *Options) {
		o.PerWorkBudget = time.Nanosecond
	})

	if got := report.Results[0].Outcome; got != TimedOut {
		t.Fatalf("outcome = %q, want %q", got, TimedOut)
	}
	if report.Measured != 1 {
		t.Fatalf("measured = %d, want the timed-out work counted in the denominator", report.Measured)
	}
	if !strings.Contains(report.Results[0].StopDetail, job.StateFetching) {
		t.Fatalf("stop detail = %q, want the state it was stuck in", report.Results[0].StopDetail)
	}
}

// Measuring a job this run did not create measures the operator's history.
// Such a work is skipped, and the skip is reported because it changes what
// every rate is a rate of.
func TestAnExistingLiveJobIsSkippedAndDisclosed(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.existing["livecohort-testrun-held"] = true
	daemon.states["job_held"] = []api.JobDetailV3{detail(job.StateAwaitingHuman, "")}

	report := runWith(t, daemon, cohortOf(work("held", bench.ReadyAfterHumanBoundary)), nil)

	if len(report.Results) != 0 {
		t.Fatalf("results = %d, want the work excluded from the measurement", len(report.Results))
	}
	if len(report.Skipped) != 1 || report.Skipped[0].Key != "held" {
		t.Fatalf("skipped = %+v, want the held work disclosed", report.Skipped)
	}
	if report.Measured != 0 {
		t.Fatalf("measured = %d, want 0", report.Measured)
	}
}

// Cleanup keeps the queue it measured, but never discards a real paper.
func TestCleanupCancelsParkedJobsAndSparesOnesHoldingAnArtifact(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_parked"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
	}
	daemon.states["job_got"] = []api.JobDetailV3{detail(job.StateReady, "")}
	daemon.states["job_gone"] = []api.JobDetailV3{detail(job.StateUnavailable, "no legal candidates")}

	report := runWith(t, daemon, cohortOf(
		work("parked", bench.ReadyAfterHumanBoundary),
		work("got", bench.AutonomousReady),
		work("gone", bench.HonestUnavailable),
	), func(o *Options) { o.Cleanup = true })

	if len(daemon.cancelled) != 1 || daemon.cancelled[0] != "job_parked" {
		t.Fatalf("cancelled = %v, want only the parked job", daemon.cancelled)
	}
	if len(report.Cancelled) != 1 {
		t.Fatalf("report cancelled = %v, want one id", report.Cancelled)
	}
	// Sparing an artifact silently leaves papers the operator never asked
	// for sitting in the ready queue, which is the illegibility this tool
	// measures. The spare must be stated.
	if len(report.Kept) != 1 || report.Kept[0] != "job_got" {
		t.Fatalf("report kept = %v, want the spared artifact-holding job named", report.Kept)
	}
	var rendered strings.Builder
	if err := report.Render(&rendered); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered.String(), "job_got") {
		t.Fatalf("rendered report does not name the kept job:\n%s", rendered.String())
	}
}

func TestCleanupReportsACompletionAfterTheParkedObservation(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_late"] = []api.JobDetailV3{detail(job.StateReady, "")}
	observed := &Result{JobID: "job_late", Outcome: HumanBoundary, CreatedByRun: true}
	cancelled, kept := cleanup(context.Background(), Options{Caller: daemon, Cleanup: true}, []*Result{observed})
	if len(cancelled) != 0 || observed.CleanedUp || observed.CleanupNote != "" {
		t.Fatalf("late completion reported as cancelled or failed: %+v, %v", observed, cancelled)
	}
	if len(kept) != 1 || kept[0] != observed.JobID {
		t.Fatalf("late completion missing from kept jobs: %v", kept)
	}
}

// A submission whose response is lost after the daemon commits is named by
// READING the committed job for its request id. The run never asks for the
// paper a second time, so the job it measures is the job it caused.
func TestALostSubmitResponseIsReconciledByRequestID(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-testrun-flaky"] = 1
	daemon.states["job_flaky"] = []api.JobDetailV3{detailFor("job_flaky", job.StateUnavailable, "no legal candidates")}

	report := runWith(t, daemon, cohortOf(work("flaky", bench.HonestUnavailable)), func(o *Options) {
		o.Cleanup = true
	})

	row := report.Results[0]
	if row.JobID != "job_flaky" {
		t.Fatalf("job id = %q, want the committed job the lookup named", row.JobID)
	}
	if row.Outcome != Unavailable {
		t.Fatalf("outcome = %q, want the settled job rather than a submit verdict", row.Outcome)
	}
	if !row.CreatedByRun {
		t.Fatalf("created_by_run = false, want the recovered job owned by this run")
	}
	if row.RequestID != "livecohort-testrun-flaky" {
		t.Fatalf("request id = %q, want the id that names the acquisition for cleanup by hand", row.RequestID)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d answered submissions, want none: recovery reads the store and never asks again", got)
	}
}

// The duplicate this path exists to prevent: the hidden job is already
// TERMINAL, so a resubmission would create a second real acquisition. The
// lookup finds a terminal job where the daemon's own deduplication does not.
func TestALostSubmitIsNeverResubmittedOnceItsJobIsTerminal(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_fast"] = []api.JobDetailV3{detailFor("job_fast", job.StateReady, "")}
	daemon.submitFails["livecohort-testrun-fast"] = 100

	report := runWith(t, daemon, cohortOf(work("fast", bench.AutonomousReady)), nil)

	row := report.Results[0]
	if row.JobID != "job_fast" {
		t.Fatalf("job id = %q, want the terminal job the lookup named", row.JobID)
	}
	if row.Outcome != AutonomousReady {
		t.Fatalf("outcome = %q, want the terminal job measured", row.Outcome)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d, want no second acquisition for a paper the daemon already finished", got)
	}
}

// A lost response with nothing committed stays visible: it is an explicit
// ambiguity carrying its request id, never a silent refusal, and cleanup
// leaves it alone rather than cancelling blind.
func TestAnUnrecoverableSubmitStaysExplicitlyAmbiguous(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-testrun-lost"] = 100
	daemon.loseWithoutCommit["livecohort-testrun-lost"] = true

	report := runWith(t, daemon, cohortOf(work("lost", bench.AutonomousReady)), func(o *Options) {
		o.Cleanup = true
	})
	row := report.Results[0]
	if row.Outcome != SubmitAmbiguous {
		t.Fatalf("outcome = %q, want %q", row.Outcome, SubmitAmbiguous)
	}
	if row.JobID != "" {
		t.Fatalf("job id = %q, want empty when no job could be named", row.JobID)
	}
	if !strings.Contains(row.StopDetail, "livecohort-testrun-lost") {
		t.Fatalf("stop detail = %q, want the request id for cleanup by hand", row.StopDetail)
	}
	if len(daemon.cancelled) != 0 {
		t.Fatalf("cancelled = %v, want no blind cancellation of an unnamed job", daemon.cancelled)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d, want no submission on a guess", got)
	}
	if row.Verdict != VerdictMissed {
		t.Fatalf("verdict = %q, want a miss rather than silent success", row.Verdict)
	}
	// The operator must be able to find the possible job by hand, so the
	// rendered report names the request id rather than burying it.
	var rendered strings.Builder
	if err := report.Render(&rendered); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered.String(), "livecohort-testrun-lost") ||
		!strings.Contains(rendered.String(), "AMBIGUOUS SUBMISSIONS") {
		t.Fatalf("rendered report hides the ambiguous submission:\n%s", rendered.String())
	}
}

// Without a store lookup the run cannot name a committed job, so it keeps
// the ambiguity instead of resubmitting into a possible duplicate.
func TestWithoutALookupALostSubmitIsAmbiguousAndNeverResubmitted(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-testrun-blind"] = 1

	report := runWith(t, daemon, cohortOf(work("blind", bench.AutonomousReady)), func(o *Options) {
		o.Lookup = nil
	})

	if got := report.Results[0].Outcome; got != SubmitAmbiguous {
		t.Fatalf("outcome = %q, want %q without a lookup", got, SubmitAmbiguous)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d, want no resubmission while the committed job cannot be read", got)
	}
}

// The cross-run duplicate: a run loses a submit response, and a LATER run
// with a fresh run id would mint a second acquisition for the same paper
// once the first job is terminal. The durable journal is what lets the
// second run find the first run's job instead of repeating it.
func TestARerunReconcilesAnUnaccountedSubmissionInsteadOfDuplicatingIt(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-runone-slow"] = 100
	daemon.loseWithoutCommit["livecohort-runone-slow"] = false
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	cohort := cohortOf(work("slow", bench.AutonomousReady))

	// The first run loses the response after the daemon committed job_slow,
	// and no lookup is available to it, so the ambiguity stays durable.
	first := runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runone"
		o.Lookup = nil
		o.Journal = journal
	})
	if got := first.Results[0].Outcome; got != SubmitAmbiguous {
		t.Fatalf("first run outcome = %q, want %q", got, SubmitAmbiguous)
	}

	// The job the first run could not name finishes before the rerun.
	daemon.states["job_slow"] = []api.JobDetailV3{detailFor("job_slow", job.StateReady, "")}
	daemon.committed["livecohort-runone-slow"] = committedJob{id: "job_slow", state: job.StateReady}

	second := runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runtwo"
		o.Journal = journal
	})

	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %v, want no second acquisition for a work the first run already asked for", daemon.submitted)
	}
	if len(second.Results) != 0 {
		t.Fatalf("results = %+v, want the work disclosed as skipped rather than measured from history", second.Results)
	}
	if len(second.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the unaccounted submission disclosed", second.Skipped)
	}
	skip := second.Skipped[0]
	if skip.JobID != "job_slow" || !strings.Contains(skip.Reason, "livecohort-runone-slow") {
		t.Fatalf("skip = %+v, want the earlier run's request id and its job named", skip)
	}

	// Once disclosed, the association is accounted for: a third run may
	// measure the work again rather than refusing it forever.
	third := runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runthree"
		o.Journal = journal
	})
	if len(third.Results) != 1 || third.Results[0].JobID == "" {
		t.Fatalf("third run = %+v, want the work measured again after the ambiguity was accounted for", third.Results)
	}
}

// A rerun that cannot read the store must not submit on a guess either: an
// unaccounted submission with no lookup is disclosed, not repeated.
func TestARerunWithoutALookupRefusesToResubmitAnUnaccountedWork(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-runone-slow"] = 100
	daemon.loseWithoutCommit["livecohort-runone-slow"] = true
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	cohort := cohortOf(work("slow", bench.AutonomousReady))

	runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runone"
		o.Lookup = nil
		o.Journal = journal
	})
	second := runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runtwo"
		o.Lookup = nil
		o.Journal = journal
	})

	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %v, want no submission while the earlier one is unaccounted for", daemon.submitted)
	}
	if len(second.Skipped) != 1 || !strings.Contains(second.Skipped[0].Reason, "force") {
		t.Fatalf("skipped = %+v, want the unresolved submission disclosed with the force remedy", second.Skipped)
	}
}

// Force is the operator overriding that refusal on purpose: it submits.
func TestForceSubmitsEvenWithAnUnaccountedEarlierSubmission(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitFails["livecohort-runone-slow"] = 100
	daemon.loseWithoutCommit["livecohort-runone-slow"] = true
	daemon.states["job_slow"] = []api.JobDetailV3{detailFor("job_slow", job.StateReady, "")}
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	cohort := cohortOf(work("slow", bench.AutonomousReady))

	runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runone"
		o.Lookup = nil
		o.Journal = journal
	})
	second := runWith(t, daemon, cohort, func(o *Options) {
		o.RunID = "runtwo"
		o.Journal = journal
		o.Force = true
	})

	if len(daemon.submitted) != 1 {
		t.Fatalf("submitted = %v, want the forced submission to go through", daemon.submitted)
	}
	if got := daemon.submittedVia["job_slow"]; got != "acquire.submit_v3" {
		t.Fatalf("submitted via %q, want force to use acquire.submit_v3 with the consumer tag", got)
	}
	if len(second.Results) != 1 || second.Results[0].JobID == "" {
		t.Fatalf("results = %+v, want the forced work measured", second.Results)
	}
}

// With force the daemon stores a generated request id instead of the
// supplied one, so the run must move the durable association onto the
// committed id it reads back. Otherwise the next run can never reconcile
// this submission by request id.
func TestForceSuccessMovesJournalOntoCommittedRequestID(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_forced"] = []api.JobDetailV3{detailFor("job_forced", job.StateReady, "")}
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	report, err := Run(ctx, Options{
		Cohort:        cohortOf(work("forced", bench.AutonomousReady)),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Force:         true,
		Cleanup:       true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := report.Results[0]
	supplied := "livecohort-testrun-forced"
	committed := "forced-" + supplied
	if row.JobID != "job_forced" {
		t.Fatalf("job id = %q, want the forced job", row.JobID)
	}
	if row.RequestID != committed {
		t.Fatalf("request id = %q, want the committed id %q the lookup can find", row.RequestID, committed)
	}
	if !row.CreatedByRun {
		t.Fatalf("created_by_run = false, want the forced job owned by this run")
	}
	if got := daemon.submittedVia["job_forced"]; got != "acquire.submit_v3" {
		t.Fatalf("submitted via %q, want force to use acquire.submit_v3 with the consumer tag", got)
	}
	if got := daemon.consumerOf["job_forced"]; got != supplied {
		t.Fatalf("consumer = %q, want the run/work tag %q stored on the job", got, supplied)
	}
	if _, ok := daemon.committed[supplied]; ok {
		t.Fatalf("committed[%q] exists, want the daemon to store only the generated id", supplied)
	}
	entry, _, found, _ := daemon.JobForRequest(context.Background(), committed)
	if !found || entry != "job_forced" {
		t.Fatalf("lookup(%q) = %q, %v, want the committed job", committed, entry, found)
	}
	if entry, _, found, _ := daemon.JobForConsumer(context.Background(), supplied); !found || entry != "job_forced" {
		t.Fatalf("consumer lookup(%q) = %q, %v, want the forced job by its tag", supplied, entry, found)
	}
	unresolved, err := journal.Unresolved("test")
	if err != nil {
		t.Fatalf("Unresolved: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want the stale supplied intent resolved and the committed one accounted for", unresolved)
	}
}

// A tagged force entry resolves by its consumer tag alone. A request-id row
// under the supplied id — if one exists — belongs to a different acquisition
// than the tag does, so consulting it first would attribute an older job.
// Fail-first: the old request-first order returned the stale row.
func TestTaggedPriorEntryPrefersConsumerOverRequestRow(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.committed["livecohort-runone-slow"] = committedJob{id: "job_stale", state: job.StateReady}
	daemon.byConsumer["tag-runone-slow"] = committedJob{id: "job_fresh", state: job.StateQueued}
	skip, settled := resolvePriorSubmission(context.Background(), Options{Lookup: daemon},
		JournalEntry{CohortID: "test", WorkKey: "slow", RequestID: "livecohort-runone-slow", Consumer: "tag-runone-slow", RunID: "runone"})
	if !settled {
		t.Fatal("settled = false, want the consumer tag's job")
	}
	if skip.JobID != "job_fresh" {
		t.Fatalf("job = %q, want the consumer tag's job, not the stale request-id row", skip.JobID)
	}
}

// orderJournal records journal operations so tests can pin their sequence.
type orderJournal struct {
	Journal
	events []string
}

func (j *orderJournal) Note(entry JournalEntry) error {
	j.events = append(j.events, "note:"+entry.RequestID)
	return j.Journal.Note(entry)
}

func (j *orderJournal) Resolve(requestID string) error {
	j.events = append(j.events, "resolve:"+requestID)
	return j.Journal.Resolve(requestID)
}

func indexOf(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

// On a successful force the committed entry must go down BEFORE the intent
// is retired: a crash between the two then leaves both entries unresolved —
// recoverable by consumer tag — while the reverse order would leave no entry
// at all and invite a duplicate acquisition on the next run.
func TestForceSuccessNotesCommittedBeforeRetiringIntent(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_forced"] = []api.JobDetailV3{detailFor("job_forced", job.StateReady, "")}
	fileJournal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	journal := &orderJournal{Journal: fileJournal}
	supplied := "livecohort-testrun-forced"
	committed := "forced-" + supplied
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	if _, err := Run(ctx, Options{
		Cohort:        cohortOf(work("forced", bench.AutonomousReady)),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Force:         true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := indexOf(journal.events, "note:"+committed)
	resolve := indexOf(journal.events, "resolve:"+supplied)
	if note < 0 {
		t.Fatalf("events = %v, want the committed entry noted", journal.events)
	}
	if resolve < 0 {
		t.Fatalf("events = %v, want the supplied intent retired", journal.events)
	}
	if note > resolve {
		t.Fatalf("events = %v, want the committed note before the intent resolve: the reverse order loses the association on a crash", journal.events)
	}
}

// failCommittedJournal refuses one durable note and delegates the rest, so
// a test can prove the crash-gap order: the intent must survive when the
// committed note does not land.
type failCommittedJournal struct {
	Journal
	failID  string
	noteErr error
}

func (j failCommittedJournal) Note(entry JournalEntry) error {
	if entry.RequestID == j.failID {
		return j.noteErr
	}
	return j.Journal.Note(entry)
}

// If the committed note fails, the run must NOT retire the intent: the
// intent carrying the consumer tag is then the only record keeping the next
// run from submitting the paper again.
func TestForceCommittedNoteFailureKeepsIntentRecoverable(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_forced"] = []api.JobDetailV3{detailFor("job_forced", job.StateReady, "")}
	fileJournal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	supplied := "livecohort-testrun-forced"
	committed := "forced-" + supplied
	journal := failCommittedJournal{Journal: fileJournal, failID: committed, noteErr: errors.New("disk full for committed")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	report, err := Run(ctx, Options{
		Cohort:        cohortOf(work("forced", bench.AutonomousReady)),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Force:         true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := report.Results[0]
	if row.JobID != "job_forced" {
		t.Fatalf("job id = %q, want the submitted job still measured", row.JobID)
	}
	if row.RequestID != committed {
		t.Fatalf("request id = %q, want committed id %q", row.RequestID, committed)
	}
	if len(report.JournalFailures) == 0 {
		t.Fatal("journal failures empty, want the failed committed note named in the report")
	}
	unresolved, err := fileJournal.Unresolved("test")
	if err != nil {
		t.Fatalf("Unresolved: %v", err)
	}
	if len(unresolved) != 1 || unresolved[0].RequestID != supplied {
		t.Fatalf("unresolved = %+v, want the supplied intent kept for the next run", unresolved)
	}
	if unresolved[0].Consumer == "" {
		t.Fatalf("unresolved = %+v, want the consumer tag kept with the intent", unresolved)
	}
}

// A forced submission whose response is lost is named by its submit_v3
// consumer tag, which the daemon stores on the job row even as it replaces
// the request id. The run measures the committed job and never resubmits.
// Fail-first: on the old request-id lookup this stayed SubmitAmbiguous with
// an empty job id.
func TestForceLostSubmitRecoversByConsumerTag(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_flakyforce"] = []api.JobDetailV3{detailFor("job_flakyforce", job.StateUnavailable, "no legal candidates")}
	daemon.submitFails["livecohort-testrun-flakyforce"] = 1
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	report, err := Run(ctx, Options{
		Cohort:        cohortOf(work("flakyforce", bench.HonestUnavailable)),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Force:         true,
		Cleanup:       true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	row := report.Results[0]
	if row.JobID != "job_flakyforce" {
		t.Fatalf("job id = %q, want the committed job the consumer tag named", row.JobID)
	}
	if row.Outcome != Unavailable {
		t.Fatalf("outcome = %q, want the settled job rather than a submit verdict", row.Outcome)
	}
	if !row.CreatedByRun {
		t.Fatalf("created_by_run = false, want the recovered job owned by this run")
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d answered submissions, want none: recovery reads the store and never asks again", got)
	}
	unresolved, err := journal.Unresolved("test")
	if err != nil {
		t.Fatalf("Unresolved: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want the recovered submission accounted for", unresolved)
	}
}

// A later run reconciles an earlier force run's unaccounted submission by its
// consumer tag instead of submitting the paper again. Fail-first: on the old
// request-id lookup the tag-less read missed and the paper was duplicated.
func TestRerunReconcilesForceEntryByConsumerTag(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_slow"] = []api.JobDetailV3{detailFor("job_slow", job.StateReady, "")}
	daemon.submitFails["livecohort-runone-slow"] = 1
	journal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	cohort := cohortOf(work("slow", bench.AutonomousReady))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	first, err := Run(ctx, Options{
		Cohort:        cohort,
		Caller:        daemon,
		Lookup:        nil,
		Journal:       journal,
		RunID:         "runone",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Force:         true,
	})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := first.Results[0].Outcome; got != SubmitAmbiguous {
		t.Fatalf("first run outcome = %q, want %q without a lookup", got, SubmitAmbiguous)
	}
	second, err := Run(ctx, Options{
		Cohort:        cohort,
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "runtwo",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the earlier force submission reconciled, not resubmitted", second.Skipped)
	}
	if got := second.Skipped[0].JobID; got != "job_slow" {
		t.Fatalf("skipped job = %q, want the consumer tag's job", got)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %v, want no second acquisition for a paper the earlier force run already asked for", daemon.submitted)
	}
}

// Non-force submissions stay on the ratified submit_v2 with no consumer tag,
// so the ordinary path gains no new failure mode and no new attribution.
func TestNonForceSubmitsViaV2WithoutConsumer(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_plain"] = []api.JobDetailV3{detailFor("job_plain", job.StateReady, "")}
	report := runWith(t, daemon, cohortOf(work("plain", bench.AutonomousReady)), func(o *Options) {
		o.Cleanup = true
	})
	if len(report.Results) != 1 || report.Results[0].JobID != "job_plain" {
		t.Fatalf("results = %+v, want the submitted job measured", report.Results)
	}
	if got := daemon.submittedVia["job_plain"]; got != "acquire.submit_v2" {
		t.Fatalf("submitted via %q, want non-force to stay on acquire.submit_v2", got)
	}
	if got := daemon.consumerOf["job_plain"]; got != "" {
		t.Fatalf("consumer = %q, want no attribution on the ordinary path", got)
	}
}

// noteFailingJournal fails the intent write for one work key and delegates
// the rest to a real journal, so the run has earlier submissions to settle
// when the failure lands.
type noteFailingJournal struct {
	Journal
	failKey string
	noteErr error
}

func (j noteFailingJournal) Note(entry JournalEntry) error {
	if entry.WorkKey == j.failKey {
		return j.noteErr
	}
	return j.Journal.Note(entry)
}

// A journal intent the run cannot write for a later work must not abandon
// the jobs earlier works already submitted: they still need settlement,
// cleanup, and a report that names them.
func TestJournalIntentFailureStillSettlesAndCleansEarlierJobs(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_first"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
	}
	fileJournal, err := OpenFileJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	journal := noteFailingJournal{Journal: fileJournal, failKey: "second", noteErr: errors.New("disk full for second")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldTimeout, oldPoll := reconcileTimeout, reconcilePoll
	reconcileTimeout, reconcilePoll = 100*time.Millisecond, 5*time.Millisecond
	defer func() { reconcileTimeout, reconcilePoll = oldTimeout, oldPoll }()
	report, runErr := Run(ctx, Options{
		Cohort: cohortOf(
			work("first", bench.ReadyAfterHumanBoundary),
			work("second", bench.AutonomousReady),
		),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       journal,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
		Cleanup:       true,
	})
	if runErr == nil {
		t.Fatal("Run succeeded while a journal intent could not be written")
	}
	if !strings.Contains(runErr.Error(), "disk full for second") {
		t.Fatalf("error = %v, want the underlying write failure", runErr)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %+v, want both the settled job and the unsubmitted work reported", report.Results)
	}
	first := report.Results[0]
	if first.JobID != "job_first" {
		t.Fatalf("first job id = %q, want the submitted job settled", first.JobID)
	}
	if first.Outcome != HumanBoundary {
		t.Fatalf("first outcome = %q, want %q: the earlier job must still settle", first.Outcome, HumanBoundary)
	}
	second := report.Results[1]
	if second.Outcome != SubmitFailed {
		t.Fatalf("second outcome = %q, want %q: the intent failure must not submit", second.Outcome, SubmitFailed)
	}
	if second.JobID != "" {
		t.Fatalf("second job id = %q, want empty when the intent was never durable", second.JobID)
	}
	if !strings.Contains(second.StopDetail, "disk full for second") {
		t.Fatalf("second stop detail = %q, want the write failure named", second.StopDetail)
	}
	if len(daemon.cancelled) != 1 || daemon.cancelled[0] != "job_first" {
		t.Fatalf("cancelled = %v, want the earlier parked job cancelled despite the later failure", daemon.cancelled)
	}
	if len(report.Cancelled) != 1 || report.Cancelled[0] != "job_first" {
		t.Fatalf("report cancelled = %v, want the earlier job named", report.Cancelled)
	}
	if len(report.JournalFailures) == 0 {
		t.Fatal("journal failures empty, want the unwritten intent named in the report")
	}
}

// A journal the run cannot read may name a submission an earlier run left
// live. The run refuses before it submits anything rather than measuring
// on a safety record it could not consult.
func TestAnUnreadableJournalStopsTheRunBeforeAnySubmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"corrupt", "{not json"},
		{"truncated to empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("seeding the journal: %v", err)
			}
			journal, err := OpenFileJournal(path)
			if err != nil {
				t.Fatalf("OpenFileJournal: %v", err)
			}
			daemon := newFakeDaemon()
			_, runErr := Run(context.Background(), Options{
				Cohort:        cohortOf(work("w", bench.AutonomousReady)),
				Caller:        daemon,
				Lookup:        daemon,
				Journal:       journal,
				RunID:         "testrun",
				PerWorkBudget: time.Minute,
				Poll:          time.Microsecond,
				ParkSettle:    time.Microsecond,
			})
			if runErr == nil {
				t.Fatal("Run measured a cohort against a journal it could not read")
			}
			if len(daemon.submitted) != 0 {
				t.Fatalf("submitted = %v, want no submission behind an unreadable safety record", daemon.submitted)
			}
		})
	}
}

// An intent the run cannot durably record is a submission it must not
// make: the next run would have nothing to reconcile against.
func TestAnUnwritableJournalStopsTheRunBeforeSubmitting(t *testing.T) {
	daemon := newFakeDaemon()
	_, err := Run(context.Background(), Options{
		Cohort:        cohortOf(work("w", bench.AutonomousReady)),
		Caller:        daemon,
		Lookup:        daemon,
		Journal:       failingJournal{noteErr: errors.New("disk full")},
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		Poll:          time.Microsecond,
		ParkSettle:    time.Microsecond,
	})
	if err == nil {
		t.Fatal("Run submitted without recording the association that protects the next run")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the underlying write failure", err)
	}
	if len(daemon.submitted) != 0 {
		t.Fatalf("submitted = %v, want no submission before its intent is durable", daemon.submitted)
	}
}

// A request the run cannot mark accounted for stays unresolved, and the
// failure is named: silence would refuse the work on every later run.
func TestAFailedResolveIsNamedInTheReport(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_w"] = []api.JobDetailV3{detailFor("job_w", job.StateReady, "")}
	report := runWith(t, daemon, cohortOf(work("w", bench.AutonomousReady)), func(o *Options) {
		o.Journal = failingJournal{resolveErr: errors.New("read-only file system")}
	})
	if len(report.JournalFailures) == 0 {
		t.Fatal("a journal write this run could not make was swallowed")
	}
	var rendered strings.Builder
	if err := report.Render(&rendered); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered.String(), "JOURNAL FAILURES") ||
		!strings.Contains(rendered.String(), "read-only file system") {
		t.Fatalf("rendered report hides the journal failure:\n%s", rendered.String())
	}
}

// failingJournal fails the one durable write a test names and accepts the
// rest, so each failure mode is exercised on its own.
type failingJournal struct {
	noteErr       error
	resolveErr    error
	unresolvedErr error
}

func (j failingJournal) Note(JournalEntry) error { return j.noteErr }
func (j failingJournal) Resolve(string) error    { return j.resolveErr }
func (j failingJournal) Unresolved(string) ([]JournalEntry, error) {
	return nil, j.unresolvedErr
}

// A classified daemon refusal is an answer, not a lost response, so it
// stays a refusal and never triggers a reconciling resubmit.
func TestAClassifiedRefusalNeverReconciles(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitErr["livecohort-testrun-bad"] = &ipc.RemoteError{Code: "invalid_argument", Message: "no identity evidence"}

	report := runWith(t, daemon, cohortOf(work("bad", bench.AutonomousReady)), nil)

	if got := report.Results[0].Outcome; got != SubmitFailed {
		t.Fatalf("outcome = %q, want %q", got, SubmitFailed)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d, want no resubmit after an answered refusal", got)
	}
}

// A run cancelled before a work is submitted records the work it never
// sent instead of submitting into the interrupt and reconciling a job
// the operator never asked this run to start.
func TestACancelledRunNeverSubmitsIntoTheInterrupt(t *testing.T) {
	daemon := newFakeDaemon()
	ctx, stop := context.WithCancel(context.Background())
	stop()
	opts := Options{
		Cohort:        cohortOf(work("late", bench.AutonomousReady)),
		Caller:        daemon,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		ParkSettle:    time.Microsecond,
		Poll:          time.Microsecond,
	}
	report, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(daemon.submitted); got != 0 {
		t.Fatalf("submitted = %d, want no submission after the run was cancelled", got)
	}
	row := report.Results[0]
	if row.Outcome != TimedOut {
		t.Fatalf("outcome = %q, want %q for a work never sent", row.Outcome, TimedOut)
	}
	if row.JobID != "" {
		t.Fatalf("job id = %q, want empty for a work never sent", row.JobID)
	}
}

// Cancelling on an already interrupted measurement context must still
// reach the daemon: cleanup runs on its own bounded clock.
func TestCleanupCancelsAfterTheRunContextIsInterrupted(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_parked"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
	}
	observed := &Result{JobID: "job_parked", Outcome: HumanBoundary, CreatedByRun: true}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	cancelled, _ := cleanup(ctx, Options{Caller: daemon, Cleanup: true}, []*Result{observed})
	if len(cancelled) != 1 {
		t.Fatalf("cancelled = %v, want the parked job even though the run context is done", cancelled)
	}
	if !daemon.cancelSawLiveCtx {
		t.Fatal("jobs.cancel arrived on a dead context, so an interrupted run cannot clean up")
	}
}

// A cleanup the daemon will not confirm must be named in the rendered
// report with its job id, or an interrupted operator walks away while
// the job keeps running.
func TestAFailedCleanupNamesTheJobInTheRenderedReport(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_parked"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
	}
	daemon.refuseCancel = true
	report := runWith(t, daemon, cohortOf(work("parked", bench.ReadyAfterHumanBoundary)), func(o *Options) {
		o.Cleanup = true
	})
	row := report.Results[0]
	if !strings.Contains(row.CleanupNote, "job_parked") {
		t.Fatalf("cleanup note = %q, want the stranded job id", row.CleanupNote)
	}
	var rendered strings.Builder
	if err := report.Render(&rendered); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered.String(), "job_parked") || !strings.Contains(rendered.String(), "could NOT be confirmed cancelled") {
		t.Fatalf("rendered report hides the failed cleanup:\n%s", rendered.String())
	}
}

// A submission the daemon refuses is a recorded outcome, not a dropped work
// and not an aborted run.
func TestARefusedSubmissionIsRecordedAndTheRunContinues(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitErr["livecohort-testrun-bad"] = &ipc.RemoteError{Code: "invalid_argument", Message: "no identity evidence"}
	daemon.states["job_good"] = []api.JobDetailV3{detail(job.StateReady, "")}

	report := runWith(t, daemon, cohortOf(
		work("bad", bench.AutonomousReady),
		work("good", bench.AutonomousReady),
	), nil)

	if len(report.Results) != 2 {
		t.Fatalf("results = %d, want both works recorded", len(report.Results))
	}
	if got := report.Results[0].Outcome; got != SubmitFailed {
		t.Fatalf("outcome = %q, want %q", got, SubmitFailed)
	}
	if !strings.Contains(report.Results[0].StopDetail, "no identity evidence") {
		t.Fatalf("stop detail = %q, want the daemon's refusal", report.Results[0].StopDetail)
	}
	if report.AutonomousReady != 1 {
		t.Fatalf("autonomous_ready = %d, want the second work still measured", report.AutonomousReady)
	}
}

// The untried-candidate column is the direct measure of papio asking a
// person for something it had not finished trying. Zero and "nobody looked"
// must stay distinguishable.
func TestUntriedCandidatesCountsOnlyWorkThatStoppedOnAHuman(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_parked"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "manual_download", Status: "open"}),
	}
	daemon.states["job_dead"] = []api.JobDetailV3{detail(job.StateUnavailable, "no legal candidates")}

	report := runWith(t, daemon, cohortOf(
		work("parked", bench.ReadyAfterHumanBoundary),
		work("dead", bench.HonestUnavailable),
	), func(o *Options) { o.Inspector = fixedInspector{untried: 2, total: 5} })

	if report.GaveUpHolding != 1 {
		t.Fatalf("gave_up_holding = %d, want only the job that stopped on a human", report.GaveUpHolding)
	}
	if report.CandidatesInspected != 2 {
		t.Fatalf("candidates_inspected = %d, want both", report.CandidatesInspected)
	}

	without := runWith(t, newFakeDaemonWith(daemon.states), cohortOf(work("parked", bench.ReadyAfterHumanBoundary)), nil)
	if without.CandidatesInspected != 0 || without.GaveUpHolding != 0 {
		t.Fatalf("uninspected run reported %d inspected / %d holding, want 0/0 rather than a claim of zero untried",
			without.CandidatesInspected, without.GaveUpHolding)
	}
	if without.Results[0].UntriedCandidates != nil {
		t.Fatal("untried candidates must stay unfilled when no inspector ran")
	}
}

func newFakeDaemonWith(states map[string][]api.JobDetailV3) *fakeDaemon {
	daemon := newFakeDaemon()
	for id, seq := range states {
		daemon.states[id] = seq
	}
	return daemon
}

type fixedInspector struct{ untried, total int }

func (f fixedInspector) UntriedCandidates(context.Context, string) (int, int, error) {
	return f.untried, f.total, nil
}

// A cohort this binary cannot grade must fail before any job is submitted.
func TestRunRefusesAnInvalidCohortWithoutSubmittingAnything(t *testing.T) {
	daemon := newFakeDaemon()
	_, err := Run(context.Background(), Options{
		Cohort: bench.Cohort{SchemaVersion: "papio-bench-cohort/99", ID: "x", Works: []bench.Work{work("a", bench.AutonomousReady)}},
		Caller: daemon,
		RunID:  "testrun",
	})
	if err == nil {
		t.Fatal("Run accepted a cohort with an unknown schema version")
	}
	if len(daemon.submitted) != 0 {
		t.Fatalf("submitted %v before validating the cohort", daemon.submitted)
	}
}

// awaiting_human and needs_review are parking states, not terminal ones: the
// daemon advances out of them on its own. Recording the first sighting made
// the first real backlog run count a success as part of the human wall it
// exists to measure, so a parked observation must be confirmed before it is
// believed.
func TestAParkedObservationIsConfirmedBeforeItIsRecorded(t *testing.T) {
	daemon := newFakeDaemon()
	// Parked when first seen, ready two polls later — the measured shape of
	// job_d0acd0940b8d3294c0d14281f8 on 2026-09-19.
	daemon.states["job_late"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "resolved"}),
		detail(job.StateReady, "", job.HumanAction{Kind: "openurl_handoff", Status: "resolved"}),
	}

	// A settle window wide enough to span the late advance, and a clock the
	// test drives so the window is exercised without sleeping.
	clock := time.Unix(0, 0).UTC()
	report := runWith(t, daemon, cohortOf(work("late", bench.ReadyAfterHumanBoundary)), func(o *Options) {
		o.ParkSettle = 10 * time.Second
		o.Now = func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		}
	})

	if got := report.Results[0].Outcome; got != AssistedReady {
		t.Fatalf("outcome = %q, want %q — the parked sighting was recorded without confirming it", got, AssistedReady)
	}
	if report.GaveUpHolding != 0 {
		t.Fatalf("gave_up_holding = %d, want 0 for a job that finished", report.GaveUpHolding)
	}
}

// A job that really does stay parked must still be recorded once the window
// closes, or the confirmation turns every park into a timeout.
func TestAParkThatHoldsThroughTheWindowIsRecordedAsTheHumanBoundary(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.states["job_stays"] = []api.JobDetailV3{
		detail(job.StateAwaitingHuman, "", job.HumanAction{Kind: "openurl_handoff", Status: "open"}),
	}

	clock := time.Unix(0, 0).UTC()
	report := runWith(t, daemon, cohortOf(work("stays", bench.ReadyAfterHumanBoundary)), func(o *Options) {
		o.ParkSettle = 5 * time.Second
		o.PerWorkBudget = time.Hour
		o.Now = func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		}
	})

	if got := report.Results[0].Outcome; got != HumanBoundary {
		t.Fatalf("outcome = %q, want %q", got, HumanBoundary)
	}
	if got := report.Results[0].Verdict; got != VerdictMet {
		t.Fatalf("verdict = %q, want met", got)
	}
}
