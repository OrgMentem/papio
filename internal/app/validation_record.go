// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"papio/internal/job"
	"papio/internal/pdf"
)

// Validation verdicts. A closed vocabulary so a consumer routes on the value
// instead of parsing prose, and deliberately about the validation only: whether
// the bytes were later promoted, imported, or superseded is the job's business,
// not this record's.
const (
	validationPass            = "pass"
	validationPayloadRejected = "payload_rejected"
	validationStructRejected  = "structure_rejected"
	validationUnsafe          = "unsafe_content"
	validationIdentityReview  = "identity_review"
	validationIdentityReject  = "identity_rejected"
	validationIncomplete      = "validation_error"
)

// validationVerdict names what the pipeline decided about one candidate's bytes.
// It mirrors validateCandidate's branch order exactly — the first matching arm
// wins there too — so the recorded verdict cannot disagree with the transition
// the job actually took.
func validationVerdict(report pdf.ValidationReport, activeContent, needsIdentityReview bool) string {
	switch {
	case report.Structural.Encrypted || activeContent:
		return validationUnsafe
	case !report.Payload.OK:
		return validationPayloadRejected
	case !report.Structural.Valid:
		return validationStructRejected
	case needsIdentityReview:
		return validationIdentityReview
	case report.Identity.Result != pdf.IdentityPass:
		return validationIdentityReject
	default:
		return validationPass
	}
}

// recordValidation persists one candidate's validation evidence.
//
// A failure here is dropped on purpose, the way FinishAttempt's is: the verdict
// itself already lives durably in the job's state, its attempt row, and its
// event stream. This record is additional evidence for a consumer that has to
// justify a rights or quality decision, and losing it must not fail an
// acquisition that otherwise succeeded.
func (s *Service) recordValidation(ctx context.Context, jobID string, candidateID int64, sha, outcome string, report pdf.ValidationReport) {
	if s.Jobs == nil {
		return
	}
	document, err := pdf.EncodeDocument(report)
	if err != nil {
		return
	}
	_ = s.Jobs.RecordValidationReport(ctx, job.ValidationRecord{
		JobID: jobID, CandidateID: candidateID, SHA256: sha,
		Outcome: outcome, Document: string(document),
	})
}

// consumerNameRE bounds the accounting label a caller may attach to its
// submissions. It is deliberately narrow but not arbitrary: it admits the
// namespaced form a real consumer uses (`inscribi:project:psyc101`) and refuses
// the two shapes that actually cause harm — whitespace, which would forge a field
// or record boundary in the tab-separated `jobs list` / `actions list` output, and
// control characters, which a terminal or a log would act on.
//
// Rejected rather than sanitized: this value partitions accounting totals, and a
// silently rewritten key attributes work to a name nobody asked for.
var consumerNameRE = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]{1,128}$`)

// validConsumer normalizes and checks the caller-supplied consumer label. An
// empty value stays empty — no attribution is a legitimate answer and the only
// honest one for a caller that named none.
func validConsumer(consumer string) (string, error) {
	consumer = strings.TrimSpace(consumer)
	if consumer == "" {
		return "", nil
	}
	if !consumerNameRE.MatchString(consumer) {
		return "", fmt.Errorf("consumer must be 1-128 characters of letters, digits, or . _ : @ / + -")
	}
	return consumer, nil
}
