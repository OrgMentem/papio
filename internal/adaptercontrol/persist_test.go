// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package adaptercontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/adaptercontrol/controltest"
)

func signedForPersist(t *testing.T, sequence int64, revisions []string) []byte {
	t.Helper()
	_, private := controltest.PlaceholderKey()
	envelope, err := Sign(Document{
		Sequence: sequence, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "papio-control-test", SuspendedRevisions: revisions,
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func openForPersist(t *testing.T, dir string) *Service {
	t.Helper()
	public, _ := controltest.PlaceholderKey()
	return Open(dir, public)
}

// failDirectorySync makes every directory flush fail until the test ends or
// restore is called, and returns restore.
func failDirectorySync(t *testing.T, err error) (restore func()) {
	t.Helper()
	original := syncDir
	syncDir = func(string) error { return err }
	restored := false
	restore = func() {
		if !restored {
			syncDir, restored = original, true
		}
	}
	t.Cleanup(restore)
	return restore
}

// TestWriteFileAtomicFlushesTheDirectoryAfterTheRename pins papio-00bf12dbef3ac703:
// the replacement is only durable once the directory entry is flushed, so the
// flush runs after the rename and its failure is the caller's error.
func TestWriteFileAtomicFlushesTheDirectoryAfterTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DocumentFileName)
	original := syncDir
	t.Cleanup(func() { syncDir = original })
	var flushed []string
	syncDir = func(flushedDir string) error {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "new" {
			t.Errorf("directory flushed before the rename landed: %q, %v", got, err)
		}
		flushed = append(flushed, flushedDir)
		return nil
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if len(flushed) != 1 || flushed[0] != dir {
		t.Fatalf("flushed directories = %v, want exactly %q", flushed, dir)
	}

	syncErr := errors.New("flush refused")
	syncDir = func(string) error { return syncErr }
	if err := writeFileAtomic(path, []byte("newer")); !errors.Is(err, syncErr) {
		t.Fatalf("writeFileAtomic = %v, want the directory flush error", err)
	}
}

// TestApplyDoesNotPublishControlItCannotMakeDurable pins the other half: a
// crash after Apply returned success must not drop a control the daemon
// already treated as canonical.
func TestApplyDoesNotPublishControlItCannotMakeDurable(t *testing.T) {
	dir := t.TempDir()
	service := openForPersist(t, dir)
	restore := failDirectorySync(t, errors.New("flush refused"))

	applied, err := service.Apply(signedForPersist(t, 3, []string{"wiley@0.1.0"}))
	if err == nil || !strings.Contains(err.Error(), "persisting adapter control") {
		t.Fatalf("Apply = %+v, %v; want a persisting error", applied, err)
	}
	if applied.Changed {
		t.Fatal("Apply reported a publication it could not make durable")
	}
	if got := service.Snapshot(); got.State != StateNone || got.RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("snapshot after a failed persist = %+v; want the held control untouched", got)
	}

	// Once the directory can be flushed the same document publishes.
	restore()
	if applied, err := service.Apply(signedForPersist(t, 3, []string{"wiley@0.1.0"})); err != nil || !applied.Changed {
		t.Fatalf("retry = %+v, %v; want published", applied, err)
	}
	if got := service.Snapshot(); got.State != StateCurrent || !got.RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("snapshot after the retry = %+v", got)
	}
}

// TestRunDueRetriesAnUnpersistedControlOnTheRecoveryCadence: a control that
// could not be made durable is not held, so the next fetch comes within the
// recovery delay instead of a day later.
func TestRunDueRetriesAnUnpersistedControlOnTheRecoveryCadence(t *testing.T) {
	body := signedForPersist(t, 9, []string{"wiley@0.1.0"})
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	service := openForPersist(t, dir)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	runner := &Runner{Service: service, URL: server.URL, Now: func() time.Time { return now }}
	restore := failDirectorySync(t, errors.New("flush refused"))

	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := service.Status().LastOutcome; got != OutcomePersistFailed {
		t.Fatalf("outcome = %q, want %q", got, OutcomePersistFailed)
	}
	restore()
	now = now.Add(retryEvery + time.Minute)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want a retry within the recovery cadence", hits.Load())
	}
	if got := service.Status().LastOutcome; got != OutcomePublished || !service.Snapshot().RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("after the retry: outcome %q, snapshot %+v", got, service.Snapshot())
	}
}

