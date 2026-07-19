// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"

	"papio/internal/work"
)

// ValidationInput accepts either a small in-memory Body (fixtures) or a Path
// produced by the fetch layer. Path validation reads bounded windows; hostile
// structural parsing stays exclusively in the re-exec worker.
type ValidationInput struct {
	Body         []byte
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
	Identity   IdentityDecision
	Evidence   []string
}

// Validate executes the complete validation pipeline. The cheap payload gate
// runs first; every structural parse is delegated to WorkerBinary.
func Validate(ctx context.Context, in ValidationInput, opt ValidationOptions) (ValidationReport, error) {
	var report ValidationReport
	if in.Body != nil {
		report.Payload = ValidatePayload(in.Body, in.DeclaredMIME)
	} else {
		payload, err := ValidatePayloadFile(in.Path, in.DeclaredMIME)
		if err != nil {
			return report, err
		}
		report.Payload = payload
	}
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
	if text.NeedsReview {
		report.Identity = IdentityDecision{Result: IdentityReview, Evidence: append([]string(nil), text.Evidence...)}
		return report, nil
	}
	report.Identity = MatchIdentityWithThreshold(text.Excerpt, in.Target, opt.TitleMatchThreshold)
	return report, nil
}

// ValidateBytes is a convenient name for callers emphasizing the parent-side
// byte gate. It rejects a nil WorkerBinary rather than ever parsing locally.
// A filesystem in.Path is required: Body only satisfies the in-memory payload
// gate, while structural validation and text extraction always read in.Path
// from disk. Passing Body without Path fails once validation reaches those
// stages.
func ValidateBytes(ctx context.Context, in ValidationInput, opt ValidationOptions) (ValidationReport, error) {
	if in.WorkerBinary == "" {
		return ValidationReport{}, errors.New("pdf worker binary is required")
	}
	return Validate(ctx, in, opt)
}
