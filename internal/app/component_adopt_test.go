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

func TestAdoptComponentRejectsUnsupportedRolesAndUnsafePDFs(t *testing.T) {
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

	for _, role := range []string{job.ComponentHTMLFullText, "main", "figures"} {
		err := svc.AdoptComponent(ctx, id, path, role)
		if !errors.Is(err, ErrComponentRole) {
			t.Fatalf("%s = %v, want ErrComponentRole", role, err)
		}
		assertComponentCount(t, jobs, id, 1)
		assertNoComponentTemp(t, svc, id)
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
	assertComponentRefusal(t, svc, jobs, id, err, ErrComponentRejected)
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
	err := svc.AdoptComponent(ctx, id, path, job.ComponentSupplement)
	if !errors.Is(err, ErrComponentPrecondition) {
		t.Fatalf("adopt component without main artifact = %v, want ErrComponentPrecondition", err)
	}
	assertComponentCount(t, jobs, id, 0)
	assertNoComponentTemp(t, svc, id)
}

func readyComponentService(t *testing.T, reqID string) (*Service, *job.Store, string) {
	t.Helper()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	svc.Fetch = fakeDownload(new(int))
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: readyCandidate()}, Policy: config.Source{Enabled: true}}}
	return svc, jobs, readyJobWithArtifact(t, svc, jobs, reqID)
}

func componentAdoptionDir(t *testing.T, svc *Service, id string) string {
	t.Helper()
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertComponentCount(t *testing.T, jobs *job.Store, id string, want int) {
	t.Helper()
	components, err := jobs.Components(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != want {
		t.Fatalf("components = %+v, want %d", components, want)
	}
}

func assertNoComponentTemp(t *testing.T, svc *Service, id string) {
	t.Helper()
	qdir, err := svc.Artifacts.QuarantineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(qdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("rejected component left quarantine temp %q", filepath.Join(qdir, entry.Name()))
		}
	}
}

func assertComponentRefusal(t *testing.T, svc *Service, jobs *job.Store, id string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("adopt component = %v, want %v", err, want)
	}
	assertComponentCount(t, jobs, id, 1)
	assertNoComponentTemp(t, svc, id)
}

func TestAdoptComponentClassifiesAdoptionRootAndCallerPathFailures(t *testing.T) {
	t.Run("missing adoption root", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_missing_root")
		path := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id, "supplement.pdf")

		err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPrecondition)
	})

	t.Run("missing caller directory", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_missing_dir")
		dir := componentAdoptionDir(t, svc, id)
		path := filepath.Join(dir, "missing", "supplement.pdf")

		err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPath)
	})
}

func TestAdoptComponentRejectsEscapesAndNonRegularFinalPaths(t *testing.T) {
	t.Run("dot dot traversal", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_dotdot")
		dir := componentAdoptionDir(t, svc, id)
		outside := filepath.Join(filepath.Dir(filepath.Dir(dir)), "outside.pdf")
		if err := os.WriteFile(outside, pdfBytes("outside"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := svc.AdoptComponent(context.Background(), id, dir+"/../../outside.pdf", job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPath)
	})

	t.Run("absolute path", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_absolute")
		componentAdoptionDir(t, svc, id)
		outside := filepath.Join(t.TempDir(), "outside.pdf")
		if err := os.WriteFile(outside, pdfBytes("outside"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := svc.AdoptComponent(context.Background(), id, outside, job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPath)
	})

	t.Run("directory", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_directory")
		dir := componentAdoptionDir(t, svc, id)
		path := filepath.Join(dir, "not-a-file.pdf")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}

		err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPath)
	})

	t.Run("final symlink", func(t *testing.T) {
		svc, jobs, id := readyComponentService(t, "wr_component_final_symlink")
		dir := componentAdoptionDir(t, svc, id)
		target := filepath.Join(dir, "target.pdf")
		if err := os.WriteFile(target, pdfBytes("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "linked.pdf")
		if err := os.Symlink("target.pdf", path); err != nil {
			t.Fatal(err)
		}

		err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
		assertComponentRefusal(t, svc, jobs, id, err, ErrComponentPath)
	})
}

func TestAdoptComponentAcceptsFileThroughAncestorSymlink(t *testing.T) {
	svc, jobs, id := readyComponentService(t, "wr_component_ancestor_symlink")
	dir := componentAdoptionDir(t, svc, id)
	physical := filepath.Join(dir, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(physical, "supplement.pdf"), pdfBytes("through parent symlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("physical", filepath.Join(dir, "linked-parent")); err != nil {
		t.Fatal(err)
	}

	if err := svc.AdoptComponent(context.Background(), id, filepath.Join(dir, "linked-parent", "supplement.pdf"), job.ComponentAppendix); err != nil {
		t.Fatalf("adopt through ancestor symlink: %v", err)
	}
	components, err := jobs.Components(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[1].Role != job.ComponentAppendix {
		t.Fatalf("components = %+v, want main plus appendix", components)
	}
}

func TestAdoptComponentRejectsInvalidAndUnsafePDFs(t *testing.T) {
	tests := []struct {
		name   string
		bytes  []byte
		report pdf.ValidationReport
	}{
		{
			name:  "invalid payload",
			bytes: []byte("not a PDF"),
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: false},
				Structural: pdf.StructuralReport{Valid: false},
			},
		},
		{
			name:  "encrypted",
			bytes: pdfBytes("encrypted"),
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, Encrypted: true},
			},
		},
		{
			name:  "embedded files",
			bytes: pdfBytes("embedded"),
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, HasEmbeddedFiles: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, jobs, id := readyComponentService(t, "wr_component_"+strings.ReplaceAll(tt.name, " ", "_"))
			svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				return tt.report, nil
			}
			dir := componentAdoptionDir(t, svc, id)
			path := filepath.Join(dir, "supplement.pdf")
			if err := os.WriteFile(path, tt.bytes, 0o600); err != nil {
				t.Fatal(err)
			}

			err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
			assertComponentRefusal(t, svc, jobs, id, err, ErrComponentRejected)
		})
	}
}

func TestAdoptComponentLeavesOperationalRootFailureUnclassified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	svc, jobs, id := readyComponentService(t, "wr_component_unreadable_root")
	root := svc.Config.EffectiveAdoptionRoot()
	dir := componentAdoptionDir(t, svc, id)
	path := filepath.Join(dir, "supplement.pdf")
	if err := os.WriteFile(path, pdfBytes("unreadable root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(root, 0o700); err != nil {
			t.Errorf("restore adoption root mode: %v", err)
		}
	})

	err := svc.AdoptComponent(context.Background(), id, path, job.ComponentSupplement)
	if err == nil {
		t.Fatal("adopt component unexpectedly succeeded with unreadable adoption root")
	}
	for _, sentinel := range []error{ErrComponentRole, ErrComponentPrecondition, ErrComponentPath, ErrComponentRejected} {
		if errors.Is(err, sentinel) {
			t.Fatalf("unreadable adoption root = %v, must not match %v", err, sentinel)
		}
	}
	assertComponentCount(t, jobs, id, 1)
	assertNoComponentTemp(t, svc, id)
}
