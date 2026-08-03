// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/work"
)

func TestValidationVerdictMirrorsCandidateBranches(t *testing.T) {
	// This table deliberately follows validateCandidate's branch order. The
	// recorded verdict must not drift from the transition the job actually takes.
	tests := []struct {
		name           string
		report         pdf.ValidationReport
		activeContent  bool
		identityReview bool
		want           string
	}{
		{
			name: "payload rejected",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: false, Reason: "too small"},
				Structural: pdf.StructuralReport{Valid: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			want: validationPayloadRejected,
		},
		{
			name: "structural invalid",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: false, Reason: "malformed xref"},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			want: validationStructRejected,
		},
		{
			name: "encrypted",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, Encrypted: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			want: validationUnsafe,
		},
		{
			name: "javascript active content",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, HasJavaScript: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			activeContent: true,
			want:          validationUnsafe,
		},
		{
			name: "embedded-file active content",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true, HasEmbeddedFiles: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			activeContent: true,
			want:          validationUnsafe,
		},
		{
			name: "identity review",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityReview},
			},
			identityReview: true,
			want:           validationIdentityReview,
		},
		{
			name: "text needs review",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true},
				Text:       pdf.TextReport{NeedsReview: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			identityReview: true,
			want:           validationIdentityReview,
		},
		{
			name: "identity rejected",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityReject},
			},
			want: validationIdentityReject,
		},
		{
			name: "pass",
			report: pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true},
				Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
			},
			want: validationPass,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activeContent := tc.activeContent || tc.report.Structural.HasJavaScript || tc.report.Structural.HasEmbeddedFiles
			needsIdentityReview := tc.identityReview || tc.report.Text.NeedsReview || tc.report.Identity.Result == pdf.IdentityReview
			if got := validationVerdict(tc.report, activeContent, needsIdentityReview); got != tc.want {
				t.Fatalf("validationVerdict() = %q, want %q", got, tc.want)
			}
		})
	}
}

func validationEvidenceReport(identityResult string) pdf.ValidationReport {
	return pdf.ValidationReport{
		Payload: pdf.PayloadReport{
			OK:          true,
			SizeBytes:   2400,
			HasHeader:   true,
			HasEOF:      true,
			SniffedMIME: "application/pdf",
			Reason:      "payload evidence",
		},
		Structural: pdf.StructuralReport{
			Valid:            true,
			Pages:            3,
			Encrypted:        false,
			HasJavaScript:    false,
			HasEmbeddedFiles: false,
			Reason:           "structural evidence",
		},
		Text: pdf.TextReport{
			Chars:       2048,
			Excerpt:     "IDENTITY_EXCERPT_STAYS_OUT_OF_DOCUMENT",
			OCRUsed:     true,
			NeedsReview: false,
			Evidence:    []string{"text extraction evidence"},
		},
		Identity: pdf.IdentityDecision{
			Result:   identityResult,
			Evidence: []string{"identity evidence"},
		},
		Evidence: []string{"top-level validation evidence"},
	}
}

func validationPipelineService(t *testing.T, report pdf.ValidationReport, validateErr error) *Service {
	t.Helper()
	svc, _ := newTestService(t)
	svc.Resolvers = []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
			Source: "fixture", URL: "https://example.test/validation-evidence.pdf",
			Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
			ReuseLicense: "unknown", ExpectedMIME: "application/pdf", Direct: true,
			IdentityConfidence: 1,
		}}},
		Policy: config.Source{Enabled: true},
	}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return report, validateErr
	}
	return svc
}

func TestValidateCandidateRecordsPassingEvidence(t *testing.T) {
	ctx := context.Background()
	report := validationEvidenceReport(pdf.IdentityPass)
	svc := validationPipelineService(t, report, nil)
	got := processOnce(t, svc, svc.Jobs, doiRequest("wr_validation_record_pass"))
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want ready", got.State)
	}

	records, err := svc.Jobs.ValidationReports(ctx, got.ID)
	if err != nil {
		t.Fatalf("ValidationReports: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("validation reports = %+v, want one record", records)
	}
	record := records[0]
	if record.JobID != got.ID || got.SelectedCandidateID == 0 || record.CandidateID != got.SelectedCandidateID {
		t.Fatalf("record provenance = %+v, job = %+v; want accepted candidate %d", record, got, got.SelectedCandidateID)
	}
	if record.Outcome != validationPass {
		t.Fatalf("record outcome = %q, want %q", record.Outcome, validationPass)
	}
	if record.SHA256 != got.ArtifactSHA256 || record.SHA256 == "" {
		t.Fatalf("record sha256 = %q, artifact sha256 = %q", record.SHA256, got.ArtifactSHA256)
	}

	var document pdf.ReportDocument
	if err := json.Unmarshal([]byte(record.Document), &document); err != nil {
		t.Fatalf("decode validation document: %v", err)
	}
	if document.SchemaVersion != pdf.ReportSchemaVersion {
		t.Fatalf("document schema version = %q, want %q", document.SchemaVersion, pdf.ReportSchemaVersion)
	}
	if document.Payload.Reason != report.Payload.Reason || document.Payload.SniffedMIME != report.Payload.SniffedMIME ||
		document.Structural.Reason != report.Structural.Reason || document.Structural.Pages != report.Structural.Pages ||
		document.Text.Chars != report.Text.Chars || !document.Text.OCRUsed || len(document.Text.Evidence) != 1 ||
		document.Text.Evidence[0] != report.Text.Evidence[0] || document.Identity.Result != report.Identity.Result ||
		len(document.Identity.Evidence) != 1 || document.Identity.Evidence[0] != report.Identity.Evidence[0] ||
		len(document.Evidence) != 1 || document.Evidence[0] != report.Evidence[0] {
		t.Fatalf("validation evidence document = %+v, want evidence from stubbed report", document)
	}
}

