// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"fmt"
	"sort"
	"strings"

	"papio/internal/api"
	"papio/internal/bench"
	"papio/internal/job"
)

// Outcome is the closed set of results an UNATTENDED live run can observe.
//
// It is deliberately not bench.ExpectedClass. A cohort's expected_class
// records what a human judge believes the acquisition should look like once
// everyone has played their part; an unattended run measures what papio does
// with nobody watching, and those differ in exactly the place that matters —
// a job parked on a human is a finished measurement here and an unfinished
// acquisition there. Keeping two vocabularies makes that gap a reported
// number instead of a rounding decision.
type Outcome string

const (
	// AutonomousReady is the only outcome that required no human at all:
	// the job reached ready or imported and never opened a human action.
	AutonomousReady Outcome = "autonomous_ready"
	// AssistedReady reached ready or imported, but opened a human action on
	// the way. Under an unattended run nobody answered it, so the job
	// finished despite the prompt — which usually means the prompt was
	// unnecessary.
	AssistedReady Outcome = "assisted_ready"
	// HumanBoundary is a job parked in awaiting_human. Correct behaviour for
	// a work that genuinely needs a person, and the dominant stop in
	// papio's current field data.
	HumanBoundary Outcome = "human_boundary"
	// IdentityReview is a job parked in needs_review.
	IdentityReview Outcome = "identity_review"
	// Unavailable is a terminal unavailable.
	Unavailable Outcome = "unavailable"
	// Failed is a terminal failed.
	Failed Outcome = "failed"
	// Cancelled is a terminal cancelled that the run did not itself cause.
	Cancelled Outcome = "cancelled"
	// TimedOut means the job never settled inside the per-work budget. It is
	// an outcome, not an error: a job that cannot settle in the budget is a
	// real result about papio, and dropping it would flatter every rate.
	TimedOut Outcome = "timed_out"
	// SubmitFailed means the daemon refused the submission.
	SubmitFailed Outcome = "submit_failed"
)

// readyOutcomes are the outcomes in which papio produced a filed artifact.
func (o Outcome) ready() bool { return o == AutonomousReady || o == AssistedReady }

// Verdict grades one observed outcome against the cohort's expectation.
type Verdict string

const (
	// VerdictMet means the observation satisfies the expectation.
	VerdictMet Verdict = "met"
	// VerdictMissed means it does not, without any safety implication.
	VerdictMissed Verdict = "missed"
	// VerdictWrongAccept means papio filed an artifact for a work the judge
	// said it should not have filed one for. This is the primary,
	// unconditional gate: a wrong paper under the right citation is the
	// worst outcome papio has, and a run that raises this count does not
	// ship whatever it does to throughput. Same discipline as
	// dev/identity-corpus.md.
	VerdictWrongAccept Verdict = "wrong_accept"
)

// classify derives the observed outcome from a settled job detail.
//
// The human-action test is "was one ever opened", not "is one open now":
// a resolved action still proves papio stopped and asked, which is the
// distinction between an autonomous acquisition and an assisted one.
func classify(detail *api.JobDetailV3) (Outcome, error) {
	if detail == nil || detail.Job == nil {
		return "", fmt.Errorf("livecohort: daemon returned no job row")
	}
	asked := len(detail.Actions) > 0
	switch detail.Job.State {
	case job.StateReady, job.StateImported:
		if asked {
			return AssistedReady, nil
		}
		return AutonomousReady, nil
	case job.StateAwaitingHuman:
		return HumanBoundary, nil
	case job.StateNeedsReview:
		return IdentityReview, nil
	case job.StateUnavailable:
		return Unavailable, nil
	case job.StateFailed:
		return Failed, nil
	case job.StateCancelled:
		return Cancelled, nil
	}
	return "", fmt.Errorf("livecohort: job %s settled in unexpected state %q", detail.Job.ID, detail.Job.State)
}

// satisfies reports whether observed meets expected.
//
// ReadyAfterHumanBoundary is met by HumanBoundary because an unattended run
// stops at exactly the boundary that expectation names — reaching it is the
// whole of what papio is responsible for here. It is also met by
// AssistedReady, which is strictly better: papio asked and then finished
// anyway.
func satisfies(expected bench.ExpectedClass, observed Outcome) bool {
	switch expected {
	case bench.AutonomousReady:
		return observed == AutonomousReady
	case bench.ReadyAfterHumanBoundary:
		return observed == HumanBoundary || observed == AssistedReady
	case bench.HonestUnavailable:
		return observed == Unavailable
	case bench.IdentityReview:
		return observed == IdentityReview
	}
	return false
}

// grade returns the verdict for one work.
//
// A wrong accept is scored ahead of a plain miss: filing an artifact for a
// work judged unavailable, or filing one a judge said needed human identity
// review, is a safety failure even though it also happens to be a miss.
func grade(expected bench.ExpectedClass, observed Outcome) Verdict {
	if observed.ready() && (expected == bench.HonestUnavailable || expected == bench.IdentityReview) {
		return VerdictWrongAccept
	}
	if satisfies(expected, observed) {
		return VerdictMet
	}
	return VerdictMissed
}

// stopDetail is the one line that says WHY a job stopped where it did.
//
// It is the field the report exists to surface: today's `papio status`
// prints the same sentence for forty-five different jobs, so a measurement
// that only counted states would reproduce that blindness at a larger scale.
func stopDetail(detail *api.JobDetailV3, observed Outcome) string {
	if detail == nil || detail.Job == nil {
		return ""
	}
	switch observed {
	case HumanBoundary, IdentityReview:
		if kinds := openActionKinds(detail.Actions); len(kinds) > 0 {
			return strings.Join(kinds, "+")
		}
		if reason := lastTransitionReason(detail.Events); reason != "" {
			return reason
		}
		return "parked with no open action"
	case Unavailable, Failed, Cancelled:
		if detail.Job.TerminalReason != "" {
			return detail.Job.TerminalReason
		}
		return string(observed)
	case AssistedReady:
		return strings.Join(allActionKinds(detail.Actions), "+")
	}
	return ""
}

// openActionKinds lists the distinct kinds of the still-open actions, sorted
// so two runs of one cohort render identically.
func openActionKinds(rows []api.ActionRow) []string {
	return actionKinds(rows, func(row api.ActionRow) bool { return row.Status == "open" })
}

func allActionKinds(rows []api.ActionRow) []string {
	return actionKinds(rows, func(api.ActionRow) bool { return true })
}

func actionKinds(rows []api.ActionRow, keep func(api.ActionRow) bool) []string {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if keep(row) {
			seen[row.Kind] = true
		}
	}
	kinds := make([]string, 0, len(seen))
	for kind := range seen {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// lastTransitionReason reads the reason off the final job.transition event.
//
// Events arrive as map[string]any because that is the shape jobs.get_v3
// publishes; this reads defensively rather than asserting, because a missing
// reason is a blank cell in a report, never a failed measurement.
func lastTransitionReason(events []map[string]any) string {
	reason := ""
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail == nil {
			continue
		}
		if value, ok := detail["reason"].(string); ok && value != "" {
			reason = value
		}
	}
	return reason
}
