// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/ipc"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/zotio"

	_ "modernc.org/sqlite"
)

// baseBatchCaller answers the daemon RPCs every batch mock needs alike: an
// ownership lookup reports every work not owned, and jobs.get reports a queued
// job. Mocks embed it and handle only the methods their test is about,
// delegating the rest so an unexpected method still fails loudly.
type baseBatchCaller struct{}

func (baseBatchCaller) Call(_ context.Context, method string, params, result any) error {
	switch method {
	case "zotio.lookup_works":
		request := params.(zotio.LookupWorksRequest)
		out := result.(*zotio.LookupWorksResult)
		out.Works = make([]zotio.WorkOwnership, len(request.Works))
		for i := range out.Works {
			out.Works[i].Status = zotio.OwnershipNotOwned
		}
	case "jobs.get":
		result.(*jobDetail).Job = json.RawMessage(`{"id":"job-x","state":"queued"}`)
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	return nil
}

// methodRecorder records which RPCs a mock caller was asked for.
type methodRecorder struct {
	mu      sync.Mutex
	methods []string
}

func (r *methodRecorder) record(method string) {
	r.mu.Lock()
	r.methods = append(r.methods, method)
	r.mu.Unlock()
}

func (r *methodRecorder) called(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, seen := range r.methods {
		if seen == method {
			return true
		}
	}
	return false
}

type resolverBatchCaller struct {
	baseBatchCaller
	t        *testing.T
	resolver string
}

func (c resolverBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		request := params.(submitParams).Request
		if request.Resolver != c.resolver {
			c.t.Errorf("resolver = %q, want %q", request.Resolver, c.resolver)
		}
		result.(*submitResult).JobID = "job-resolver-profile"
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
	return nil
}

func TestSubmitAppliesResolverProfileToEveryBatchRequest(t *testing.T) {
	request := protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      "batch-resolver-request",
		Identifiers:    &protocol.Identifiers{DOI: "10.1000/resolver"},
		DesiredVersion: "any",
	}
	output, err := Submit(context.Background(), resolverBatchCaller{t: t, resolver: "institute"}, t.TempDir(), []protocol.WorkRequest{request}, SubmitOptions{Resolver: "institute"})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].State != "queued" {
		t.Fatalf("output = %+v", output)
	}
}

type collectionBatchCaller struct {
	baseBatchCaller
	mu          sync.Mutex
	collections []string
}

func (c *collectionBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		request := params.(submitParams).Request
		c.mu.Lock()
		c.collections = append(c.collections, request.Collection)
		c.mu.Unlock()
		result.(*submitResult).JobID = "job-collection-default"
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
	return nil
}

type fingerprintBatchCaller struct {
	baseBatchCaller
	methodRecorder
	expectedFingerprint string
	lookupErr           error
}

