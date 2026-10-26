package publicationwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/enrich"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

type fixtureRelations struct {
	edges []enrich.PublicationRelation
	err   error
	calls int
}

func (r *fixtureRelations) PublicationRelations(context.Context, string) ([]enrich.PublicationRelation, error) {
	r.calls++
	return r.edges, r.err
}

type fixtureSubmitter struct {
	jobs      *job.Store
	calls     int
	request   protocol.WorkRequest
	auto      bool
	lostReply bool
}

func (s *fixtureSubmitter) SubmitOnceWithAutoImport(ctx context.Context, r protocol.WorkRequest, auto *bool) (string, error) {
	s.calls++
	s.request = r
	s.auto = auto != nil && *auto
	cfg := config.Default()
	cfg.AccessMode = "conservative"
	service := &app.Service{Config: cfg, Jobs: s.jobs}
	id, err := service.SubmitOnceWithAutoImport(ctx, r, auto)
	if err != nil {
		return "", err
	}
	if s.lostReply && s.calls == 1 {
		return "", errors.New("lost reply")
	}
	return id, nil
}

func testDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// These rows are explicit fixture evidence, not a live acquisition claim.
func acquiredFixture(t *testing.T, s *store.Store, version string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	jobs := &job.Store{S: s}
	id, err := jobs.CreateRequest(ctx, "source-"+version, work.Work{DOI: "10.1000/preprint"}, "", "", job.Policy{DesiredVersion: "any"}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "annotated-original.pdf")
	data := []byte("%PDF-1.7\nfixture original, with retained annotation\n%%EOF\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	sha := hex.EncodeToString(hash[:])
	at := store.Now()
	result, err := s.DB().ExecContext(ctx, `INSERT INTO candidates(job_id,source,url_redacted,url_key,version,access_basis,reuse_license,status,created_at) VALUES(?,'fixture','https://fixture.invalid/original','fixture',?,'open_access','unknown','accepted',?)`, id, version, at)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO artifacts(sha256,size_bytes,mime,path,created_at) VALUES(?,?,'application/pdf',?,?)`, sha, len(data), path, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO job_artifacts(job_id,artifact_sha256,role,candidate_id,identity_result,created_at) VALUES(?,?,'main',?,'pass',?)`, id, sha, candidate, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO validation_reports(job_id,candidate_id,sha256,outcome,recorded_at,document) VALUES(?,?,?,'pass',?,'{}')`, id, candidate, sha, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE jobs SET state='ready',selected_candidate_id=?,artifact_sha256=? WHERE id=?`, candidate, sha, id); err != nil {
		t.Fatal(err)
	}
	return id, path, sha
}

