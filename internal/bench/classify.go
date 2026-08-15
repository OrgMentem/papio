// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"fmt"

	"papio/internal/job"
)

// ReportClass is the outcome bench assigns one work under one overlay. It
// extends ExpectedClass with FixtureMissing, a bench-only state a cohort's
// expected_class enum must never contain (Cohort.Validate rejects it):
// "we never ran this work" is not a judgement about papio, and conflating
// it with HonestUnavailable would silently launder missing fixture coverage
// into a real finding.
type ReportClass string

// The report's outcome vocabulary: ExpectedClass's four values plus
// ClassFixtureMissing.
const (
	ClassAutonomousReady                     = ReportClass(AutonomousReady)
	ClassReadyAfterHumanBoundary             = ReportClass(ReadyAfterHumanBoundary)
	ClassHonestUnavailable                   = ReportClass(HonestUnavailable)
	ClassIdentityReview                      = ReportClass(IdentityReview)
	ClassFixtureMissing          ReportClass = "fixture_missing"
)

// Classify maps one settled job's outcome to a ReportClass.
//
//   - state is the job's final job.Row.State.
//   - terminal is only meaningful when state is StateUnavailable or
//     StateCancelled; it is job.NormalizeTerminalReason's output (job.Get
//     already normalizes it, so callers can pass job.Row.TerminalReason
//     straight through).
//   - humanEpisodes is the number of blocking (non-informational)
//     human_actions rows the job opened — see runner.go's episode count,
//     which excludes the conservative-mode openurl_available advisory the
//     same way dev/post-build-followups.md item 1 does.
//
// The mapping is exhaustive over job.go's TerminalReason vocabulary by a
// named switch, not a default case: TestClassifyCoversEveryDeclaredTerminalReason
// fails the moment job.go grows a reason this switch does not know about,
// so an unrecognized terminal reason is a build-time bench defect, never a
// silently misfiled report row (an unknown reason returns an error, per
// dev/post-build-followups.md item 4).
func Classify(state string, terminal job.TerminalReason, humanEpisodes int) (ReportClass, error) {
	switch state {
	case job.StateReady, job.StateImported:
		if humanEpisodes == 0 {
			return ClassAutonomousReady, nil
		}
		return ClassReadyAfterHumanBoundary, nil
	case job.StateUnavailable, job.StateCancelled:
		switch terminal {
		case job.TerminalReasonReviewRejected:
			// The only terminal reason job.go's own comments and call site
			// (ResolveReview rejecting a verify_identity human action) tie
			// to identity/validation review.
			return ClassIdentityReview, nil
		case job.TerminalReasonNoLegalCandidates,
			job.TerminalReasonTemporarySourceFailuresDidNotClear,
			job.TerminalReasonTemporaryCandidateFailuresDidNotClear,
			job.TerminalReasonCandidatesExhausted,
			job.TerminalReasonNoIdentifier,
			job.TerminalReasonDOINotRegistered,
			job.TerminalReasonNoEntitlement,
			job.TerminalReasonBrowserRejected,
			// DocumentDeliveryAvailable ends THIS job's direct-fetch attempt
			// without an artifact in hand — a document-delivery (ILL)
			// request may still be pursued elsewhere, but from this job's
			// perspective that is the same "not directly available"
			// outcome NoEntitlement already reports, so it shares the
			// bucket.
			job.TerminalReasonDocumentDeliveryAvailable,
			job.TerminalReasonCancelledByUser,
			job.TerminalReasonBrowserCancelled,
			job.TerminalReasonUserDismissed,
			// InsufficientIdentityEvidence is an honest "cannot verify from
			// candidate-derived facts alone" outcome — it reports that a
			// title-only anchor lacks independent authority, not a human
			// review rejection, so it belongs with HonestUnavailable.
			job.TerminalReasonInsufficientIdentityEvidence:
			return ClassHonestUnavailable, nil
		default:
			return "", fmt.Errorf("bench: unrecognized terminal reason %q for state %q — classify.go's mapping must cover every job.TerminalReason", terminal, state)
		}
	default:
		return "", fmt.Errorf("bench: job settled in non-terminal state %q — a hermetic run must drive every scripted work to ready, imported, unavailable, or cancelled", state)
	}
}