func (c *fingerprintBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	c.record(method)

	switch method {
	case "library.lookup_works":
		request := params.(libraryLookupParams)
		if request.ExpectedFingerprint != c.expectedFingerprint {
			return fmt.Errorf("expected fingerprint = %q, got %q", c.expectedFingerprint, request.ExpectedFingerprint)
		}
		if c.lookupErr != nil {
			return c.lookupErr
		}
		result.(*ownership.Result).Works = make([]ownership.WorkResult, len(request.Works))
	case "acquire.submit_v2":
		result.(*submitResult).JobID = "job-fingerprint"
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
	return nil
}

func TestSubmitBindsHoldingsLookupToFingerprint(t *testing.T) {
	caller := &fingerprintBatchCaller{expectedFingerprint: "library-fingerprint"}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{doiWork("batch-fingerprint", "10.1000/fingerprint")},
		SubmitOptions{Holdings: true, LibraryFingerprint: "library-fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Submitted) != 1 {
		t.Fatalf("Submitted = %+v, want one job", output.Submitted)
	}
	if !caller.called("library.lookup_works") {
		t.Fatal("generic holdings lookup was not called")
	}
}

func TestSubmitDoesNotFallbackOnLibraryPreconditionFailure(t *testing.T) {
	caller := &fingerprintBatchCaller{
		expectedFingerprint: "library-fingerprint",
		lookupErr:           &ipc.RemoteError{Code: "precondition_failed", Message: "library configuration does not match caller"},
	}
	output, err := Submit(context.Background(), caller, t.TempDir(),
		[]protocol.WorkRequest{doiWork("batch-fingerprint-mismatch", "10.1000/fingerprint-mismatch")},
		SubmitOptions{Holdings: true, LibraryFingerprint: "library-fingerprint"})
	if err == nil {
		t.Fatal("library precondition failure must stop the batch")
	}
	if output != nil {
		t.Fatalf("output = %+v, want nil after precondition failure", output)
	}
	if caller.called("zotio.lookup_works") {
		t.Fatal("a library precondition failure must not use the unknown-method fallback")
	}
	if caller.called("acquire.submit_v2") {
		t.Fatal("a library precondition failure must not create jobs")
	}
}

func doiWork(requestID, doi string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      requestID,
		Identifiers:    &protocol.Identifiers{DOI: doi},
		DesiredVersion: "any",
	}
}

// An unset collection falls back to the batch's label so imported papers are
// filed under the search that produced them instead of landing loose in the
// library root; an explicit collection always wins.
func TestSubmitCollectionFallsBackToLabel(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		collection string
		want       string
	}{
		{name: "collection unset falls back to label", label: "evidence synthesis", collection: "", want: "evidence synthesis"},
		{name: "explicit collection wins over label", label: "evidence synthesis", collection: "Reading", want: "Reading"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := &collectionBatchCaller{}
			work := doiWork("batch-collection", "10.1000/collection")
			if _, err := Submit(context.Background(), caller, t.TempDir(), []protocol.WorkRequest{work}, SubmitOptions{Label: test.label, Collection: test.collection}); err != nil {
				t.Fatal(err)
			}
			if len(caller.collections) != 1 || caller.collections[0] != test.want {
				t.Fatalf("submitted collections = %q, want [%s]", caller.collections, test.want)
			}
		})
	}
}

func TestParseWorkRejectsUnknownFields(t *testing.T) {
	for _, data := range []string{
		`{"doi":"10.1000/example","DOIs":["10.1000/typo"]}`,
		`{"work":{"doi":"10.1000/example","author":"Ada"}}`,
		`{"work":{"doi":"10.1000/example"},"typo":true}`,
	} {
		if _, err := ParseWork([]byte(data)); err == nil {
			t.Fatalf("ParseWork(%s) accepted an unknown field", data)
		}
	}
}

func TestParseWorkAcceptsDiscoveredWorkEnvelope(t *testing.T) {
	request, err := ParseWork([]byte(`{"work":{"doi":"10.1000/example","container":"Journal"},"openalex_id":"W12345","is_oa":true,"oa_url":"https://example.test/paper","cited_by":1,"abstract":"Summary","owned":false,"owned_item_key":"AB12CD34"}`))
	if err != nil {
		t.Fatalf("ParseWork discovered envelope: %v", err)
	}
	if request.Identifiers == nil || request.Identifiers.DOI != "10.1000/example" {
		t.Fatalf("request identifiers = %#v", request.Identifiers)
	}
}

func TestBatchRequestIDSeparatesLegacyPrefixCollision(t *testing.T) {
	const first = "10.1000/collision-11784"
	const second = "10.1000/collision-77155"

	firstSum := sha256.Sum256([]byte("doi:" + first))
	secondSum := sha256.Sum256([]byte("doi:" + second))
	if string(firstSum[:4]) != string(secondSum[:4]) {
		t.Fatal("test inputs must collide on the legacy four-byte hash prefix")
	}

	firstID := batchRequestID(&protocol.Identifiers{DOI: first}, "", nil, 0)
	secondID := batchRequestID(&protocol.Identifiers{DOI: second}, "", nil, 0)
	if firstID == secondID {
		t.Fatalf("batch request IDs collided: %q", firstID)
	}
	if len(firstID) != len("batch-")+batchIdentityHashBytes*2 {
		t.Fatalf("batch request ID length = %d, want %d", len(firstID), len("batch-")+batchIdentityHashBytes*2)
	}
}

