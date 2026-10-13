package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	bindingISBN  = "9789240114487"
	bindingTitle = "Mental health atlas 2024"
)

// bookText is a whole book's extracted text whose cover is letter-spaced
// display type: the article rules cannot read its title off page one, but its
// title page and copyright page follow.
func bookText(printedISBN string) string {
	return "Me n t a l\nHe a l t h\n\nAt l a s\n2024\n\f" +
		"Mental Health Atlas 2024\nISBN " + printedISBN + " (electronic version)\n© World Health Organization 2025\n\f" +
		strings.Repeat("Mental health services are described in the chapters that follow. ", 40)
}

// bookValidator is the production validator's identity stage on fixed text: the
// article rules judge the target the app passes, exactly as pdf.Validate does.
// It records that target so a test can see what the file was compared with.
func bookValidator(text string, seen *work.Work) ValidateFunc {
	return func(_ context.Context, _, _ string, target work.Work) (pdf.ValidationReport, error) {
		*seen = target
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 98},
			Text:       pdf.TextReport{Chars: int64(len(text)), Excerpt: text},
			Identity:   pdf.MatchIdentityWithThreshold(text, target, 0.6),
		}, nil
	}
}

const bookURL = "https://repo.example.org/server/api/core/bitstreams/b/content"

// bookCandidate is the repository's candidate. A binding is stamped with the
// candidate's own URL, as the source does: the statement is about that file.
func bookCandidate(binding *resolver.ISBNBinding) resolver.Candidate {
	if binding != nil {
		bound := *binding
		bound.URL = bookURL
		binding = &bound
	}
	return resolver.Candidate{
		Source: "open_books", URL: bookURL,
		Version: resolver.VersionUnknown, AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-nc-sa-3.0-igo",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.95,
		Authority:    resolver.AuthorityExactEcho,
		ISBNBinding:  binding,
		ResolvedWork: work.Work{ISBN: bindingISBN, Title: bindingTitle, Year: 2025},
	}
}

func runBookJob(t *testing.T, request protocol.WorkRequest, candidate resolver.Candidate, text string) (*job.Row, work.Work) {
	t.Helper()
	svc, jobs := exhaustionService(t)
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "open_books", cands: []resolver.Candidate{candidate}}, Policy: svc.Resolvers[0].Policy}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	var target work.Work
	svc.Validate = bookValidator(text, &target)
	return processOnce(t, svc, jobs, request), target
}

func isbnRequest(id string, mutate func(*protocol.WorkRequest)) protocol.WorkRequest {
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: id,
		Identifiers: &protocol.Identifiers{ISBN: bindingISBN}, DesiredVersion: "any",
	}
	if mutate != nil {
		mutate(&request)
	}
	return request
}

// The WHO atlas was fetchable and still parked: the requester gave an ISBN and
// no title, so the validator had nothing to compare the file with.
func TestISBNBoundBookWithMatchingFrontMatterReachesReady(t *testing.T) {
	row, target := runBookJob(t, isbnRequest("wr_isbn_bound_ready", nil),
		bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}),
		bookText("978-92-4-011448-7"))
	if row.State != job.StateReady {
		t.Fatalf("state = %q (reason %q), want ready", row.State, row.TerminalReason)
	}
	if target.ISBN != bindingISBN {
		t.Fatalf("validator target = %+v, want the requested ISBN", target)
	}
}

// The record's creators are promoted onto the job like its title (it is an
// exact-echo candidate), so they read back as an "authored" target. They are
// the repository's words, not the requester's, and must not keep a book that
// the record, the printed ISBN and the title all agree on in review.
func TestPromotedRecordCreatorsDoNotBlockABoundBook(t *testing.T) {
	candidate := bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	candidate.ResolvedWork.Authors = []string{"World Health Organization"}
	row, target := runBookJob(t, isbnRequest("wr_isbn_bound_creators", nil), candidate, bookText("978-92-4-011448-7"))
	if len(target.Authors) == 0 {
		t.Fatal("test premise: the promoted creators should have reached the generic validator")
	}
	if row.State != job.StateReady {
		t.Fatalf("state = %q, want ready", row.State)
	}
}