// TestRunDueIsNotAnErrorWhenItsStatusCannotBeWritten pins what the RunDue
// comment promises (papio-5c220a2694b8839b): the outcome is recorded on a
// best-effort basis, a failed write never fails the pass, and the next pass
// fetches again because no attempt was recorded.
func TestRunDueIsNotAnErrorWhenItsStatusCannotBeWritten(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	// A directory where the status file belongs makes every status write fail.
	if err := os.Mkdir(filepath.Join(dir, StatusFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	runner := &Runner{Service: openForPersist(t, dir), URL: server.URL, Now: func() time.Time { return now }}
	for i := range 2 {
		if err := runner.RunDue(context.Background()); err != nil {
			t.Fatalf("RunDue %d = %v; a status write failure is not a maintenance error", i, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2: an unrecorded attempt is retried by the next pass", hits.Load())
	}
	if got := runner.Service.Status(); got != (Status{}) {
		t.Fatalf("status = %+v, want none recorded", got)
	}
}

// TestRefusedControlDoesNotSurviveARestart: a document whose directory entry
// could not be flushed is refused, and the file it replaced must be the one
// Open reads afterwards. Leaving the refused document at the path would let a
// later successful flush, or a restart after one, load a control the daemon
// never accepted, lifting a suspension it still holds.
func TestRefusedControlDoesNotSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DocumentFileName)
	service := openForPersist(t, dir)
	held := signedForPersist(t, 3, []string{"wiley@0.1.0"})
	if applied, err := service.Apply(held); err != nil || !applied.Changed {
		t.Fatalf("first apply = %+v, %v", applied, err)
	}

	restore := failDirectorySync(t, errors.New("flush refused"))
	if _, err := service.Apply(signedForPersist(t, 4, nil)); err == nil {
		t.Fatal("Apply accepted a lifting document it could not make durable")
	}
	restore()

	if got := service.Snapshot(); got.Sequence != 3 || !got.RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("held control after the refusal = %+v; want sequence 3 still suspending", got)
	}
	if onDisk, err := os.ReadFile(path); err != nil || string(onDisk) != string(held) {
		t.Fatalf("file after the refusal = %q, %v; want the control held before", onDisk, err)
	}
	reopened := openForPersist(t, dir).Snapshot()
	if reopened.State != StateCurrent || reopened.Sequence != 3 || !reopened.RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("after a restart = %+v; want the control held before the refusal", reopened)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

// With no control held, a refused first document must leave no file at all, so
// a restart reads StateNone rather than a document that was never accepted.
func TestRefusedFirstControlLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	service := openForPersist(t, dir)
	restore := failDirectorySync(t, errors.New("flush refused"))
	if _, err := service.Apply(signedForPersist(t, 1, []string{"wiley@0.1.0"})); err == nil {
		t.Fatal("Apply accepted a document it could not make durable")
	}
	restore()
	if _, err := os.Stat(filepath.Join(dir, DocumentFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control file after the refusal: %v; want none", err)
	}
	if got := openForPersist(t, dir).Snapshot(); got.State != StateNone {
		t.Fatalf("after a restart = %+v; want state none", got)
	}
}

// A damaged copy stays as it was: the service reported it unavailable, and a
// refused repair must not turn it into a document nobody accepted.
func TestRefusedRepairKeepsTheDamagedCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DocumentFileName)
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := openForPersist(t, dir)
	if got := service.Snapshot(); got.State != StateUnavailable {
		t.Fatalf("damaged control = %+v", got)
	}
	restore := failDirectorySync(t, errors.New("flush refused"))
	if _, err := service.Apply(signedForPersist(t, 2, nil)); err == nil {
		t.Fatal("Apply accepted a repair it could not make durable")
	}
	restore()
	if onDisk, err := os.ReadFile(path); err != nil || string(onDisk) != "garbage" {
		t.Fatalf("file after the refused repair = %q, %v; want the damaged copy", onDisk, err)
	}
	if got := openForPersist(t, dir).Snapshot(); got.State != StateUnavailable {
		t.Fatalf("after a restart = %+v; want still unavailable", got)
	}
}

// A control file that exists but cannot be read cannot be restored if its
// replacement turns out not to be durable, so the replacement does not start.
func TestApplyRefusesToReplaceAControlFileItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file this user cannot read")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, DocumentFileName)
	if err := os.WriteFile(path, []byte("garbage"), 0o000); err != nil {
		t.Fatal(err)
	}
	service := openForPersist(t, dir)
	if got := service.Snapshot(); got.State != StateUnavailable {
		t.Fatalf("unreadable control = %+v; want unavailable", got)
	}
	applied, err := service.Apply(signedForPersist(t, 2, nil))
	if err == nil || !strings.Contains(err.Error(), "cannot be read") || applied.Changed {
		t.Fatalf("Apply = %+v, %v; want a refusal that names the unreadable file", applied, err)
	}
	if got := service.Snapshot(); got.State != StateUnavailable {
		t.Fatalf("snapshot after the refusal = %+v; want unchanged", got)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Mode().Perm() != 0 {
		t.Fatalf("control file after the refusal: %v, %v; want the same unreadable file", info, statErr)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
	// Once the file can be read again the repair goes through.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if applied, err := service.Apply(signedForPersist(t, 2, nil)); err != nil || !applied.Changed {
		t.Fatalf("Apply after the file is readable = %+v, %v", applied, err)
	}
}