// legacyDaemonCaller answers acquire.submit_v2 the way a pre-0.13.0 daemon
// does, so the batch path has to reach the retained v1 method.
type legacyDaemonCaller struct {
	baseBatchCaller
	t          *testing.T
	mu         sync.Mutex
	sawBareReq bool
}

func (c *legacyDaemonCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
	case "acquire.submit":
		// With no auto-import override the legacy method was always sent a bare
		// WorkRequest; preserving that is part of speaking v1 correctly.
		if _, ok := params.(protocol.WorkRequest); ok {
			c.mu.Lock()
			c.sawBareReq = true
			c.mu.Unlock()
		} else {
			c.t.Errorf("legacy params = %#v, want a bare protocol.WorkRequest", params)
		}
		result.(*submitResult).JobID = "job-legacy"
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
	return nil
}

// TestBatchFallsBackToLegacySubmitOnAnOlderDaemon pins the mixed-version case.
// Without the fallback every work in the batch records submission_failed with
// unknown_method, because one binary serves as CLI, daemon and native host and
// a newer CLI routinely meets an older running daemon.
func TestBatchFallsBackToLegacySubmitOnAnOlderDaemon(t *testing.T) {
	caller := &legacyDaemonCaller{t: t}
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "wr_legacy_fallback",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/legacy-fallback"},
	}
	output, err := Submit(context.Background(), caller, t.TempDir(), []protocol.WorkRequest{request}, SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit = %v, want the legacy method used instead of a failure", err)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].JobID != "job-legacy" {
		t.Fatalf("submitted = %+v, want the job id the legacy method returned", output.Submitted)
	}
	if !caller.sawBareReq {
		t.Fatal("legacy submit never received the bare WorkRequest form")
	}
}

func TestSubmitRejectsDuplicateWorks(t *testing.T) {
	caller := &fingerprintBatchCaller{expectedFingerprint: ""}
	work := doiWork("dup", "10.1000/dup")
	dup := doiWork("dup2", "10.1000/dup")
	output, err := Submit(context.Background(), caller, t.TempDir(), []protocol.WorkRequest{work, dup}, SubmitOptions{})
	if err == nil {
		t.Fatalf("Submit with duplicates = %+v, want an error naming the duplicate", output)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error %q must name the duplicate", err.Error())
	}
	if caller.called("acquire.submit_v2") || caller.called("zotio.lookup_works") || caller.called("library.lookup_works") {
		t.Fatal("a duplicate batch must fail before any daemon RPC")
	}
}

func TestSubmitDistinctWorksBehaveUnchanged(t *testing.T) {
	caller := &fingerprintBatchCaller{expectedFingerprint: ""}
	works := []protocol.WorkRequest{
		doiWork("r1", "10.1000/a"),
		doiWork("r2", "10.1000/b"),
	}
	output, err := Submit(context.Background(), caller, t.TempDir(), works, SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit = %v", err)
	}
	if len(output.Submitted) != 2 {
		t.Fatalf("Submitted = %+v, want 2", output.Submitted)
	}
	if output.Submitted[0].RequestID == output.Submitted[1].RequestID {
		t.Fatalf("distinct works must have distinct RequestIDs: %q", output.Submitted[0].RequestID)
	}
	if output.Submitted[0].RequestID == "" || output.Submitted[1].RequestID == "" {
		t.Fatalf("RequestIDs must be populated: %+v", output.Submitted)
	}
	if output.Submitted[0].JobID == "" || output.Submitted[1].JobID == "" {
		t.Fatalf("JobIDs must be populated: %+v", output.Submitted)
	}
}