// Authors the requester DID supply stay in force: the book does not print them,
// so the file is parked rather than accepted on the repository's word.
func TestRequestedAuthorsKeepTheArticleRulesForABoundBook(t *testing.T) {
	row, _ := runBookJob(t, isbnRequest("wr_isbn_bound_authors", func(r *protocol.WorkRequest) {
		r.Authors = []string{"Jane Smith"}
	}), bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}), bookText("978-92-4-011448-7"))
	if row.State == job.StateReady {
		t.Fatal("requested authors the book does not print were ignored")
	}
}

func TestISBNBoundBookIsParkedWhenTheFilePrintsAnotherISBN(t *testing.T) {
	row, _ := runBookJob(t, isbnRequest("wr_isbn_bound_other", nil),
		bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}),
		bookText("978-92-4-099999-1"))
	if row.State != job.StateNeedsReview {
		t.Fatalf("state = %q, want needs_review: a repository's claim about its record is not a claim about the bytes", row.State)
	}
}

// Without the binding the same bytes park: it is the binding, not a loosened
// identity rule, that lets the file through.
func TestTheSameBookWithoutABindingStaysParked(t *testing.T) {
	row, _ := runBookJob(t, isbnRequest("wr_isbn_unbound", nil), bookCandidate(nil), bookText("978-92-4-011448-7"))
	if row.State != job.StateNeedsReview {
		t.Fatalf("state = %q, want needs_review", row.State)
	}
}

// A title the requester supplied is the title the file must carry; the record's
// never replaces it.
func TestBindingNeverOverridesARequestedTitle(t *testing.T) {
	row, target := runBookJob(t, isbnRequest("wr_isbn_titled", func(r *protocol.WorkRequest) {
		r.Title = "A Completely Different Book"
	}), bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}), bookText("978-92-4-011448-7"))
	if target.Title != "A Completely Different Book" {
		t.Fatalf("validator target title = %q, want the requested title", target.Title)
	}
	if row.State == job.StateReady {
		t.Fatal("a file that does not carry the requested title reached ready on the record's title")
	}
}

// A binding for another ISBN than the request's is ignored outright.
func TestBindingForAnotherISBNIsIgnored(t *testing.T) {
	row, _ := runBookJob(t, isbnRequest("wr_isbn_mismatched_binding", nil),
		bookCandidate(&resolver.ISBNBinding{ISBN: "9789240115873", Title: bindingTitle}),
		bookText("978-92-4-011448-7"))
	if row.State == job.StateReady {
		t.Fatal("a binding for a different ISBN accepted the file")
	}
}

// A chapter is requested by its DOI; the book its ISBN names is not the file.
func TestBindingNeverAppliesToAWorkWithAnotherIdentifier(t *testing.T) {
	row, _ := runBookJob(t, isbnRequest("wr_isbn_with_doi", func(r *protocol.WorkRequest) {
		r.Identifiers.DOI = "10.1000/chapter"
	}), bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}), bookText("978-92-4-011448-7"))
	if row.State == job.StateReady {
		t.Fatal("a DOI-bearing request was accepted on an ISBN binding")
	}
}

