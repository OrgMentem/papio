// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/delivery"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/work"
)

// stageSupplied writes body into the job's supply directory, as the CLI does,
// and returns the staged file name.
func stageSupplied(t *testing.T, svc *Service, jobID, name string, body []byte) string {
	t.Helper()
	dir, err := SuppliedPDFStagingDir(svc.Config.DataDir, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func excerptValidation(excerpt string) ValidateFunc {
	return func(_ context.Context, _, _ string, target work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 8},
			Text:       pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt},
			Identity:   pdf.MatchIdentity(excerpt, target),
		}, nil
	}
}

func eventDetails(t *testing.T, jobs *job.Store, jobID, kind string) []map[string]any {
	t.Helper()
	rows, err := jobs.S.DB().QueryContext(context.Background(),
		`SELECT detail_json FROM events WHERE job_id = ? AND kind = ? ORDER BY seq`, jobID, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		detail := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

func assertStagedFileGone(t *testing.T, svc *Service, jobID, name string) {
	t.Helper()
	dir, err := SuppliedPDFStagingDir(svc.Config.DataDir, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file after supply: %v; want it removed", err)
	}
}

// The matching PDF takes the browser adoption path end to end: validated,
// promoted, ready, and recorded as operator-supplied rather than as a
// browser download or a daemon fetch.
func TestSupplyPDFPromotesAMatchingPDFWithOperatorProvenance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, jobs *job.Store) string
	}{
		{"parked", func(t *testing.T, jobs *job.Store) string { return parkAwaitingHuman(t, jobs, "wr_supply_parked") }},
		// A live job is parked through the same legal edges browser adoption uses.
		{"queued", func(t *testing.T, jobs *job.Store) string { return createLiveJob(t, jobs, "wr_supply_queued") }},
		// A paywalled job usually waits here between attempts; supply follows
		// retry_wait -> resolving first.
		{"retry_wait", func(t *testing.T, jobs *job.Store) string {
			return createLiveJob(t, jobs, "wr_supply_retry_wait",
				[2]string{job.StateQueued, job.StateResolving}, [2]string{job.StateResolving, job.StateRetryWait})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			svc.Validate = passValidation()
			id := tc.setup(t, jobs)
			name := stageSupplied(t, svc, id, "supplied_01.pdf", pdfBytes("operator supplied"))

			got, err := svc.SupplyPDF(ctx, id, name)
			if err != nil {
				t.Fatalf("supply: %v", err)
			}
			if got.Outcome != AdoptionAccepted {
				t.Fatalf("outcome = %q, want %q", got.Outcome, AdoptionAccepted)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateReady || row.ArtifactSHA256 != got.SHA256 {
				t.Fatalf("job = %s artifact %s; want ready with %s", row.State, row.ArtifactSHA256, got.SHA256)
			}
			if err := svc.Artifacts.Verify(got.SHA256); err != nil {
				t.Fatalf("artifact verify: %v", err)
			}
			candidate, err := jobs.GetCandidate(ctx, got.CandidateID)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Source != job.CandidateSourceOperator || candidate.URLKey != "operator-supply:sha256:"+got.SHA256 ||
				candidate.Version != resolver.VersionUnknown || candidate.AccessBasis != resolver.AccessManual {
				t.Fatalf("candidate = %+v; want operator provenance keyed by content", candidate)
			}
			producers := eventDetails(t, jobs, id, job.ArtifactProducerEvent)
			if len(producers) != 1 || producers[0]["producer"] != string(job.ProducerManual) || producers[0]["basis"] != "operator_supplied" {
				t.Fatalf("producer records = %+v; want one manual/operator_supplied", producers)
			}
			adopted := false
			for _, detail := range eventDetails(t, jobs, id, "job.transition") {
				if detail["source"] == "browser" {
					t.Fatalf("transition %+v attributes supplied bytes to the browser", detail)
				}
				if detail["reason"] == "adopt_supplied_pdf" && detail["source"] == job.CandidateSourceOperator {
					adopted = true
				}
			}
			if !adopted {
				t.Fatal("missing the operator adoption transition")
			}
			assertStagedFileGone(t, svc, id, name)
		})
	}
}