// blockingStateCaller holds every jobs.get until its release channel closes,
// so a test can observe what an interrupted Submit left on disk before the
// final manifest write.
type blockingStateCaller struct {
	baseBatchCaller
	mu      sync.Mutex
	submits int
	release chan struct{}
	jobID   string
	state   string
}

func (c *blockingStateCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		c.mu.Lock()
		c.submits++
		c.mu.Unlock()
		result.(*submitResult).JobID = c.jobID
		return nil
	case "jobs.get":
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		result.(*jobDetail).Job = json.RawMessage(`{"id":` + strconv.Quote(c.jobID) + `,"state":` + strconv.Quote(c.state) + `}`)
		return nil
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
}

// replayBatchCaller never creates jobs. It answers jobs.get from a fixed
// state table so a retry must reattach to already-known jobs or fail loudly.
type replayBatchCaller struct {
	baseBatchCaller
	mu      sync.Mutex
	submits int
	states  map[string]string
}

func (c *replayBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		c.mu.Lock()
		c.submits++
		c.mu.Unlock()
		return fmt.Errorf("retry must not submit work again")
	case "jobs.get":
		id := params.(map[string]string)["job_id"]
		state, ok := c.states[id]
		if !ok {
			return fmt.Errorf("job %q is gone", id)
		}
		result.(*jobDetail).Job = json.RawMessage(`{"id":` + strconv.Quote(id) + `,"state":` + strconv.Quote(state) + `}`)
		return nil
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
}

