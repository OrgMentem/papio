// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/pdf"
)

// Error classes for artifacts.text_v1 refusals. Each refusal has its own class
// so a caller can tell "wait for the job" from "this hash is not text-eligible"
// from "papio has never seen this hash".
const (
	ArtifactTextJobStateClass        = "artifact_text_job_state"
	ArtifactTextNotEligibleClass     = "artifact_text_not_eligible"
	ArtifactTextQuarantinedClass     = "artifact_text_quarantined"
	ArtifactTextNotFoundClass        = "artifact_text_not_found"
	ArtifactTextToolUnavailableClass = "artifact_text_tool_unavailable"
	ArtifactTextTimeoutClass         = "artifact_text_timeout"
)

// ArtifactTextResult is the artifacts.text_v1 result: extracted text for one
// page range of a validated artifact. Text never exceeds pdf.MaxTextBytes, which
// is sized to fit ipc.MaxResultBytes after JSON escaping; page ranges are the
// paging mechanism. OCRUsed is always false: this method never runs OCR.
type ArtifactTextResult struct {
	Text       string `json:"text"`
	FirstPage  int    `json:"first_page"`
	LastPage   int    `json:"last_page"`
	PagesTotal int    `json:"pages_total"`
	Chars      int64  `json:"chars"`
	Truncated  bool   `json:"truncated"`
	OCRUsed    bool   `json:"ocr_used"`
}

// artifactText is artifacts.text_v1: a pure read of an artifact's text through
// the same bounded pdftotext path validation uses. Only the artifact of a job
// in state ready or imported qualifies; quarantined bytes, adoption in flight
// and unknown hashes are refused, each with its own error class.
func artifactText(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID     string `json:"job_id,omitempty"`
		SHA256    string `json:"sha256,omitempty"`
		FirstPage int    `json:"first_page,omitempty"`
		LastPage  int    `json:"last_page,omitempty"`
		// MaxBytes is a pointer so an explicitly sent zero stays
		// distinguishable from an omitted cap: zero means "no cap was
		// named" on the wire, and the CLI rejects --max-bytes 0, so the
		// RPC must reject an explicit 0 the same way rather than silently
		// serving the default.
		MaxBytes *int `json:"max_bytes,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if (params.JobID == "") == (params.SHA256 == "") {
		return badParams(errors.New("exactly one of job_id or sha256 is required"))
	}
	if params.FirstPage < 0 || params.LastPage < 0 || (params.LastPage != 0 && params.FirstPage > params.LastPage) {
		return badParams(fmt.Errorf("invalid page range %d-%d", params.FirstPage, params.LastPage))
	}
	if params.MaxBytes != nil && (*params.MaxBytes < 1 || *params.MaxBytes > pdf.MaxTextBytes) {
		return badParams(fmt.Errorf("max_bytes must be between 1 and %d", pdf.MaxTextBytes))
	}
	maxBytes := 0
	if params.MaxBytes != nil {
		maxBytes = *params.MaxBytes
	}
	if system == nil || system.Jobs == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "job store is not configured"}
	}
	sha, rpcErr := textEligibleSHA(ctx, system, params.JobID, params.SHA256)
	if rpcErr != nil {
		return nil, rpcErr
	}
	artifact, err := system.Jobs.GetArtifact(ctx, sha)
	if err != nil {
		return failure(err)
	}
	if artifact == nil {
		return nil, textRefusal("not_found", ArtifactTextNotFoundClass, "artifact not found")
	}
	if artifact.MIME != "application/pdf" {
		return nil, textRefusal("precondition_failed", ArtifactTextNotEligibleClass, "artifact is not a PDF; text extraction needs a PDF")
	}
	result, err := pdf.ExtractPageText(ctx, artifact.Path, system.PDFCapability, pdf.DefaultSemanticOptions(), pdf.PageTextRequest{
		FirstPage: params.FirstPage, LastPage: params.LastPage, PagesTotal: artifact.PageCount, MaxBytes: maxBytes,
	})
	switch {
	case errors.Is(err, pdf.ErrToolUnavailable):
		return nil, textRefusal("precondition_failed", ArtifactTextToolUnavailableClass,
			"pdftotext (Poppler) is not installed on the daemon host; install it to read artifact text")
	case errors.Is(err, pdf.ErrInvalidPageRange), errors.Is(err, pdf.ErrTextTooLarge):
		return badParams(err)
	case errors.Is(err, context.DeadlineExceeded):
		return nil, textRefusal("unavailable", ArtifactTextTimeoutClass, "text extraction timed out; request a narrower page range")
	case err != nil:
		log.Printf("rpc artifacts.text_v1: %v", err)
		return failure(err)
	}
	return marshal(ArtifactTextResult{
		Text: result.Text, FirstPage: result.FirstPage, LastPage: result.LastPage,
		PagesTotal: result.PagesTotal, Chars: result.Chars, Truncated: result.Truncated, OCRUsed: false,
	})
}

// textEligibleSHA resolves a job id or a hash to an artifact hash that
// artifacts.text_v1 may serve.
func textEligibleSHA(ctx context.Context, system *bootstrap.System, jobID, sha string) (string, *ipc.RPCError) {
	eligible := func(state string) bool { return state == job.StateReady || state == job.StateImported }
	if jobID != "" {
		row, err := system.Jobs.Get(ctx, jobID)
		if err != nil {
			_, rpcErr := failure(err)
			return "", rpcErr
		}
		if !eligible(row.State) {
			return "", textRefusal("precondition_failed", ArtifactTextJobStateClass,
				fmt.Sprintf("job is %s; text is served only for ready or imported jobs", row.State))
		}
		if row.ArtifactSHA256 == "" {
			return "", textRefusal("not_found", ArtifactTextNotFoundClass, "job has no validated artifact")
		}
		return row.ArtifactSHA256, nil
	}
	states, err := system.Jobs.ArtifactHolderStates(ctx, sha)
	if err != nil {
		_, rpcErr := failure(err)
		return "", rpcErr
	}
	if slices.ContainsFunc(states, eligible) {
		return sha, nil
	}
	if len(states) > 0 {
		return "", textRefusal("precondition_failed", ArtifactTextNotEligibleClass,
			"artifact is held only by jobs that are not ready or imported ("+strings.Join(states, ", ")+")")
	}
	quarantined, err := system.Jobs.IsQuarantinedSHA(ctx, sha)
	if err != nil {
		_, rpcErr := failure(err)
		return "", rpcErr
	}
	if quarantined {
		return "", textRefusal("precondition_failed", ArtifactTextQuarantinedClass,
			"file is quarantined awaiting review; text is not served for quarantined bytes")
	}
	return "", textRefusal("not_found", ArtifactTextNotFoundClass, "artifact not found")
}

func textRefusal(code, class, message string) *ipc.RPCError {
	return &ipc.RPCError{Code: code, Message: message, Detail: &ipc.ErrorDetail{ErrorClass: class}}
}