// A PDF of a different work never becomes ready because the operator named
// the job: a contradicted DOI goes to identity review, and a hard identity
// reject re-parks the job for a different file.
func TestSupplyPDFOfADifferentWorkIsNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name        string
		excerpt     string
		title       string
		wantOutcome string
		wantState   string
		wantAction  string
	}{
		{
			name: "contradicted DOI", excerpt: "DOI: 10.9999/other-paper\nSome unrelated title and content\n",
			wantOutcome: AdoptionNeedsReview, wantState: job.StateNeedsReview, wantAction: "verify_identity",
		},
		{
			name: "different title", excerpt: "Methods for Cultivating Greenhouse Tomatoes\nby Robert Smith\n2021\n",
			title:       "SUVA: A Probabilistic Framework for Auditing LLMs with an Application to Social Preferences",
			wantOutcome: AdoptionRejected, wantState: job.StateAwaitingHuman, wantAction: "manual_download",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			svc.Validate = excerptValidation(tc.excerpt)
			id := parkAwaitingHuman(t, jobs, "wr_supply_wrong")
			if tc.title != "" {
				row, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := jobs.EnrichWorkRequestMetadata(ctx, row.WorkRequestID, tc.title, []string{"Yan Leng", "Yuan Yuan"}, 2026); err != nil {
					t.Fatal(err)
				}
			}
			name := stageSupplied(t, svc, id, "supplied_wrong.pdf", pdfBytes("wrong work"))

			got, err := svc.SupplyPDF(ctx, id, name)
			if err != nil {
				t.Fatalf("supply: %v", err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State == job.StateReady || row.ArtifactSHA256 != "" {
				t.Fatalf("wrong work became ready: state=%s artifact=%s", row.State, row.ArtifactSHA256)
			}
			if got.Outcome != tc.wantOutcome || row.State != tc.wantState {
				t.Fatalf("outcome %q state %s; want %q %s", got.Outcome, row.State, tc.wantOutcome, tc.wantState)
			}
			if !openActionKinds(t, jobs, id)[tc.wantAction] {
				t.Fatalf("open actions = %v; want %s", openActionKinds(t, jobs, id), tc.wantAction)
			}
			assertStagedFileGone(t, svc, id, name)
		})
	}
}

