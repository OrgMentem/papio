// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/store"
)

// fakePDFToText writes a pdftotext stand-in that echoes its arguments and then
// prints two pages, so a test sees both the page range the daemon asked for and
// the text it returned.
func fakePDFToText(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are POSIX shell scripts")
	}
	path := filepath.Join(t.TempDir(), "pdftotext")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'ARGS:%s|' \"$*\"\nprintf 'alpha\\fbeta\\f'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func detailClass(detail *ipc.ErrorDetail) string {
	if detail == nil {
		return ""
	}
	return detail.ErrorClass
}

func TestArtifactsTextServesOnlyReadyAndImportedJobs(t *testing.T) {
	system, id := readyZotioJobSystem(t, "TEXT0001")
	system.PDFCapability = pdf.Capability{PDFToText: fakePDFToText(t)}
	router := Router(system)
	ctx := context.Background()

	var ready ArtifactTextResult
	if rpcErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": id}, &ready); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !strings.Contains(ready.Text, "alpha") || ready.OCRUsed || ready.Truncated || ready.FirstPage != 1 || ready.PagesTotal != 1 {
		t.Fatalf("ready result = %+v", ready)
	}
	if ready.Chars != int64(len([]rune(ready.Text))) {
		t.Fatalf("chars = %d, text = %q", ready.Chars, ready.Text)
	}

	var ranged ArtifactTextResult
	if rpcErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": id, "first_page": 1, "last_page": 1}, &ranged); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !strings.HasPrefix(ranged.Text, "ARGS:-f 1 -l 1 ") {
		t.Fatalf("page range was not passed to pdftotext: %q", ranged.Text)
	}

	if err := system.Jobs.Transition(ctx, id, job.StateReady, job.StateImported, nil); err != nil {
		t.Fatal(err)
	}
	var imported ArtifactTextResult
	if rpcErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": id}, &imported); rpcErr != nil {
		t.Fatalf("imported job refused: %+v", rpcErr)
	}
	if !strings.Contains(imported.Text, "alpha") {
		t.Fatalf("imported result = %+v", imported)
	}

	// The artifact hash of the ready job resolves the same way.
	row, err := system.Jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var bySHA ArtifactTextResult
	if rpcErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"sha256": row.ArtifactSHA256}, &bySHA); rpcErr != nil {
		t.Fatalf("sha256 lookup refused: %+v", rpcErr)
	}
	if bySHA.Text != imported.Text {
		t.Fatalf("sha lookup text = %q, want %q", bySHA.Text, imported.Text)
	}
}

