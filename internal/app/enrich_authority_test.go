// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/work"
)

func titleOnlyRequest(id, title string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      id,
		Title:          title,
		Authors:        []string{"Ada Lovelace"},
		Year:           2024,
		DesiredVersion: "any",
	}
}

// A metadata enricher matches by SEARCH. `matchesTitleSearch` accepts a
// title-only submission on the normalized title alone — no year, no author
// list — so the matched record's own DOI is a guess about which of several
// same-titled works the requester meant. Persisting it makes the guess the
// job's canonical identity, and everything downstream, validation included,
// then reads it as what was asked for. Gaps the requester left open may be
// filled; strong identifiers may not be invented.
func TestEnrichDoesNotAdoptASearchDerivedIdentifier(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	enricher := &fakeEnricher{
		matched: true,
		// Agrees on every bibliographic field the requester gave, and adds one
		// strong identifier of its own. Nothing here is a conflict; the only
		// question is whether a search may name the work's canonical id.
		result: work.Work{
			DOI:     "10.1002/wrong-same-title",
			Title:   "A Common Review Title",
			Authors: []string{"Ada Lovelace"},
			Year:    2024,
		},
	}
	svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: config.SourceCrossrefMetadata, Enricher: enricher}}
	svc.Config.Sources[config.SourceCrossrefMetadata] = config.Source{Enabled: true}

	id, err := svc.Submit(ctx, titleOnlyRequest("wr_enrich_authority_01", "A Common Review Title"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := jobs.SubmittedIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.enrich(ctx, row, anchor); err != nil {
		t.Fatal(err)
	}
	if enricher.calls != 1 {
		t.Fatalf("enricher calls = %d, want the search to have run", enricher.calls)
	}

	stored, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Work.DOI != "" {
		t.Fatalf("persisted DOI = %q, want none: a title search cannot verify which work it found", stored.Work.DOI)
	}
	// A submission must carry a full title/authors/year tuple when it has no
	// identifier (protocol.go:248), so an identifier-free anchor has no
	// bibliographic gap to fill: the identifier is the only thing enrichment
	// could contribute here, and it is the one thing it may not.
	if stored.Work.Title != "A Common Review Title" || stored.Work.Year != 2024 {
		t.Fatalf("stored work = %+v, want the submitted bibliography intact", stored.Work)
	}
	// In-memory the identifier still serves this pass: resolvers may look for
	// candidates with it. That is what search evidence is licensed to do.
	if row.Work.DOI != "10.1002/wrong-same-title" {
		t.Fatalf("in-memory DOI = %q, want the unpersisted identifier available to this pass", row.Work.DOI)
	}
}

// An enricher that contradicts an identifier the requester supplied is refused
// outright rather than merged: this is the pre-existing conflict rule, and the
// authority gate must not have widened it into an acceptance.
func TestEnrichStillRefusesAContradictingRecord(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	enricher := &fakeEnricher{
		matched: true,
		result:  work.Work{DOI: "10.1002/different", Title: "Other Paper", Year: 2001},
	}
	svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: config.SourceCrossrefMetadata, Enricher: enricher}}
	svc.Config.Sources[config.SourceCrossrefMetadata] = config.Source{Enabled: true}

	request := doiRequest("wr_enrich_authority_02")
	request.Title = "Requested Paper"
	id, err := svc.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := jobs.SubmittedIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.enrich(ctx, row, anchor); err != nil {
		t.Fatal(err)
	}
	stored, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Work.Title == "Other Paper" || stored.Work.Year == 2001 {
		t.Fatalf("stored work = %+v, want a contradicting record refused whole", stored.Work)
	}
}

// The self-confirming loop this closes. A row can carry a strong identifier the
// requester never attested — every job persisted before the item-5 authority
// gate existed may, since the old enrich path wrote a title match's own DOI
// durably (see TestResolveEnrichesTitleOnlyWorkForThisPassOnly, whose contract
// this replaced). Validating against that identifier asks the document to
// confirm a guess some earlier pass made about it, and the document agrees,
// because the guess came from a record describing it. Validation must be asked
// what the REQUESTER named, which is what the immutable anchor holds.
func TestValidationComparesAgainstTheAttestedAnchor(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)

	var seen []work.Work
	svc.Validate = func(_ context.Context, _ string, _ string, target work.Work) (pdf.ValidationReport, error) {
		seen = append(seen, target)
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"title match"}},
		}, nil
	}
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf",
		ResolvedWork: work.Work{DOI: "10.1234/from-search"},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)

	id, err := svc.Submit(ctx, titleOnlyRequest("wr_validate_anchor_01", "A Common Review Title"))
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what the pre-gate enrich path did: a search-derived identifier
	// written onto the work durably, with nothing having verified it.
	if _, err := jobs.FillWorkMetadata(ctx, id, work.Work{DOI: "10.1234/from-search"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if row.Work.DOI != "10.1234/from-search" {
		t.Fatalf("row.Work.DOI = %q, want the unattested identifier this test guards", row.Work.DOI)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("validation never ran; this test would prove nothing")
	}
	anchor, err := jobs.SubmittedIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !anchor.Attested || anchor.Work.DOI != "" {
		t.Fatalf("anchor = %+v, want an attested identifier-free anchor", anchor.Work)
	}
	for i, target := range seen {
		if target.DOI != "" {
			t.Fatalf("validation %d was asked to confirm a search-derived DOI %q; the requester attested none", i, target.DOI)
		}
		if target.Title != "A Common Review Title" {
			t.Fatalf("validation %d target = %+v, want the submitted identity", i, target)
		}
	}
}

// Legacy rows predate the anchor and have nothing attested; they must keep
// validating against row.Work rather than against an empty work, which would
// make every identity check vacuous on exactly the oldest data.
func TestValidationTargetFallsBackForUnattestedRows(t *testing.T) {
	row := &job.Row{Work: work.Work{DOI: "10.1002/legacy", Title: "Legacy"}}
	got := validationTarget(job.SubmittedIdentity{}, row)
	if got.DOI != "10.1002/legacy" || got.Title != "Legacy" {
		t.Fatalf("target = %+v, want row.Work for an unattested row", got)
	}
	attested := job.SubmittedIdentity{Attested: true, Work: work.Work{DOI: "10.1002/anchor"}}
	if got := validationTarget(attested, row); got.DOI != "10.1002/anchor" {
		t.Fatalf("target = %+v, want the anchor to win when attested", got)
	}
}
