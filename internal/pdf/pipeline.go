// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"

	"papio/internal/work"
)

// ValidationInput names the file the fetch layer produced. Every stage reads
// Path: the payload gate reads a bounded prefix, and hostile structural
// parsing stays exclusively in the re-exec worker. Validating bytes already in
// memory is ValidatePayload's job, not this pipeline's — the payload gate is
// the only stage that can work without a file.
type ValidationInput struct {
	DeclaredMIME string
	Path         string
	WorkerBinary string
	Capability   Capability
	Target       work.Work
}

// ValidationOptions independently bounds structural and semantic stages.
type ValidationOptions struct {
	Structural          StructuralOptions
	Semantic            SemanticOptions
	TitleMatchThreshold float64
}

// ValidationReport preserves every stage's evidence so callers can distinguish
// a hard rejection from a tool-capability review.
type ValidationReport struct {
	Payload    PayloadReport
	Structural StructuralReport
	Text       TextReport
	Metadata   MetadataFields
	Identity   IdentityDecision
	Evidence   []string
}

// Validate executes the complete validation pipeline. The cheap payload gate
// runs first; every structural parse is delegated to WorkerBinary.
func Validate(ctx context.Context, in ValidationInput, opt ValidationOptions) (ValidationReport, error) {
	var report ValidationReport
	payload, err := ValidatePayloadFile(in.Path, in.DeclaredMIME)
	if err != nil {
		return report, err
	}
	report.Payload = payload
	if !report.Payload.OK {
		return report, nil
	}
	if opt.Structural.PDFInfoPath == "" {
		// Detection is delegated to the caller/doctor surface; when present it
		// enables an independent external page-count cross-check.
		opt.Structural.PDFInfoPath = in.Capability.PDFInfo
	}
	structural, err := ValidateStructural(ctx, in.WorkerBinary, in.Path, opt.Structural)
	if err != nil {
		return report, err
	}
	report.Structural = structural
	if !structural.Valid {
		return report, nil
	}
	text, err := ExtractText(ctx, in.Path, in.Capability, opt.Semantic)
	if err != nil {
		return report, err
	}
	report.Text = text
	// Embedded metadata is read for every valid PDF, before the identity
	// decision, because the candidate-binding predicate consumes it as a
	// second source for the same identifier corroboration the text supplies
	// (see metadata.go). It is never required: absence yields empty fields and
	// no error, so a file no publisher produced validates exactly as before.
	metadata, err := ExtractMetadata(ctx, in.Path, in.Capability, opt.Semantic)
	if err != nil {
		return report, err
	}
	report.Metadata = metadata
	if text.NeedsReview {
		report.Identity = IdentityDecision{Result: IdentityReview, Evidence: append([]string(nil), text.Evidence...)}
		return report, nil
	}
	report.Identity = MatchIdentityWithThreshold(text.Excerpt, in.Target, opt.TitleMatchThreshold)
	return report, nil
}

// ValidateFile is the production entrypoint: unlike Validate it refuses to run
// at all without a worker binary, rather than reaching the structural stage and
// failing there. Validate stays worker-optional so the cheap payload gate can
// reject a file before any parse is attempted.
func ValidateFile(ctx context.Context, in ValidationInput, opt ValidationOptions) (ValidationReport, error) {
	if in.WorkerBinary == "" {
		return ValidationReport{}, errors.New("pdf worker binary is required")
	}
	return Validate(ctx, in, opt)
}
