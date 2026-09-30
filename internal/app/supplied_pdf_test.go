// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"papio/internal/delivery"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/store"
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

// pinDeliveryRequest pins jobID to a new document-delivery request in state.
func pinDeliveryRequest(t *testing.T, jobs *job.Store, deliverySvc *delivery.Service, jobID string, state delivery.State) *delivery.Request {
	t.Helper()
	ctx := context.Background()
	row, err := jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: jobID, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != delivery.StateOffered {
		if err := deliverySvc.UpdateState(ctx, request.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	return request
}

func openActionIDs(t *testing.T, jobs *job.Store, jobID, kind string) []int64 {
	t.Helper()
	actions, err := jobs.ListHumanActionsForJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, action := range actions {
		if action.Action.Status == "open" && action.Action.Kind == kind {
			ids = append(ids, action.Action.ID)
		}
	}
	return ids
}

// failCandidateInsert makes the next candidate insert fail, which is the
// first write a supply makes after it parks the job.
func failCandidateInsert(t *testing.T, jobs *job.Store) {
	t.Helper()
	if _, err := jobs.S.DB().Exec(`CREATE TRIGGER supply_fail_candidate BEFORE INSERT ON candidates
		BEGIN SELECT RAISE(ABORT, 'injected candidate failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = jobs.S.DB().Exec(`DROP TRIGGER IF EXISTS supply_fail_candidate`) })
}

// A job pinned to a live document-delivery request would leave that provider
// request behind if a supplied PDF finished it. It is refused in every state
// the command otherwise accepts, before the job moves or the staged file is
// read.
func TestSupplyPDFRefusesALiveDeliveryRequestInEveryState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state delivery.State
		steps [][2]string
		want  string
	}{
		{"queued", delivery.StateSubmitted, nil, job.StateQueued},
		{"retry_wait", delivery.StateSubmitted, [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateRetryWait}}, job.StateRetryWait},
		{"resolving", delivery.StatePending, [][2]string{{job.StateQueued, job.StateResolving}}, job.StateResolving},
		{"fetching", delivery.StateSubmitted, [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}}, job.StateFetching},
		{"awaiting_human", delivery.StatePending, [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateAwaitingHuman}}, job.StateAwaitingHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs, deliverySvc := newDeliveryTestService(t)
			svc.Delivery = deliverySvc
			id := createLiveJob(t, jobs, "wr_supply_delivery_"+tc.name, tc.steps...)
			if tc.want == job.StateAwaitingHuman {
				// A parked job carries the action that makes it adoptable, so
				// only the delivery refusal stands between it and ready.
				if _, err := jobs.OpenHumanAction(ctx, id, job.CandidateEligibleKind, "please download the paper", job.Access(false, "")); err != nil {
					t.Fatal(err)
				}
			}
			before := openActionIDs(t, jobs, id, job.CandidateEligibleKind)
			pinDeliveryRequest(t, jobs, deliverySvc, id, tc.state)
			name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("delivery pending"))
			if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFState) {
				t.Fatalf("supply during a live delivery request = %v; want ErrSuppliedPDFState", err)
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || after.State != tc.want || after.ArtifactSHA256 != "" {
				t.Fatalf("job after refused supply = %+v, %v; want untouched %s", after, err, tc.want)
			}
			if open := openActionIDs(t, jobs, id, job.CandidateEligibleKind); !slices.Equal(open, before) {
				t.Fatalf("open manual_download actions = %v; want %v, untouched", open, before)
			}
			dir, err := SuppliedPDFStagingDir(svc.Config.DataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("staged file after refusal: %v; want it kept for a later supply", err)
			}
		})
	}
}

// A request that goes live after the up-front check, as a delivery poll or a
// submit for a job sharing the request would make it, is still refused: the
// check runs again inside the awaiting_human -> validating transaction. A
// job the supply took from retry_wait goes back there.
func TestSupplyPDFRechecksTheDeliveryRequestWhenItEntersValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, jobs *job.Store) string
		want  string
	}{
		{"awaiting_human", func(t *testing.T, jobs *job.Store) string {
			return parkAwaitingHuman(t, jobs, "wr_supply_race_parked")
		}, job.StateAwaitingHuman},
		{"retry_wait", func(t *testing.T, jobs *job.Store) string {
			return createLiveJob(t, jobs, "wr_supply_race_retry",
				[2]string{job.StateQueued, job.StateResolving}, [2]string{job.StateResolving, job.StateRetryWait})
		}, job.StateRetryWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs, deliverySvc := newDeliveryTestService(t)
			svc.Delivery = deliverySvc
			id := tc.setup(t, jobs)
			request := pinDeliveryRequest(t, jobs, deliverySvc, id, delivery.StateOffered)
			// The request goes live between the park and the transition.
			if _, err := jobs.S.DB().Exec(fmt.Sprintf(`CREATE TRIGGER supply_race_delivery AFTER INSERT ON candidates
				BEGIN UPDATE delivery_requests SET state = 'submitted' WHERE id = %d; END`, request.ID)); err != nil {
				t.Fatal(err)
			}
			name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("delivery raced"))
			if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFState) {
				t.Fatalf("supply racing a delivery submit = %v; want ErrSuppliedPDFState", err)
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || after.State != tc.want || after.ArtifactSHA256 != "" {
				t.Fatalf("job after raced supply = %+v, %v; want %s without an artifact", after, err, tc.want)
			}
		})
	}
}

// A retry claim moves a job to resolving under the worker's lease and may
// submit a delivery request in a later commit, which no check at park time
// can see. A supply therefore refuses a job a worker holds.
func TestSupplyPDFRefusesAJobAWorkerHolds(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := createLiveJob(t, jobs, "wr_supply_leased", [2]string{job.StateQueued, job.StateResolving})
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET lease_owner = 'worker', lease_expires_at = ? WHERE id = ?`,
		store.FormatTime(time.Now().Add(time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("leased"))
	if _, err := svc.SupplyPDF(ctx, id, name); !errors.Is(err, ErrSuppliedPDFState) {
		t.Fatalf("supply to a leased resolving job = %v; want ErrSuppliedPDFState", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateResolving || row.LeaseOwner != "worker" {
		t.Fatalf("job after refused supply = %+v, %v; want resolving and still held by the worker", row, err)
	}
}

// A supply that fails after it parked a retry_wait job, and before validation
// took the job, puts the job back in retry_wait with its schedule. The action
// the park opened closes; an action the job already had stays open.
func TestSupplyPDFPutsARetryWaitJobBackWhenTheSupplyFailsAfterParking(t *testing.T) {
	for _, priorAction := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_action_%t", priorAction), func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			svc.Validate = passValidation()
			id := createLiveJob(t, jobs, "wr_supply_unpark_retry", [2]string{job.StateQueued, job.StateResolving})
			retryAt := time.Now().Add(3 * time.Hour)
			if err := jobs.Transition(ctx, id, job.StateResolving, job.StateRetryWait, nil, job.WithRetryAt(retryAt)); err != nil {
				t.Fatal(err)
			}
			var prior []int64
			if priorAction {
				if _, err := jobs.OpenHumanAction(ctx, id, job.CandidateEligibleKind, "please download the paper", job.Access(false, "")); err != nil {
					t.Fatal(err)
				}
				prior = openActionIDs(t, jobs, id, job.CandidateEligibleKind)
			}
			failCandidateInsert(t, jobs)
			name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("unpark"))
			_, err := svc.SupplyPDF(ctx, id, name)
			if err == nil || !strings.Contains(err.Error(), "injected candidate failure") {
				t.Fatalf("supply = %v; want the candidate failure", err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil || row.State != job.StateRetryWait || row.RetryAt != store.FormatTime(retryAt) {
				t.Fatalf("job after failed supply = %+v, %v; want retry_wait at %s", row, err, store.FormatTime(retryAt))
			}
			if open := openActionIDs(t, jobs, id, job.CandidateEligibleKind); !slices.Equal(open, prior) {
				t.Fatalf("open manual_download actions = %v; want %v", open, prior)
			}
		})
	}
}

// queued has no legal edge back from awaiting_human, so a supply that fails
// after parking a queued job leaves it parked with its manual_download action
// and tells the operator so.
func TestSupplyPDFReportsAQueuedJobItCouldNotPutBack(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := createLiveJob(t, jobs, "wr_supply_unpark_queued")
	failCandidateInsert(t, jobs)
	name := stageSupplied(t, svc, id, "supplied.pdf", pdfBytes("unpark"))
	_, err := svc.SupplyPDF(ctx, id, name)
	if !errors.Is(err, ErrSuppliedPDFState) || !strings.Contains(err.Error(), "parked awaiting a PDF") {
		t.Fatalf("supply = %v; want ErrSuppliedPDFState saying the job is parked awaiting a PDF", err)
	}
	if len(err.Error()) > 500 || strings.Contains(err.Error(), svc.Config.DataDir) {
		t.Fatalf("operator error %q must fit the RPC message bound and carry no path", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after failed supply = %+v, %v; want awaiting_human", row, err)
	}
	if open := openActionIDs(t, jobs, id, job.CandidateEligibleKind); len(open) != 1 {
		t.Fatalf("open manual_download actions = %v; want the park's one", open)
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