// applyISBNBinding is a no-op unless every precondition holds; the gates are
// checked on the report itself so a pass cannot be mistaken for a skipped one.
func TestApplyISBNBindingLeavesTheReportAloneUnlessEveryGateHolds(t *testing.T) {
	svc, _ := newTestService(t)
	review := pdf.IdentityDecision{Result: pdf.IdentityReview, Evidence: []string{"generic"}}
	base := func() pdf.ValidationReport {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true},
			Text:       pdf.TextReport{Excerpt: bookText("978-92-4-011448-7")},
			Identity:   review,
		}
	}
	binding := &resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}
	anchor := func(w work.Work) job.SubmittedIdentity {
		return job.SubmittedIdentity{Attested: true, Work: w, Fields: map[string]bool{"isbn": true}}
	}
	row := &job.Row{}

	if got := svc.applyISBNBinding(base(), anchor(work.Work{ISBN: bindingISBN}), row, binding); got.Identity.Result != pdf.IdentityPass {
		t.Fatalf("control: all gates hold but result = %+v", got.Identity)
	}
	cases := map[string]func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding){
		"no binding": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			return base(), anchor(work.Work{ISBN: bindingISBN}), nil
		},
		"a DOI-bearing anchor": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			return base(), anchor(work.Work{ISBN: bindingISBN, DOI: "10.1000/chapter"}), binding
		},
		"another ISBN": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			return base(), anchor(work.Work{ISBN: "9789240115873"}), binding
		},
		"text needing review": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			r := base()
			r.Text.NeedsReview = true
			return r, anchor(work.Work{ISBN: bindingISBN}), binding
		},
		"invalid payload": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			r := base()
			r.Payload.OK = false
			return r, anchor(work.Work{ISBN: bindingISBN}), binding
		},
		"invalid structure": func() (pdf.ValidationReport, job.SubmittedIdentity, *resolver.ISBNBinding) {
			r := base()
			r.Structural.Valid = false
			return r, anchor(work.Work{ISBN: bindingISBN}), binding
		},
	}
	for name, build := range cases {
		report, a, b := build()
		before := report.Identity
		if got := svc.applyISBNBinding(report, a, row, b); got.Identity.Result != before.Result || len(got.Identity.Evidence) != len(before.Evidence) {
			t.Errorf("%s: identity changed to %+v", name, got.Identity)
		}
	}
}

// A different EDITION with the same title must not reach ready. The record's
// promoted creators made the generic validator park the file (the book does not
// print them); re-judging without them lets the article rules pass a book whose
// title line is right, so the bound ISBN has to be required of that Pass too.
func TestDifferentEditionWithTheSameTitleIsNotAcceptedOnTheBinding(t *testing.T) {
	candidate := bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	candidate.ResolvedWork.Authors = []string{"Jane Smith"}
	otherEdition := "Mental Health Atlas 2024\nISBN 978-92-4-099999-1\nWorld Health Organization\n\f" +
		strings.Repeat("Mental health services are described in the chapters that follow. ", 40)
	row, target := runBookJob(t, isbnRequest("wr_isbn_other_edition", nil), candidate, otherEdition)
	if len(target.Authors) == 0 {
		t.Fatal("test premise: the promoted creators should have reached the generic validator")
	}
	if row.State == job.StateReady {
		t.Fatal("a file printing another edition's ISBN reached ready on the repository's binding")
	}
	// The same file printing the bound ISBN is accepted.
	row, _ = runBookJob(t, isbnRequest("wr_isbn_same_edition", nil), candidate,
		strings.Replace(otherEdition, "978-92-4-099999-1", "978-92-4-011448-7", 1))
	if row.State != job.StateReady {
		t.Fatalf("state = %q, want ready for the bound ISBN", row.State)
	}
}

// A generic Pass reached before the binding is kept as it was.
func TestAGenericPassIsKeptWhenABindingIsPresent(t *testing.T) {
	svc, _ := newTestService(t)
	passed := pdf.ValidationReport{
		Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true},
		Text:     pdf.TextReport{Excerpt: "Mental Health Atlas 2024\nISBN 978-92-4-099999-1\n"},
		Identity: pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"generic"}},
	}
	anchor := job.SubmittedIdentity{Attested: true, Work: work.Work{ISBN: bindingISBN}, Fields: map[string]bool{"isbn": true}}
	got := svc.applyISBNBinding(passed, anchor, &job.Row{}, &resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	if got.Identity.Result != pdf.IdentityPass || len(got.Identity.Evidence) != 1 || got.Identity.Evidence[0] != "generic" {
		t.Fatalf("identity = %+v, want the untouched generic pass", got.Identity)
	}
	// And a Reject stays a Reject.
	rejected := passed
	rejected.Identity = pdf.IdentityDecision{Result: pdf.IdentityReject, Evidence: []string{"generic"}}
	rejected.Text.Excerpt = bookText("978-92-4-011448-7")
	if got := svc.applyISBNBinding(rejected, anchor, &job.Row{}, &resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle}); got.Identity.Result != pdf.IdentityReject {
		t.Fatalf("identity = %+v, want the reject kept", got.Identity)
	}
}