func TestArtifactsTextRefusesIneligibleTargetsWithDistinctErrors(t *testing.T) {
	system, readyID := readyZotioJobSystem(t, "TEXT0002")
	system.PDFCapability = pdf.Capability{PDFToText: fakePDFToText(t)}
	router := Router(system)
	ctx := context.Background()
	db := system.Store.DB()

	readyRow, err := system.Jobs.Get(ctx, readyID)
	if err != nil {
		t.Fatal(err)
	}
	sharedSHA := readyRow.ArtifactSHA256

	// A job that is not ready or imported, named by job id.
	queuedID := createAPIAttributionTestJob(t, system.Jobs, "api-text-queued", "10.1000/api-text-queued", "inscribi")
	stateErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": queuedID}, nil)

	// A hash held only by a job that is not ready or imported (an adoption
	// still in flight), with no ready/imported job to vouch for it.
	orphanSHA := strings.Repeat("c", 64)
	if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{SHA256: orphanSHA, SizeBytes: 1, MIME: "application/pdf", Path: filepath.Join(t.TempDir(), "orphan.pdf"), IdentityResult: "pass"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET artifact_sha256 = ? WHERE id = ?`, orphanSHA, queuedID); err != nil {
		t.Fatal(err)
	}
	adoptionErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"sha256": orphanSHA}, nil)

	// A quarantined file: bound to a human action, never promoted to an artifact.
	quarantineSHA := strings.Repeat("d", 64)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO human_actions (job_id, kind, status, quarantine_path, quarantine_sha256, revision, created_at)
		VALUES (?, 'verify_identity', 'open', '/quarantine/x.pdf', ?, 1, ?)`,
		queuedID, quarantineSHA, store.Now()); err != nil {
		t.Fatal(err)
	}
	quarantineErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"sha256": quarantineSHA}, nil)

	unknownErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"sha256": strings.Repeat("e", 64)}, nil)
	missingArgErr := callMethod(t, router, "artifacts.text_v1", map[string]any{}, nil)
	bothArgErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": readyID, "sha256": sharedSHA}, nil)

	if stateErr == nil || adoptionErr == nil || quarantineErr == nil || unknownErr == nil {
		t.Fatalf("a refusal was served: state=%v adoption=%v quarantine=%v unknown=%v", stateErr, adoptionErr, quarantineErr, unknownErr)
	}
	if unknownErr.Code != "not_found" {
		t.Fatalf("unknown sha code = %q, want not_found", unknownErr.Code)
	}
	for name, e := range map[string]string{"state": stateErr.Code, "adoption": adoptionErr.Code, "quarantine": quarantineErr.Code} {
		if e != "precondition_failed" {
			t.Fatalf("%s code = %q, want precondition_failed", name, e)
		}
	}
	classes := map[string]string{}
	for name, detail := range map[string]string{
		"state":      detailClass(stateErr.Detail),
		"adoption":   detailClass(adoptionErr.Detail),
		"quarantine": detailClass(quarantineErr.Detail),
		"unknown":    detailClass(unknownErr.Detail),
	} {
		if detail == "" {
			t.Fatalf("%s refusal carries no error_class", name)
		}
		if other, dup := classes[detail]; dup {
			t.Fatalf("%s and %s share error_class %q; the refusals must be distinguishable", name, other, detail)
		}
		classes[detail] = name
	}
	messages := map[string]bool{}
	for _, message := range []string{stateErr.Message, adoptionErr.Message, quarantineErr.Message, unknownErr.Message} {
		if messages[message] {
			t.Fatalf("duplicate refusal message %q", message)
		}
		messages[message] = true
	}
	if missingArgErr == nil || missingArgErr.Code != "invalid_argument" || bothArgErr == nil || bothArgErr.Code != "invalid_argument" {
		t.Fatalf("selector gate: missing=%v both=%v", missingArgErr, bothArgErr)
	}
}

func TestArtifactsTextRejectsBadRangesAndByteCaps(t *testing.T) {
	system, id := readyZotioJobSystem(t, "TEXT0003")
	system.PDFCapability = pdf.Capability{PDFToText: fakePDFToText(t)}
	router := Router(system)
	for name, params := range map[string]map[string]any{
		"past the end":    {"job_id": id, "first_page": 5},
		"reversed":        {"job_id": id, "first_page": 3, "last_page": 2},
		"negative":        {"job_id": id, "first_page": -1},
		"over the cap":    {"job_id": id, "max_bytes": pdf.MaxTextBytes + 1},
		"negative budget": {"job_id": id, "max_bytes": -1},
		// An explicit zero is not the omitted-cap default: the CLI refuses
		// --max-bytes 0, so the RPC refuses max_bytes 0 the same way
		// instead of silently serving the full default read.
		"explicit zero budget": {"job_id": id, "max_bytes": 0},
	} {
		rpcErr := callMethod(t, router, "artifacts.text_v1", params, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("%s = %+v, want invalid_argument", name, rpcErr)
		}
	}
	var lowered ArtifactTextResult
	if rpcErr := callMethod(t, router, "artifacts.text_v1", map[string]any{"job_id": id, "max_bytes": 8}, &lowered); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !lowered.Truncated || len(lowered.Text) != 8 {
		t.Fatalf("lowered cap result = %+v", lowered)
	}
}

func TestArtifactsTextWithoutPDFToTextIsATypedCapabilityError(t *testing.T) {
	system, id := readyZotioJobSystem(t, "TEXT0004")
	system.PDFCapability = pdf.Capability{}
	rpcErr := callMethod(t, Router(system), "artifacts.text_v1", map[string]any{"job_id": id}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" || detailClass(rpcErr.Detail) != ArtifactTextToolUnavailableClass {
		t.Fatalf("missing pdftotext = %+v, want precondition_failed/%s", rpcErr, ArtifactTextToolUnavailableClass)
	}
}
