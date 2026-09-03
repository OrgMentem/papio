// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/discovery"
	"papio/internal/protocol"
	"papio/internal/watch"
	"papio/internal/zotio"
)

// stubWatchBackend stands in for every dependency watch.Runner composes. The
// runner is a concrete struct, not an interface, so a handler-level failure
// test builds a real runner over the test's own store and swaps only the
// backends. Search is the first dependency a discovery run reaches, so its
// error is what the handler must classify; Lookup and Submitter exist to get
// past the runner's "dependencies are not configured" guard.
type stubWatchBackend struct {
	searchErr error
}

func (s stubWatchBackend) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return nil, s.searchErr
}

func (s stubWatchBackend) LookupWorks(context.Context, zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error) {
	return &zotio.LookupWorksResult{}, nil
}

func (s stubWatchBackend) SubmitWithAutoImport(context.Context, protocol.WorkRequest, *bool) (string, error) {
	return "", errors.New("stub submitter must not be reached")
}

// watchRunnerFixture returns a system whose WatchRunner is a real runner over
// the test's store with stubbed backends, plus one discovery watch to address.
func watchRunnerFixture(t *testing.T, searchErr error) (system *bootstrap.System, watchID int64) {
	t.Helper()
	sys := testSystem(t)
	created, err := sys.Watches.Create(context.Background(), watch.CreateInput{
		Query: "handler coverage", CadenceHours: 24, PerRunCap: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := stubWatchBackend{searchErr: searchErr}
	sys.WatchRunner = &watch.Runner{
		Store:     sys.Watches,
		Discovery: backend,
		Lookup:    backend,
		Submitter: backend,
		DataDir:   sys.Config.DataDir,
	}
	return sys, created.ID
}

// TestWatchRunHandlerReportsInternalForBackendFailure pins watch.run's error
// taxonomy. A watch execution failure is never a routine 'not found': the
// handler routes through watchFailure, so even a missing watch row — whose
// underlying error IS sql.ErrNoRows — must surface as internal with the watch
// error class, not as the not_found the generic failure helper would produce.
func TestWatchRunHandlerReportsInternalForBackendFailure(t *testing.T) {
	system, watchID := watchRunnerFixture(t, errors.New("fixture discovery backend refused the request"))
	router := Router(system)

	for _, tc := range []struct {
		name string
		id   int64
	}{
		{name: "backend failure", id: watchID},
		{name: "missing watch row", id: watchID + 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := callMethod(t, router, "watch.run", map[string]any{"id": tc.id}, nil)
			if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Message != "watch execution failed" {
				t.Fatalf("watch.run = %#v, want internal/watch execution failed", rpcErr)
			}
			if rpcErr.Detail == nil || rpcErr.Detail.ErrorClass != "watch_execution_failed" {
				t.Fatalf("watch.run detail = %#v, want watch_execution_failed class", rpcErr.Detail)
			}
		})
	}

	for _, params := range []map[string]any{
		{},
		{"id": 0},
		{"id": -1},
		{"id": watchID, "unexpected": true},
		{"id": "not-a-number"},
	} {
		if rpcErr := callMethod(t, router, "watch.run", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("watch.run %v = %#v, want invalid_argument", params, rpcErr)
		}
	}

	// A daemon without a runner cannot answer at all, and that is a
	// precondition rather than a bad request or an execution failure.
	system.WatchRunner = nil
	if rpcErr := callMethod(t, router, "watch.run", map[string]any{"id": watchID}, nil); rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("watch.run without runner = %#v, want precondition_failed", rpcErr)
	}
}