// The PDF papio adopts is the sanitized rewrite, so the binding must be judged
// against the rewrite's own text. Without that, a book that carried an embedded
// file was parked even though the same book without one reaches ready.
func TestBindingIsAppliedToTheSanitizedRewrite(t *testing.T) {
	svc, jobs := exhaustionService(t)
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "open_books", cands: []resolver.Candidate{
		bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})}}, Policy: svc.Resolvers[0].Policy}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	text := bookText("978-92-4-011448-7")
	svc.Validate = func(_ context.Context, path, _ string, target work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 98, HasEmbeddedFiles: !strings.HasSuffix(path, ".sanitized")},
			Text:       pdf.TextReport{Chars: int64(len(text)), Excerpt: text},
			Identity:   pdf.MatchIdentityWithThreshold(text, target, 0.6),
		}, nil
	}
	svc.Sanitize = func(_ context.Context, src, dest string) (pdf.StructuralReport, error) {
		body, err := os.ReadFile(src)
		if err != nil {
			return pdf.StructuralReport{}, err
		}
		return pdf.StructuralReport{Valid: true}, os.WriteFile(dest, body, 0o600)
	}
	row := processOnce(t, svc, jobs, isbnRequest("wr_isbn_sanitized", nil))
	if row.State != job.StateReady {
		t.Fatalf("state = %q, want ready: the sanitized rewrite carries the same front matter", row.State)
	}
}

// A binding is a statement about ONE file. When the repository's own file fails
// and landing-page recovery swaps in a different citation_pdf_url, the derived
// candidate is another file and must not inherit it: a preview or an excerpt
// that happens to print the title and the ISBN would otherwise be upgraded.
func TestLandingDerivedCandidateDoesNotInheritTheISBNBinding(t *testing.T) {
	const landing = "https://repo.example.org/handle/10665/382452"
	const preview = "https://publisher.example/preview/atlas.pdf"
	svc, jobs := exhaustionService(t)
	candidate := bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	candidate.Landing = landing
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "open_books", cands: []resolver.Candidate{candidate}}, Policy: svc.Resolvers[0].Policy}}
	svc.LandingReader = &fakeLandingReader{pdfURLFor: map[string]string{landing: preview}}
	svc.Fetch = landingExpansionFetch(preview)
	var target work.Work
	svc.Validate = bookValidator(bookText("978-92-4-011448-7"), &target)

	row := processOnce(t, svc, jobs, isbnRequest("wr_isbn_landing_derived", nil))
	if row.State == job.StateReady {
		t.Fatal("a file reached through landing-page recovery was accepted on the repository's binding")
	}
}

// The binding names its file: a candidate whose URL is not the bound one has no
// binding, whatever copied it.
func TestBoundISBNOnlyDescribesTheBoundURL(t *testing.T) {
	c := bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	if c.BoundISBN() == nil {
		t.Fatal("the candidate's own binding must be returned")
	}
	c.URL = "https://elsewhere.example/other.pdf"
	if c.BoundISBN() != nil {
		t.Fatal("a candidate at another URL kept the binding")
	}
	if err := resolver.ValidateCandidate(bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})); err != nil {
		t.Fatalf("a well-formed bound candidate must validate: %v", err)
	}
	mismatched := bookCandidate(&resolver.ISBNBinding{ISBN: bindingISBN, Title: bindingTitle})
	mismatched.ISBNBinding.URL = "https://elsewhere.example/other.pdf"
	if resolver.ValidateCandidate(mismatched) == nil {
		t.Fatal("a binding that names another URL passed the source boundary")
	}
}