func TestRejectedCandidateRecordsValidationEvidence(t *testing.T) {
	ctx := context.Background()
	report := validationEvidenceReport(pdf.IdentityReject)
	svc := validationPipelineService(t, report, nil)
	got := processOnce(t, svc, svc.Jobs, doiRequest("wr_validation_record_reject"))
	if got.State == job.StateReady {
		t.Fatalf("rejected candidate made job ready: %+v", got)
	}

	records, err := svc.Jobs.ValidationReports(ctx, got.ID)
	if err != nil {
		t.Fatalf("ValidationReports: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("validation reports = %+v, want one rejected record", records)
	}
	record := records[0]
	if record.CandidateID == 0 {
		t.Fatalf("rejected validation record has no candidate provenance: %+v", record)
	}
	if record.Outcome != validationIdentityReject {
		t.Fatalf("rejected record outcome = %q, want %q", record.Outcome, validationIdentityReject)
	}
	var document pdf.ReportDocument
	if err := json.Unmarshal([]byte(record.Document), &document); err != nil {
		t.Fatalf("decode rejected validation document: %v", err)
	}
	if document.Identity.Result != pdf.IdentityReject || len(document.Identity.Evidence) != 1 ||
		document.Identity.Evidence[0] != "identity evidence" {
		t.Fatalf("rejected document identity = %+v", document.Identity)
	}
}

func TestValidationErrorRecordsPartialEvidenceAndParksJob(t *testing.T) {
	ctx := context.Background()
	report := validationEvidenceReport(pdf.IdentityPass)
	report.Payload.Reason = "partial report before validator failure"
	svc := validationPipelineService(t, report, errors.New("validator deadline"))
	got := processOnce(t, svc, svc.Jobs, doiRequest("wr_validation_record_error"))
	if got.State != job.StateNeedsReview {
		t.Fatalf("job state = %s, want needs_review", got.State)
	}

	records, err := svc.Jobs.ValidationReports(ctx, got.ID)
	if err != nil {
		t.Fatalf("ValidationReports: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("validation reports = %+v, want one partial record", records)
	}
	record := records[0]
	if record.Outcome != validationIncomplete {
		t.Fatalf("validation error outcome = %q, want %q", record.Outcome, validationIncomplete)
	}
	var document pdf.ReportDocument
	if err := json.Unmarshal([]byte(record.Document), &document); err != nil {
		t.Fatalf("decode partial validation document: %v", err)
	}
	if document.Payload.Reason != report.Payload.Reason || len(document.Evidence) != 1 ||
		document.Evidence[0] != report.Evidence[0] {
		t.Fatalf("partial validation document = %+v, want stubbed partial evidence", document)
	}
}

func TestValidationEvidenceWriteFailureDoesNotFailSuccessfulAcquisition(t *testing.T) {
	ctx := context.Background()
	svc := validationPipelineService(t, validationEvidenceReport(pdf.IdentityPass), nil)
	_, err := svc.Jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_validation_report_insert
		BEFORE INSERT ON validation_reports
		BEGIN
			SELECT RAISE(FAIL, 'validation evidence unavailable');
		END`)
	if err != nil {
		t.Fatalf("install validation report failure trigger: %v", err)
	}

	got := processOnce(t, svc, svc.Jobs, doiRequest("wr_validation_record_write_failure"))
	if got.State != job.StateReady {
		t.Fatalf("job state = %s, want ready despite evidence write failure", got.State)
	}
	records, err := svc.Jobs.ValidationReports(ctx, got.ID)
	if err != nil {
		t.Fatalf("ValidationReports: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("validation reports = %+v, want no record after forced write failure", records)
	}
}