func TestPublicationRequiresAcquiredNonPublishedEvidence(t *testing.T) {
	ctx := context.Background()
	for _, version := range []string{"published", "unknown", "preprint", "accepted"} {
		t.Run(version, func(t *testing.T) {
			db := testDB(t)
			id, _, _ := acquiredFixture(t, db, version)
			s := NewService(db, &fixtureRelations{}, nil, nil)
			_, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
			if version == "published" || version == "unknown" {
				if err == nil {
					t.Fatal("non-preprint evidence accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().ExecContext(ctx, `UPDATE job_artifacts SET identity_result='review' WHERE job_id=?`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24}); err == nil {
				t.Fatal("ambiguous acquisition accepted")
			}
		})
	}
}

func TestPublicationDirectionalNoticeRestartAndExplicitAcquisitionPreserveOriginal(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, path, sha := acquiredFixture(t, db, "preprint")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	relations := &fixtureRelations{}
	submitter := &fixtureSubmitter{jobs: &job.Store{S: db}}
	s := NewService(db, relations, submitter, nil)
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Run(ctx, w.ID)
	if err != nil || first.NewNotices != 0 {
		t.Fatalf("absent relation = %+v, %v", first, err)
	}
	relations.edges = []enrich.PublicationRelation{
		{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/published", Type: "is-preprint-of", Provider: "fixture"},
		{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/correction", Type: "is-corrected-by", Provider: "fixture"},
		{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/retraction", Type: "is-retracted-by", Provider: "fixture"},
		{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/generic", Type: "has-version", Provider: "fixture"},
	}
	second, err := s.Run(ctx, w.ID)
	if err != nil || second.NewNotices != 1 {
		t.Fatalf("confirmed relation = %+v, %v", second, err)
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil || len(notices) != 1 || notices[0].TargetDOI != "10.1000/published" {
		t.Fatalf("notices = %+v, %v", notices, err)
	}
	if submitter.calls != 0 {
		t.Fatal("notice acquired automatically")
	}
	dir := filepath.Dir(db.Path())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	submitter.jobs = &job.Store{S: reopened}
	s = NewService(reopened, relations, submitter, nil)
	third, err := s.Run(ctx, w.ID)
	if err != nil || third.NewNotices != 0 {
		t.Fatalf("restart duplicates = %+v, %v", third, err)
	}
	result, err := s.Acquire(ctx, notices[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OriginalSHA256 != sha || result.JobID == id || submitter.request.DesiredVersion != "published" || submitter.request.Identifiers.DOI != "10.1000/published" || submitter.auto || submitter.request.ZotioItemKey != "" {
		t.Fatalf("acquisition = %+v, request %+v", result, submitter.request)
	}
	if _, err := s.Acquire(ctx, notices[0].ID); err != nil || submitter.calls != 1 {
		t.Fatalf("explicit acquisition duplicated: %v, calls %d", err, submitter.calls)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("original annotations changed: %v", err)
	}
	var currentSHA string
	if err := reopened.DB().QueryRowContext(ctx, `SELECT artifact_sha256 FROM jobs WHERE id=?`, id).Scan(&currentSHA); err != nil || currentSHA != sha {
		t.Fatalf("original acquisition changed: %q, %v", currentSHA, err)
	}
}

type recordingSink struct {
	intents []notify.Intent
	err     error
}

func (r *recordingSink) Route(_ context.Context, in notify.Intent) error {
	if r.err != nil {
		return r.err
	}
	r.intents = append(r.intents, in)
	return nil
}

// A durable notice becomes exactly one notification: it is routed once under
// publication-notice:<id>, never again on later runs, never re-routed when a
// crash left a router row but no notified_at, and a Route failure fails the
// run before the watch records a successful pass.
func TestPublicationNoticeDeliveryRoutesOnceAndSurvivesCrashWindow(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	relations := &fixtureRelations{edges: []enrich.PublicationRelation{
		{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/published", Type: "is-preprint-of", Provider: "fixture"},
	}}
	sink := &recordingSink{err: errors.New("router down")}
	s := NewService(db, relations, nil, sink)
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	lastError := func() string {
		var v string
		if err := db.DB().QueryRowContext(ctx, `SELECT COALESCE(last_error,'') FROM publication_watches WHERE id=?`, w.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	notifiedAt := func(noticeID int64) string {
		var v string
		if err := db.DB().QueryRowContext(ctx, `SELECT COALESCE(notified_at,'') FROM publication_notices WHERE id=?`, noticeID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// (d) A failing router fails the run, and the watch records why.
	if _, err := s.Run(ctx, w.ID); err == nil {
		t.Fatal("Run swallowed the Route error")
	}
	if got := lastError(); !strings.Contains(got, "router down") {
		t.Fatalf("last_error = %q after a failed delivery, want the Route error", got)
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices = %+v, %v", notices, err)
	}
	noticeID := notices[0].ID
	if got := notifiedAt(noticeID); got != "" {
		t.Fatalf("notified_at = %q after a failed Route", got)
	}

	// (a) The retry routes exactly once, with the aggregate key, and stamps notified_at.
	sink.err = nil
	if _, err := s.Run(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	wantKey := fmt.Sprintf("publication-notice:%d", noticeID)
	if len(sink.intents) != 1 || sink.intents[0].AggregateKey != wantKey || sink.intents[0].EventKind != "publication.available" {
		t.Fatalf("routed intents = %+v, want one publication.available under %s", sink.intents, wantKey)
	}
	if notifiedAt(noticeID) == "" || lastError() != "" {
		t.Fatalf("notified_at %q / last_error %q after a successful delivery", notifiedAt(noticeID), lastError())
	}

	// (b) A further run does not route again.
	if _, err := s.Run(ctx, w.ID); err != nil || len(sink.intents) != 1 {
		t.Fatalf("second run routed again: %d intents, %v", len(sink.intents), err)
	}

	// (c) Crash window: the router row exists but notified_at was never
	// stamped. Delivery must re-stamp without routing a second notification.
	if _, err := db.DB().ExecContext(ctx, `UPDATE publication_notices SET notified_at=NULL WHERE id=?`, noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO notification_intents
		(category, event_kind, aggregate_key, phase, window_start, payload_json, first_at, last_at, count, available_at, desktop_state, webhook_state)
		VALUES ('integrity_notice','publication.available',?,'scan','2026-08-01T00:00:00Z','{}','2026-08-01T00:00:00Z','2026-08-01T00:00:00Z',1,'2026-08-01T00:00:00Z','pending','pending')`, wantKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("crash-window run re-routed: %d intents, want 1", len(sink.intents))
	}
	if notifiedAt(noticeID) == "" {
		t.Fatal("crash-window run did not re-stamp notified_at")
	}
}

func TestPublicationRejectsWrongDirectionalProofBeforeAnyNotices(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "accepted")
	relations := &fixtureRelations{edges: []enrich.PublicationRelation{{SourceDOI: "10.1000/unrelated", TargetDOI: "10.1000/preprint", Type: "is-preprint-of", Provider: "fixture"}}}
	s := NewService(db, relations, nil, nil)
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, w.ID); err == nil {
		t.Fatal("wrong direction accepted")
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("invalid proof persisted notices: %+v, %v", notices, err)
	}
}

func TestPublicationAcquisitionRecoversLostReplyAfterTerminalJob(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	relations := &fixtureRelations{edges: []enrich.PublicationRelation{{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/published", Type: "is-preprint-of", Provider: "fixture"}}}
	submitter := &fixtureSubmitter{jobs: &job.Store{S: db}, lostReply: true}
	s := NewService(db, relations, submitter, nil)
	cfg := config.Default()
	cfg.AccessMode = "conservative"
	service := &app.Service{Config: cfg, Jobs: submitter.jobs}
	auto := false
	existing, err := service.SubmitWithAutoImport(ctx, protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "other-request", Identifiers: &protocol.Identifiers{DOI: "10.1000/published"}, DesiredVersion: "published"}, &auto)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, notices[0].ID); err == nil {
		t.Fatal("lost reply must fail")
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET state='failed' WHERE id=?`, existing); err != nil {
		t.Fatal(err)
	}
	result, err := s.Acquire(ctx, notices[0].ID)
	if err != nil || result.JobID != existing || submitter.calls != 2 {
		t.Fatalf("converged lost reply recovery = %+v, %v, calls %d", result, err, submitter.calls)
	}
	var count int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j JOIN identifiers i ON i.work_request_id=j.work_request_id WHERE i.kind='doi' AND i.value='10.1000/published'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("once-only target jobs = %d, %v", count, err)
	}
}

func TestPausedPublicationWatchKeepsNoticeHistory(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	r := &fixtureRelations{edges: []enrich.PublicationRelation{{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/published", Type: "is-preprint-of", Provider: "fixture"}}}
	s := NewService(db, r, nil, nil)
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	r.edges = append(r.edges, enrich.PublicationRelation{SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/other", Type: "is-preprint-of", Provider: "fixture"})
	if err := s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil || len(notices) != 1 {
		t.Fatalf("pause lost history or ran: %+v, %v", notices, err)
	}
}

func TestPublicationImportedEvidenceAndUnavailableSourceGate(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET state='imported' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(db, nil, nil, nil).Add(ctx, AddInput{JobID: id, CadenceHours: 24}); err == nil {
		t.Fatal("opt-in without typed source succeeded")
	}
	s := NewService(db, &fixtureRelations{}, nil, nil)
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil || w.JobID != id {
		t.Fatalf("imported preprint opt-in = %+v, %v", w, err)
	}
}

func TestPublicationFailedScanKeepsCadenceAndErrorAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	relations := &fixtureRelations{err: errors.New("fixture provider unavailable")}
	s := NewService(db, relations, nil, nil)
	s.Now = func() time.Time { return at }
	w, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunDue(ctx); err == nil {
		t.Fatal("failed scan did not report failure")
	}
	watches, err := s.List(ctx)
	if err != nil || len(watches) != 1 || watches[0].LastRunAt != store.FormatTime(at) || watches[0].LastError == "" {
		t.Fatalf("failed attempt history = %+v, %v", watches, err)
	}
	restarted := NewService(db, relations, nil, nil)
	restarted.Now = func() time.Time { return at.Add(time.Minute) }
	if err := restarted.RunDue(ctx); err != nil || relations.calls != 1 {
		t.Fatalf("failure retried before cadence: %v, calls %d", err, relations.calls)
	}
	restarted.Now = func() time.Time { return at.Add(24 * time.Hour) }
	if err := restarted.RunDue(ctx); err == nil || relations.calls != 2 {
		t.Fatalf("cadence retry missing: %v, calls %d", err, relations.calls)
	}
	notices, err := s.Notices(ctx, w.ID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("failed scan created notice: %+v, %v", notices, err)
	}
}

func TestPublicationRunDueAfterClockRollback(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	relations := &fixtureRelations{}
	s := NewService(db, relations, nil, nil)
	s.Now = func() time.Time { return now.Add(30 * 24 * time.Hour) }
	watched, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, watched.ID); err != nil {
		t.Fatal(err)
	}
	relations.edges = []enrich.PublicationRelation{{
		SourceDOI: "10.1000/preprint", TargetDOI: "10.1000/published", Type: "is-preprint-of", Provider: "fixture",
	}}
	s.Now = func() time.Time { return now }
	if err := s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	notices, err := s.Notices(ctx, watched.ID)
	if err != nil || len(notices) != 1 || notices[0].TargetDOI != "10.1000/published" {
		t.Fatalf("notices after clock rollback = %+v, %v, want the newly published work", notices, err)
	}
	stored, err := s.List(ctx)
	if err != nil || len(stored) != 1 || stored[0].LastRunAt != store.FormatTime(now) {
		t.Fatalf("watch after clock rollback = %+v, %v, want the corrected run time", stored, err)
	}
}