// TestWatchDigestClearHandlerRejectsBadIDAndMissingRunner covers
// watch.digest_clear end to end: the parameter gate, the runner precondition,
// and the count it reports for a digest it really consumed.
func TestWatchDigestClearHandlerRejectsBadIDAndMissingRunner(t *testing.T) {
	system, watchID := watchRunnerFixture(t, errors.New("fixture discovery backend refused the request"))
	router := Router(system)

	for _, params := range []map[string]any{
		{},
		{"id": 0},
		{"id": -3},
		{"id": watchID, "keys": []string{"10.1000/one"}}, // digest_clear takes no keys
		{"id": "not-a-number"},
	} {
		if rpcErr := callMethod(t, router, "watch.digest_clear", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("watch.digest_clear %v = %#v, want invalid_argument", params, rpcErr)
		}
	}

	recorded, err := system.Watches.RecordDigest(context.Background(), watchID, time.Now(), []watch.DigestEntry{
		{WorkKey: "10.1000/clear-one", Title: "Clear One", DOI: "10.1000/clear-one", Abstract: "Context"},
		{WorkKey: "10.1000/clear-two", Title: "Clear Two", DOI: "10.1000/clear-two", Abstract: "Context"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorded != 2 {
		t.Fatalf("recorded digest entries = %d, want 2", recorded)
	}

	var cleared WatchDigestClearResult
	if rpcErr := callMethod(t, router, "watch.digest_clear", map[string]any{"id": watchID}, &cleared); rpcErr != nil {
		t.Fatalf("watch.digest_clear = %#v", rpcErr)
	}
	if cleared.Cleared != recorded {
		t.Fatalf("cleared = %d, want %d", cleared.Cleared, recorded)
	}
	// Clearing is a consume, not a query: a second call finds nothing left and
	// still succeeds rather than reporting a missing digest.
	var again WatchDigestClearResult
	if rpcErr := callMethod(t, router, "watch.digest_clear", map[string]any{"id": watchID}, &again); rpcErr != nil {
		t.Fatalf("second watch.digest_clear = %#v", rpcErr)
	}
	if again.Cleared != 0 {
		t.Fatalf("second clear = %d, want 0", again.Cleared)
	}

	system.WatchRunner = nil
	if rpcErr := callMethod(t, router, "watch.digest_clear", map[string]any{"id": watchID}, nil); rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("watch.digest_clear without runner = %#v, want precondition_failed", rpcErr)
	}
}

// TestBrowserDevReloadHandlerRejectsUnknownFieldAndMissingHolder covers the
// only two answers browser.dev_reload can give without a live extension
// attached to the bridge. A latched reload needs a session that completed the
// bridge handshake and reports a dev-capable extension version, which no
// in-process test fixture can stand up, so success is deliberately untested
// here and belongs to internal/browser's own bridge tests.
func TestBrowserDevReloadHandlerRejectsUnknownFieldAndMissingHolder(t *testing.T) {
	router := Router(testSystem(t))

	// The method takes no parameters at all, so an invented field is refused
	// rather than ignored: a caller must never believe it scoped a reload to a
	// session the daemon never read.
	if rpcErr := callMethod(t, router, "browser.dev_reload", map[string]any{"session_id": "sess-1"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("browser.dev_reload with unknown field = %#v, want invalid_argument", rpcErr)
	}

	rpcErr := callMethod(t, router, "browser.dev_reload", map[string]any{}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("browser.dev_reload without holder = %#v, want invalid_argument", rpcErr)
	}
	if rpcErr.Message != "no browser session holds the bridge" {
		t.Fatalf("browser.dev_reload message = %q, want the bridge-holder refusal", rpcErr.Message)
	}
}

// TestExportBundleV2ReturnsPopulatedBundleAndWritesFile pins the one thing
// bundle.export_v2 exists for: the same export bundle.export performs, with the
// document returned inline. The written file is asserted too, because the file
// at Path — not the response body — is the canonical record either way.
func TestExportBundleV2ReturnsPopulatedBundleAndWritesFile(t *testing.T) {
	system, jobID, art := readyBundleSystem(t)
	router := Router(system)
	output := t.TempDir()

	for _, params := range []map[string]any{
		{},
		{"job_id": jobID},
		{"output_dir": output},
		{"job_id": "", "output_dir": output},
		{"job_id": jobID, "output_dir": output, "unexpected": true},
	} {
		if rpcErr := callMethod(t, router, "bundle.export_v2", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("bundle.export_v2 %v = %#v, want invalid_argument", params, rpcErr)
		}
	}

	if rpcErr := callMethod(t, router, "bundle.export_v2", map[string]any{
		"job_id": "job-does-not-exist", "output_dir": output,
	}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("bundle.export_v2 for unknown job = %#v, want not_found", rpcErr)
	}

	var result BundleResult
	if rpcErr := callMethod(t, router, "bundle.export_v2", map[string]any{
		"job_id": jobID, "output_dir": output,
	}, &result); rpcErr != nil {
		t.Fatalf("bundle.export_v2 = %#v", rpcErr)
	}
	if want := filepath.Join(output, "bundle.json"); result.Path != want {
		t.Fatalf("path = %q, want %q", result.Path, want)
	}
	if result.Bundle == nil {
		t.Fatal("bundle.export_v2 returned a nil bundle body; the populated body is the whole reason this method exists")
	}
	if result.Bundle.JobID != jobID || result.Bundle.SchemaVersion == "" || result.Bundle.ProvenanceDigest == "" {
		t.Fatalf("bundle body = %+v", result.Bundle)
	}
	if result.Bundle.Artifact.SHA256 != art.SHA256 || result.Bundle.Artifact.SizeBytes != art.SizeBytes {
		t.Fatalf("bundle artifact = %+v, want sha %s size %d", result.Bundle.Artifact, art.SHA256, art.SizeBytes)
	}
	if result.Bundle.Validation.Identity != "pass" {
		t.Fatalf("bundle validation = %+v, want a passing identity", result.Bundle.Validation)
	}

	// The response is only a convenience; the export must exist on disk, and
	// the artifact it names must be there beside it.
	written, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeAcquisitionBundle(written)
	if err != nil {
		t.Fatalf("written bundle.json does not decode: %v", err)
	}
	if decoded.JobID != result.Bundle.JobID || decoded.ProvenanceDigest != result.Bundle.ProvenanceDigest {
		t.Fatalf("written document %+v disagrees with the returned body %+v", decoded, result.Bundle)
	}
	artifactPath := filepath.Join(output, filepath.FromSlash(decoded.Artifact.Path))
	info, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatalf("exported artifact missing: %v", err)
	}
	if info.Size() != decoded.Artifact.SizeBytes {
		t.Fatalf("exported artifact size = %d, want %d", info.Size(), decoded.Artifact.SizeBytes)
	}

	// bundle.export is the same export without the body, and that difference is
	// a wire contract: an older CLI decodes results with unknown fields
	// refused, so v1 must keep answering with a nil bundle.
	var v1 BundleResult
	if rpcErr := callMethod(t, router, "bundle.export", map[string]any{
		"job_id": jobID, "output_dir": t.TempDir(),
	}, &v1); rpcErr != nil {
		t.Fatalf("bundle.export = %#v", rpcErr)
	}
	if v1.Bundle != nil {
		t.Fatalf("bundle.export returned a body: %+v", v1.Bundle)
	}
}