// The RPC names a file inside the job's staging directory and nothing else.
// Every other shape is refused before the job moves.
func TestSupplyPDFRefusesNamesOutsideTheStagingDirectory(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_supply_escape")
	outside := filepath.Join(svc.Config.DataDir, "outside.pdf")
	if err := os.WriteFile(outside, pdfBytes("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageSupplied(t, svc, id, "real.pdf", pdfBytes("staged"))
	dir, err := SuppliedPDFStagingDir(svc.Config.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"", ".", "..", "../outside.pdf", "../../outside.pdf", outside, "nested/../real.pdf",
		`..\outside.pdf`, "link.pdf", "nested", "missing.pdf",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFName) {
				t.Fatalf("supply %q = %v; want ErrSuppliedPDFName", name, err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil || row.State != job.StateAwaitingHuman {
				t.Fatalf("job after refused supply = %+v, %v; want untouched awaiting_human", row, err)
			}
		})
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("file outside the staging directory was touched: %v", err)
	}
}

// A planted symlink in place of the job's staging directory could point the
// daemon at any directory on disk; it is refused rather than followed.
func TestSupplyPDFRefusesASymlinkedStagingDirectory(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_supply_linked_dir")
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "paper.pdf"), pdfBytes("elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := SuppliedPDFStagingDir(svc.Config.DataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SupplyPDF(ctx, id, "paper.pdf"); !errors.Is(err, ErrSuppliedPDFName) {
		t.Fatalf("supply through a symlinked directory = %v; want ErrSuppliedPDFName", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "paper.pdf")); err != nil {
		t.Fatalf("file behind the symlink was touched: %v", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after refused supply = %+v, %v; want untouched awaiting_human", row, err)
	}
}

// A finished job never takes new bytes. The refusal is typed, so the RPC seam
// can give it an error class the CLI turns into the acquire-again remedy.
func TestSupplyPDFRefusesAFinishedJob(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := createLiveJob(t, jobs, "wr_supply_cancelled", [2]string{job.StateQueued, job.StateResolving})
	if err := jobs.Cancel(ctx, id, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("refused"))
	_, err := svc.SupplyPDF(ctx, id, name)
	var finished *SuppliedPDFJobFinishedError
	if !errors.As(err, &finished) || finished.State != job.StateCancelled {
		t.Fatalf("supply to a cancelled job = %v; want SuppliedPDFJobFinishedError naming cancelled", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateCancelled || row.ArtifactSHA256 != "" {
		t.Fatalf("job after refused supply = %+v, %v; want untouched cancelled", row, err)
	}
}

func TestSupplyPDFRefusesAJobHeldForReview(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := createLiveJob(t, jobs, "wr_supply_review",
		[2]string{job.StateQueued, job.StateResolving}, [2]string{job.StateResolving, job.StateFetching},
		[2]string{job.StateFetching, job.StateNeedsReview})
	name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("refused"))
	if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFState) {
		t.Fatalf("supply to a job held for review = %v; want ErrSuppliedPDFState", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateNeedsReview || row.ArtifactSHA256 != "" {
		t.Fatalf("job after refused supply = %+v, %v; want untouched needs_review", row, err)
	}
}

// A job in retry_wait on a live document-delivery request would leave that
// provider request behind if a supplied PDF finished it, so it is refused
// until the operator settles the request.
func TestSupplyPDFRefusesARetryWaitJobWithALiveDeliveryRequest(t *testing.T) {
	ctx := context.Background()
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	id := createLiveJob(t, jobs, "wr_supply_delivery",
		[2]string{job.StateQueued, job.StateResolving}, [2]string{job.StateResolving, job.StateRetryWait})
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	request, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deliverySvc.UpdateState(ctx, request.ID, delivery.StateSubmitted); err != nil {
		t.Fatal(err)
	}
	name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("delivery pending"))
	if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFState) {
		t.Fatalf("supply during a live delivery request = %v; want ErrSuppliedPDFState", err)
	}
	after, err := jobs.Get(ctx, id)
	if err != nil || after.State != job.StateRetryWait {
		t.Fatalf("job after refused supply = %+v, %v; want untouched retry_wait", after, err)
	}
}

// The retry_wait admission belongs to the operator's explicit request only:
// a browser download that names a job waiting to retry is refused as before.
func TestAdoptDownloadStillRefusesARetryWaitJob(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := createLiveJob(t, jobs, "wr_adopt_retry_wait",
		[2]string{job.StateQueued, job.StateResolving}, [2]string{job.StateResolving, job.StateRetryWait})
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, pdfBytes("browser download"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stateErr *adoptionStateError
	if err := svc.AdoptDownload(ctx, id, path); !errors.As(err, &stateErr) || stateErr.state != job.StateRetryWait {
		t.Fatalf("browser adoption of a retry_wait job = %v; want the adoption state refusal", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateRetryWait {
		t.Fatalf("job after refused adoption = %+v, %v; want untouched retry_wait", row, err)
	}
}

func TestSupplyPDFRefusesAStagedFileOverTheFetchLimit(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_supply_oversized")
	body := pdfBytes("oversized")
	svc.Config.Fetch.MaxBytes = int64(len(body)) - 1
	name := stageSupplied(t, svc, id, "big.pdf", body)
	if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFSize) {
		t.Fatalf("oversized supply = %v; want ErrSuppliedPDFSize", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after oversized supply = %+v, %v; want awaiting_human", row, err)
	}
	assertStagedFileGone(t, svc, id, name)
}