// A retry after an interrupted batch must reattach to the original jobs,
// including terminal ones, instead of submitting the work again. The first
// Submit is stalled inside its state read; the association must already be
// durable on disk before that read completes.
func TestSubmitRetryReusesTerminalJobsAfterInterruption(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-one", "10.1000/retry-one")}
	manifestPath := filepath.Join(dataDir, "batches", ID(works, now)+".json")

	first := &blockingStateCaller{release: make(chan struct{}), jobID: "job-interrupted", state: "queued"}
	type submitOutcome struct {
		output *SubmitOutput
		err    error
	}
	done := make(chan submitOutcome, 1)
	go func() {
		output, err := Submit(context.Background(), first, dataDir, []protocol.WorkRequest{doiWork("retry-one", "10.1000/retry-one")}, SubmitOptions{Now: now})
		done <- submitOutcome{output, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(manifestPath)
		if err == nil && strings.Contains(string(data), "job-interrupted") {
			break
		}
		if time.Now().After(deadline) {
			close(first.release)
			t.Fatalf("interrupted Submit left no durable job association before its state read (read err %v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(first.release)
	firstOut := <-done
	if firstOut.err != nil {
		t.Fatalf("first Submit = %v", firstOut.err)
	}
	if len(firstOut.output.Submitted) != 1 || firstOut.output.Submitted[0].JobID != "job-interrupted" {
		t.Fatalf("first Submitted = %+v, want job-interrupted", firstOut.output.Submitted)
	}

	// The job completes while the CLI is gone. The retry runs with the same
	// clock and work set, so it derives the same batch identity.
	replay := &replayBatchCaller{states: map[string]string{"job-interrupted": "ready"}}
	output, err := Submit(context.Background(), replay, dataDir, []protocol.WorkRequest{doiWork("retry-one", "10.1000/retry-one")}, SubmitOptions{Now: now})
	if err != nil {
		t.Fatalf("retry Submit = %v", err)
	}
	if replay.submits != 0 {
		t.Fatalf("retry submitted %d new jobs, want none", replay.submits)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].JobID != "job-interrupted" {
		t.Fatalf("retry Submitted = %+v, want the original terminal job", output.Submitted)
	}
	if output.Submitted[0].State != "ready" {
		t.Fatalf("retry state = %q, want ready", output.Submitted[0].State)
	}
	manifest, err := Load(dataDir, output.BatchID)
	if err != nil {
		t.Fatalf("Load retry manifest = %v", err)
	}
	if len(manifest.Works) != 1 || manifest.Works[0].JobID != "job-interrupted" {
		t.Fatalf("retry manifest = %+v, want the original job, not a replacement", manifest.Works)
	}
}

// goneJobBatchCaller answers jobs.get only for the job it just created, so a
// test can prove a retry falls back to a fresh submit when the prior job is
// gone from the daemon.
type goneJobBatchCaller struct {
	baseBatchCaller
	jobID string
}

func (c *goneJobBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		result.(*submitResult).JobID = c.jobID
		return nil
	case "jobs.get":
		id := params.(map[string]string)["job_id"]
		if id != c.jobID {
			return fmt.Errorf("job %q is gone", id)
		}
		result.(*jobDetail).Job = json.RawMessage(`{"id":` + strconv.Quote(id) + `,"state":"queued"}`)
		return nil
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
}

// A recorded job the daemon no longer confirms, with no readable store to
// prove absence, must stop the work instead of risking a duplicate provider
// effect.
func TestSubmitRefusesResubmitWhenStoreUnreadable(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-two", "10.1000/retry-two")}
	batchID := ID(works, now)
	stale := NewManifest(works, "", "", now)
	stale.Works[0].JobID = "job-gone"
	if err := Write(dataDir, stale); err != nil {
		t.Fatalf("Write stale manifest = %v", err)
	}
	caller := &goneJobBatchCaller{jobID: "job-fresh"}
	output, err := Submit(context.Background(), caller, dataDir, []protocol.WorkRequest{doiWork("retry-two", "10.1000/retry-two")}, SubmitOptions{Now: now})
	if err == nil || !strings.Contains(err.Error(), "refusing to resubmit") {
		t.Fatalf("Submit err = %v, want a refusal to resubmit", err)
	}
	if output == nil || len(output.Submitted) != 0 || output.Failed != 1 {
		t.Fatalf("output = %+v, want no submissions and one failure", output)
	}
	if output.BatchID != batchID {
		t.Fatalf("BatchID = %q, want %q", output.BatchID, batchID)
	}
}

// seedJobsDB builds the smallest database the store inspector can read: a
// papio.db holding only the jobs rows the test needs.
func seedJobsDB(t *testing.T, dataDir string, rows [][3]string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "papio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, work_request_id TEXT, state TEXT, created_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO jobs (id, work_request_id, state, created_at) VALUES (?, ?, ?, ?)`, row[0], row[1], row[2], "2026-09-26T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
}

// A readable store proving absence lets the retry submit anew: the recorded
// job is really gone, not merely unconfirmed.
func TestSubmitProvesAbsenceViaStoreThenSubmits(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-three", "10.1000/retry-three")}
	batchID := ID(works, now)
	stale := NewManifest(works, "", "", now)
	stale.Works[0].JobID = "job-gone"
	if err := Write(dataDir, stale); err != nil {
		t.Fatalf("Write stale manifest = %v", err)
	}
	seedJobsDB(t, dataDir, nil)
	caller := &goneJobBatchCaller{jobID: "job-fresh"}
	output, err := Submit(context.Background(), caller, dataDir, []protocol.WorkRequest{doiWork("retry-three", "10.1000/retry-three")}, SubmitOptions{Now: now})
	if err != nil {
		t.Fatalf("Submit = %v", err)
	}
	if output.BatchID != batchID {
		t.Fatalf("BatchID = %q, want %q", output.BatchID, batchID)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].JobID != "job-fresh" {
		t.Fatalf("Submitted = %+v, want a fresh job after proven absence", output.Submitted)
	}
}

// A first run that wrote its intent and then failed before the daemon
// recorded any job, with no papio.db yet, leaves a jobless intent. The
// missing store proves nothing was committed, so the retry must submit
// instead of refusing the work forever.
func TestSubmitRetriesJoblessIntentWhenStoreMissing(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-missing-store", "10.1000/retry-missing-store")}
	if err := Write(dataDir, NewManifest(works, "", "", now)); err != nil {
		t.Fatalf("Write intent manifest = %v", err)
	}
	caller := &goneJobBatchCaller{jobID: "job-fresh"}
	output, err := Submit(context.Background(), caller, dataDir, []protocol.WorkRequest{doiWork("retry-missing-store", "10.1000/retry-missing-store")}, SubmitOptions{Now: now})
	if err != nil {
		t.Fatalf("Submit = %v, want the jobless intent submitted", err)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].JobID != "job-fresh" || output.Failed != 0 {
		t.Fatalf("output = %+v, want one fresh submission", output)
	}
	manifest, err := Load(dataDir, output.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Works[0].JobID != "job-fresh" {
		t.Fatalf("manifest = %+v, want the fresh job recorded", manifest.Works)
	}
}

// flakyBatchCaller submits every work except refuseDOI, naming each job
// "job-" plus its DOI suffix, and answers jobs.get from states — or fails
// every state read when states is nil.
type flakyBatchCaller struct {
	baseBatchCaller
	refuseDOI string
	states    map[string]string
	mu        sync.Mutex
	submitted []string
}

func (c *flakyBatchCaller) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "acquire.submit_v2":
		doi := params.(submitParams).Request.Identifiers.DOI
		if doi == c.refuseDOI {
			return errors.New("daemon refused the submission")
		}
		c.mu.Lock()
		c.submitted = append(c.submitted, doi)
		c.mu.Unlock()
		result.(*submitResult).JobID = "job-" + strings.TrimPrefix(doi, "10.1000/")
		return nil
	case "jobs.get":
		id := params.(map[string]string)["job_id"]
		state, ok := c.states[id]
		if !ok {
			return errors.New("daemon connection reset")
		}
		result.(*jobDetail).Job = json.RawMessage(`{"id":` + strconv.Quote(id) + `,"state":` + strconv.Quote(state) + `}`)
		return nil
	default:
		return c.baseBatchCaller.Call(ctx, method, params, result)
	}
}

// A state read that fails after a successful submission must not lose the
// job: the work is reported as submitted in an unknown state, the error still
// surfaces, and the manifest records the job beside a sibling whose
// submission really failed. A retry reattaches to the recorded job instead of
// submitting it again, and submits only the failed sibling.
func TestSubmitStateLookupFailureKeepsSubmittedJob(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := func() []protocol.WorkRequest {
		return []protocol.WorkRequest{
			doiWork("state-lookup", "10.1000/state-lookup"),
			doiWork("refused", "10.1000/refused"),
		}
	}
	first := &flakyBatchCaller{refuseDOI: "10.1000/refused"}
	output, err := Submit(context.Background(), first, dataDir, works(), SubmitOptions{Now: now})
	if err == nil || !strings.Contains(err.Error(), "getting state for job-state-lookup") {
		t.Fatalf("Submit err = %v, want the state lookup failure", err)
	}
	if output == nil || output.Failed != 1 || len(output.Submitted) != 1 {
		t.Fatalf("output = %+v, want one kept submission and one failure", output)
	}
	if got := output.Submitted[0]; got.JobID != "job-state-lookup" || got.State != "unknown" {
		t.Fatalf("Submitted = %+v, want job-state-lookup in state unknown", got)
	}
	manifest, err := Load(dataDir, output.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	byDOI := map[string]ManifestWork{}
	for _, work := range manifest.Works {
		byDOI[work.Work.Identifiers.DOI] = work
	}
	if kept := byDOI["10.1000/state-lookup"]; kept.JobID != "job-state-lookup" || kept.Status == "submission_failed" {
		t.Fatalf("manifest kept work = %+v, want the submitted job recorded", kept)
	}
	if refused := byDOI["10.1000/refused"]; refused.JobID != "" || refused.Status != "submission_failed" {
		t.Fatalf("manifest refused work = %+v, want submission_failed with no job", refused)
	}

	retry := &flakyBatchCaller{states: map[string]string{"job-state-lookup": "queued", "job-refused": "queued"}}
	again, err := Submit(context.Background(), retry, dataDir, works(), SubmitOptions{Now: now})
	if err != nil {
		t.Fatalf("retry Submit = %v", err)
	}
	if len(retry.submitted) != 1 || retry.submitted[0] != "10.1000/refused" {
		t.Fatalf("retry submitted %v, want only the previously refused work", retry.submitted)
	}
	if len(again.Submitted) != 2 || again.Failed != 0 {
		t.Fatalf("retry output = %+v, want both works submitted", again)
	}
}

// The commit-before-reply window: the daemon committed a job that turned
// terminal, but the killed run never learned its ID, so the manifest holds
// only a jobless intent. The retry must adopt the committed job with zero
// new submits.
func TestSubmitAdoptsStoreCommittedTerminalJob(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-four", "10.1000/retry-four")}
	batchID := ID(works, now)
	intent := NewManifest(works, "", "", now)
	if err := Write(dataDir, intent); err != nil {
		t.Fatalf("Write intent manifest = %v", err)
	}
	requestID := RequestID(batchID, works[0])
	seedJobsDB(t, dataDir, [][3]string{{"job-committed", requestID, "ready"}})
	replay := &replayBatchCaller{states: map[string]string{}}
	output, err := Submit(context.Background(), replay, dataDir, []protocol.WorkRequest{doiWork("retry-four", "10.1000/retry-four")}, SubmitOptions{Now: now})
	if err != nil {
		t.Fatalf("Submit = %v", err)
	}
	if replay.submits != 0 {
		t.Fatalf("retry submitted %d new jobs, want none", replay.submits)
	}
	if len(output.Submitted) != 1 || output.Submitted[0].JobID != "job-committed" {
		t.Fatalf("Submitted = %+v, want the committed terminal job", output.Submitted)
	}
	if output.Submitted[0].State != "ready" {
		t.Fatalf("retry state = %q, want ready", output.Submitted[0].State)
	}
}

// A dataDir that cannot persist the intent must fail before any provider
// effect: no job may exist without a durable association.
func TestSubmitIntentWriteFailureSubmitsNothing(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dataDir, 0o700); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	caller := &fingerprintBatchCaller{expectedFingerprint: ""}
	_, err := Submit(context.Background(), caller, dataDir, []protocol.WorkRequest{doiWork("retry-five", "10.1000/retry-five")}, SubmitOptions{Now: now})
	if err == nil || !strings.Contains(err.Error(), "writing batch intent") {
		t.Fatalf("Submit err = %v, want an intent write failure", err)
	}
	if caller.called("acquire.submit_v2") {
		t.Fatal("a batch that cannot persist its intent must not submit")
	}
}

// A corrupt manifest may describe submitted work. The retry must abort
// rather than overwrite it with a jobless intent and submit again.
func TestSubmitCorruptManifestAbortsBeforeSubmit(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	works := []protocol.WorkRequest{doiWork("retry-six", "10.1000/retry-six")}
	if err := os.MkdirAll(filepath.Join(dataDir, "batches"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "batches", ID(works, now)+".json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	caller := &fingerprintBatchCaller{expectedFingerprint: ""}
	_, err := Submit(context.Background(), caller, dataDir, []protocol.WorkRequest{doiWork("retry-six", "10.1000/retry-six")}, SubmitOptions{Now: now})
	if err == nil || !strings.Contains(err.Error(), "reading batch manifest") {
		t.Fatalf("Submit err = %v, want a manifest read failure", err)
	}
	if caller.called("acquire.submit_v2") {
		t.Fatal("a batch with an unreadable manifest must not submit")
	}
}
