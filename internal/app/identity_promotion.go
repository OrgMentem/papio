// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"

	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// identityWorkFields are the bibliographic fields that constitute canonical
// identity metadata before artifact validation.
func identityWorkFields(w work.Work) work.Work {
	return work.Work{
		DOI: w.DOI, PMID: w.PMID, ArXiv: w.ArXiv, ISBN: w.ISBN, OpenAlex: w.OpenAlex,
		Title: w.Title, Year: w.Year, Authors: append([]string(nil), w.Authors...),
	}
}

// identityAuthorConflict reports whether two author lists disagree when both are
// non-empty. Empty-on-either-side is not a conflict — sparse anchors deliberately
// omit authors.
func identityAuthorConflict(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// conflictsIdentity compares strong identifiers and bibliographic identity fields.
// conflicts() checks identifiers only and remains safe for non-identity access
// metadata checks elsewhere in the pipeline.
func conflictsIdentity(base, observed work.Work) bool {
	if conflicts(base, observed) {
		return true
	}
	if base.Title != "" && observed.Title != "" && base.Title != observed.Title {
		return true
	}
	if base.Year != 0 && observed.Year != 0 && base.Year != observed.Year {
		return true
	}
	return identityAuthorConflict(base.Authors, observed.Authors)
}

// fillMissingFromCandidate merges identity-bearing fields from observed into
// base when authority permits. Non-identity candidate fields are out of scope.
func fillMissingFromCandidate(base work.Work, c resolver.Candidate) work.Work {
	observed := c.ResolvedWork
	if !c.Authority.MayPromoteIdentity() {
		return base
	}
	if conflictsIdentity(base, observed) {
		return base
	}
	for _, pair := range []struct {
		dst   *string
		value string
	}{
		{&base.DOI, observed.DOI}, {&base.PMID, observed.PMID}, {&base.ArXiv, observed.ArXiv},
		{&base.ISBN, observed.ISBN}, {&base.OpenAlex, observed.OpenAlex}, {&base.Title, observed.Title},
	} {
		if *pair.dst == "" {
			*pair.dst = pair.value
		}
	}
	if len(base.Authors) == 0 && len(observed.Authors) > 0 {
		base.Authors = append([]string(nil), observed.Authors...)
	}
	if base.Year == 0 {
		base.Year = observed.Year
	}
	return base
}

// accumulatePromotedIdentity folds ranked candidates into one promotable identity
// view. Each candidate is checked against the running accumulation, not merely the
// pre-merge row.Work, so mutually incompatible identifiers cannot both stick.
func accumulatePromotedIdentity(base work.Work, ranked []resolver.Candidate) work.Work {
	accumulated := base
	for _, c := range ranked {
		accumulated = fillMissingFromCandidate(accumulated, c)
	}
	return accumulated
}

// enrichmentPersistWork returns the subset of enricher output that may be written
// durably. Every enricher reached from the enrich loop matches by title search,
// so its strong identifiers are its guess about which of several same-titled
// works the requester meant — never exact-echo verified — and adopting one makes
// the guess this job's canonical identity, which validation then compares the
// document against and confirms. Only bibliographic gaps the anchor left open
// may be filled, and only when the record does not contradict it.
//
// The anchor's completeness is deliberately NOT a licence here: a full
// title/authors/year tuple says the requester described the work, not that any
// search result naming an identifier for it describes the same one.
// The two failure meanings are separate returns on purpose: a record that
// CONTRADICTS the anchor must abandon the match, while a record that simply has
// nothing new to offer is a successful enrichment with no write. Folding both
// into one bool made an agreeing record read as a conflict.
func enrichmentPersistWork(anchor job.SubmittedIdentity, enriched work.Work) (out work.Work, changed, ok bool) {
	if conflictsIdentity(anchor.Work, enriched) {
		return work.Work{}, false, false
	}
	if anchor.Work.Title == "" && enriched.Title != "" {
		out.Title = enriched.Title
		changed = true
	}
	if len(anchor.Work.Authors) == 0 && len(enriched.Authors) > 0 {
		out.Authors = append([]string(nil), enriched.Authors...)
		changed = true
	}
	if anchor.Work.Year == 0 && enriched.Year != 0 {
		out.Year = enriched.Year
		changed = true
	}
	return out, changed, true
}

// mergeObservedInMemory fills missing fields on base from observed for the
// current pass only. Unlike fillMissingFromCandidate, this does not gate on
// evidence authority — callers use it only for ephemeral resolver input.
func mergeObservedInMemory(base, observed work.Work) work.Work {
	return fillMissing(base, observed)
}

// insufficientIdentitySettlementDetail names what additional authority would
// settle a title-only sparse anchor.
func insufficientIdentitySettlementDetail() map[string]any {
	return map[string]any{
		"reason": "insufficient_identity_evidence",
		"settlement": []string{
			"a second resolver agreeing on the same work",
			"a matching identifier from another registry",
			"human confirmation of identity",
		},
	}
}

// validationTarget returns the work identity PDF validation must compare against.
// When the anchor is attested it wins over mutable row.Work; otherwise behavior
// is unchanged for legacy rows.
func validationTarget(anchor job.SubmittedIdentity, row *job.Row) work.Work {
	if anchor.Attested {
		return anchor.Work
	}
	if row == nil {
		return work.Work{}
	}
	return row.Work
}

// sameWorkIdentity compares identity-bearing fields only.
func sameWorkIdentity(a, b work.Work) bool {
	return sameWork(identityWorkFields(a), identityWorkFields(b))
}

// settleInsufficientIdentity ends a job that cannot verify canonical identity
// from candidate-derived facts alone.
func (s *Service) settleInsufficientIdentity(ctx context.Context, row *job.Row, from string) error {
	detail := insufficientIdentitySettlementDetail()
	err := s.Jobs.Transition(ctx, row.ID, from, job.StateUnavailable, detail,
		job.WithTerminalReason(job.TerminalReasonInsufficientIdentityEvidence))
	if err == nil {
		s.recordStandaloneOutcome(ctx, row)
	}
	return err
}
