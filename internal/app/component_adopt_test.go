// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/work"
)

// readyJobWithArtifact drives a job to ready through the ordinary pipeline, so
// its main component edge is written by the real transition rather than by hand.
func readyJobWithArtifact(t *testing.T, svc *Service, jobs *job.Store, reqID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest(reqID))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim = %+v, %v", row, err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	out, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", out.State)
	}
	return id
}

// readyCandidate is the single direct OA candidate the fake resolver offers, so
// the job reaches ready through the ordinary fetch/validate path.
func readyCandidate() []resolver.Candidate {
	return []resolver.Candidate{{
		Source: "fixture", URL: "https://example.test/paper.pdf",
		ResolvedWork: work.Work{DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"A"}, Year: 2024},
		Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "cc-by-4.0",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}
}

// A supplement is filed beside the main artifact, and the acquisition reports
// both components with the main one first.
func TestAdoptComponentRecordsASupplementBesideTheMainArtifact(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	svc.Fetch = fakeDownload(new(int))
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: readyCandidate()}, Policy: config.Source{Enabled: true}}}
	ctx := context.Background()
	id := readyJobWithArtifact(t, svc, jobs, "wr_component_main")

	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "supplement.pdf")
	if err := os.WriteFile(path, pdfBytes("supplementary tables"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptComponent(ctx, id, path, job.ComponentSupplement); err != nil {
		t.Fatalf("adopt supplement: %v", err)
	}

	components, err := jobs.Components(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 {
		t.Fatalf("components = %+v, want main plus supplement", components)
	}
	if components[0].Role != job.ComponentMain {
		t.Fatalf("first component role = %q, want main first", components[0].Role)
	}
	if components[1].Role != job.ComponentSupplement {
		t.Fatalf("second component role = %q, want supplement", components[1].Role)
	}
	if components[1].SHA256 == components[0].SHA256 {
		t.Fatal("supplement recorded the main artifact's digest")
	}
	// A component is evidence about an acquisition, never the acquisition: the
	// job's own main artifact must be untouched.
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.ArtifactSHA256 != components[0].SHA256 || row.State != job.StateReady {
		t.Fatalf("main artifact or state disturbed by a component: %+v", row)
	}
	// Identity is deliberately unasserted for a supplement, which is usually not
	// the article and would fail a title/DOI match.
	if components[1].IdentityResult != "" {
		t.Fatalf("supplement asserted identity %q; a supplement is not the work", components[1].IdentityResult)
	}
}

func TestAdoptComponentRejectsUnsupportedRolesAndUnreadablePDFs(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	svc.Fetch = fakeDownload(new(int))
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: readyCandidate()}, Policy: config.Source{Enabled: true}}}
	ctx := context.Background()
	id := readyJobWithArtifact(t, svc, jobs, "wr_component_reject")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "part.pdf")
	if err := os.WriteFile(path, pdfBytes("a part"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := svc.AdoptComponent(ctx, id, path, job.ComponentHTMLFullText); !errors.Is(err, ErrComponentRole) {
		t.Fatalf("html_fulltext = %v, want ErrComponentRole: raw provider HTML is active content", err)
	}
	if err := svc.AdoptComponent(ctx, id, path, "main"); !errors.Is(err, ErrComponentRole) {
		t.Fatalf("main = %v, want ErrComponentRole: the main component is the transition's job", err)
	}
	if err := svc.AdoptComponent(ctx, id, path, "figures"); !errors.Is(err, ErrComponentRole) {
		t.Fatalf("unknown role = %v, want ErrComponentRole", err)
	}

	// An active-content PDF is refused exactly as for a main file.
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2, HasJavaScript: true},
			Text:       pdf.TextReport{Chars: 100},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
		}, nil
	}
	err := svc.AdoptComponent(ctx, id, path, job.ComponentSupplement)
	if err == nil || !strings.Contains(err.Error(), "active content") {
		t.Fatalf("active-content supplement = %v, want rejection", err)
	}
	components, listErr := jobs.Components(ctx, id)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(components) != 1 {
		t.Fatalf("rejected component was recorded anyway: %+v", components)
	}
}

// A job with no main artifact cannot carry components: a supplement is evidence
// about an acquisition that happened.
func TestAdoptComponentRequiresAMainArtifact(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	ctx := context.Background()
	id := parkAwaitingHuman(t, jobs, "wr_component_no_main")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "supplement.pdf")
	if err := os.WriteFile(path, pdfBytes("orphan supplement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptComponent(ctx, id, path, job.ComponentSupplement); err == nil {
		t.Fatal("filed a component against a job holding no main artifact")
	}
}
