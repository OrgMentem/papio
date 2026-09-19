// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/bench"
	"papio/internal/job"
)

// fakeDaemon answers the three methods a run uses, marshalling through JSON
// the way the real ipc client does so a shape mismatch fails here too.
type fakeDaemon struct {
	// states[jobID] is the sequence of job rows successive polls observe;
	// the last entry repeats once exhausted.
	states    map[string][]api.JobDetailV3
	polls     map[string]int
	existing  map[string]bool
	cancelled []string
	submitted []string
	submitErr map[string]error
	nextID    int
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		states:    map[string][]api.JobDetailV3{},
		polls:     map[string]int{},
		existing:  map[string]bool{},
		submitErr: map[string]error{},
	}
}

func (f *fakeDaemon) Call(_ context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		var decoded submitParams
		if err := roundTrip(params, &decoded); err != nil {
			return err
		}
		key := decoded.Request.RequestID
		if err := f.submitErr[key]; err != nil {
			return err
		}
		if decoded.AutoImport == nil || *decoded.AutoImport {
			return errors.New("a measurement must submit with auto_import false")
		}
		f.nextID++
		id := f.idFor(key)
		f.submitted = append(f.submitted, id)
		return roundTrip(api.SubmitV2Result{JobID: id, Existing: f.existing[key]}, result)
	case "jobs.get_v3":
		var decoded map[string]string
		if err := roundTrip(params, &decoded); err != nil {
			return err
		}
		id := decoded["job_id"]
		seq := f.states[id]
		if len(seq) == 0 {
			return errors.New("no such job " + id)
		}
		i := f.polls[id]
		if i >= len(seq) {
			i = len(seq) - 1
		}
		f.polls[id] = i + 1
		return roundTrip(seq[i], result)
	case "jobs.cancel":
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
	rows := make([]api.ActionRow, 0, len(actions))
	for _, action := range actions {
		rows = append(rows, api.ActionRow{HumanAction: action})
	}
	return api.JobDetailV3{
		Job:     &api.JobRow{Row: job.Row{ID: "job", State: state, TerminalReason: terminalReason}},
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
	opts := Options{
		Cohort:        cohort,
		Caller:        daemon,
		RunID:         "testrun",
		PerWorkBudget: time.Minute,
		ParkSettle:    time.Microsecond,
		Poll:          time.Microsecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	report, err := Run(context.Background(), opts)
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

// A submission the daemon refuses is a recorded outcome, not a dropped work
// and not an aborted run.
func TestARefusedSubmissionIsRecordedAndTheRunContinues(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.submitErr["livecohort-testrun-bad"] = errors.New("invalid_argument: no identity evidence")
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
