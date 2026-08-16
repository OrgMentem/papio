package grab

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestAllocateReturnsExistingNonterminalGrab(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	first, err := svc.Allocate(ctx, "pdf.example.org", "A paper")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(ctx, "pdf.example.org", "A paper")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second allocation id = %q, want existing %q", second.ID, first.ID)
	}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestAllocateEffectSteering(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	prepareCalled := false
	prepareGrabID := ""
	g, err := svc.AllocateEffect(ctx, "pdf.example.org", "A Paper", 42, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), func(grabID string) error {
		prepareCalled = true
		prepareGrabID = grabID
		return nil
	})
	if err != nil {
		t.Fatalf("AllocateEffect: %v", err)
	}
	if g == nil || g.Outcome != "steering" {
		t.Fatalf("AllocateEffect outcome = %q, want steering", g.Outcome)
	}
	if !prepareCalled || prepareGrabID != g.ID {
		t.Fatalf("prepare called=%v grabID=%q want %q", prepareCalled, prepareGrabID, g.ID)
	}
	if g.State != StateAwaitingFile {
		t.Fatalf("grab state = %q, want awaiting_file", g.State)
	}
	db := s.DB()
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 1 {
		t.Fatalf("pdf_grabs count = %d, want 1", n)
	}
	var jobID sql.NullString
	var attempt int64
	var holder int64
	var domain, kind, status, grabID string
	if err := db.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind, status, grab_id FROM effect_permits WHERE grab_id = ?`, g.ID).Scan(&jobID, &attempt, &holder, &domain, &kind, &status, &grabID); err != nil {
		t.Fatalf("permit lookup: %v", err)
	}
	if jobID.Valid {
		t.Fatalf("permit job_id = %q, want NULL", jobID.String)
	}
	if attempt != 0 || holder != 42 || domain != "pdf_grab:pdf.example.org" || kind != "pdf_grab" || status != "held" || grabID != g.ID {
		t.Fatalf("permit fields = jobValid %v attempt %d holder %d domain %q kind %q status %q grabID %q", jobID.Valid, attempt, holder, domain, kind, status, grabID)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started' AND job_id IS NULL`); n != 1 {
		t.Fatalf("browser.pdf_grab_started events = %d, want 1", n)
	}
}

func TestAllocateEffectPrepareErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	_, err = svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 1, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), func(string) error {
		return errors.New("prepare failed")
	})
	if err == nil || err.Error() != "prepare failed" {
		t.Fatalf("AllocateEffect err = %v, want prepare failed", err)
	}
	db := s.DB()
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 0 {
		t.Fatalf("pdf_grabs after rollback = %d, want 0", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM effect_permits`); n != 0 {
		t.Fatalf("effect_permits after rollback = %d, want 0", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started'`); n != 0 {
		t.Fatalf("events after rollback = %d, want 0", n)
	}
}

func TestAllocateEffectBusyLeavesZero(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	// Occupy global lane via held permit (jobless pdf) to trigger ErrBusy.
	now := store.Now()
	lease := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_permits(id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind, slot_index, grab_id, status, lease_until, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"permit-busy-001", nil, 0, 1, "pdf_grab:other.example.org", "pdf_grab", 0, "grab-busy-001", "held", lease, now, now); err != nil {
		t.Fatalf("insert busy permit: %v", err)
	}
	svc := New(s, nil)
	prepareCalled := false
	_, err = svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 2, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), func(string) error {
		prepareCalled = true
		return nil
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("AllocateEffect err = %v, want ErrBusy", err)
	}
	if prepareCalled {
		t.Fatal("prepare must not be called when busy")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 0 {
		t.Fatalf("pdf_grabs after busy = %d, want 0", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM effect_permits`); n != 1 {
		t.Fatalf("effect_permits after busy = %d, want 1 (only blocker)", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started'`); n != 0 {
		t.Fatalf("events after busy = %d, want 0", n)
	}
}

func TestAllocateEffectLegacyBlockerBusy(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	now := store.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`,
		"request-blocker-001", now); err != nil {
		t.Fatalf("insert blocker request: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		"job_00000000000000000000000001", "request-blocker-001", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatalf("insert blocker job: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO legacy_effect_blockers(id, effect_kind, job_id, safety_domain_id, grab_id, cleanup_only, status, created_at, updated_at) VALUES(?, 'pdf_grab', NULL, ?, ?, 1, 'unresolved', ?, ?)`,
		"blocker-001", "pdf_grab:pdf.example.org", "grab_legacy_blocker_001", now, now); err != nil {
		t.Fatalf("insert legacy blocker: %v", err)
	}
	svc := New(s, nil)
	_, err = svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 1, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("AllocateEffect err = %v, want ErrBusy due to legacy blocker", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 0 {
		t.Fatalf("pdf_grabs after legacy busy = %d, want 0", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM effect_permits`); n != 0 {
		t.Fatalf("effect_permits after legacy busy = %d, want 0", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started'`); n != 0 {
		t.Fatalf("events after legacy busy = %d, want 0", n)
	}
}

func TestAllocateEffectExistingNoPrepare(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	first, err := svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 1, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), func(string) error { return nil })
	if err != nil {
		t.Fatalf("first AllocateEffect: %v", err)
	}
	calls := 0
	second, err := svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 1, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), func(string) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("second AllocateEffect: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second id = %q, want existing %q", second.ID, first.ID)
	}
	if second.Outcome != "existing" {
		t.Fatalf("second outcome = %q, want existing", second.Outcome)
	}
	if calls != 0 {
		t.Fatalf("prepare called %d times for existing, want 0", calls)
	}
	db := s.DB()
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 1 {
		t.Fatalf("pdf_grabs after existing = %d, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM effect_permits`); n != 1 {
		t.Fatalf("effect_permits after existing = %d, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started'`); n != 1 {
		t.Fatalf("events after existing = %d, want 1", n)
	}
}

func TestMarkQuarantinedSettlesPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 5, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("AllocateEffect: %v", err)
	}
	if err := svc.MarkQuarantined(ctx, g.ID, "/tmp/quarantine/"+g.ID); err != nil {
		t.Fatalf("MarkQuarantined: %v", err)
	}
	got, err := svc.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after quarantine: %v %v", got, err)
	}
	if got.State != StateQuarantined {
		t.Fatalf("state = %q, want quarantined", got.State)
	}
	db := s.DB()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatalf("permit lookup: %v", err)
	}
	if status != "settled" {
		t.Fatalf("permit status = %q, want settled", status)
	}
}

func TestMarkFailedValidationSettlesPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 6, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("AllocateEffect: %v", err)
	}
	if err := svc.MarkFailedValidation(ctx, g.ID, "not a PDF"); err != nil {
		t.Fatalf("MarkFailedValidation: %v", err)
	}
	got, err := svc.Get(ctx, g.ID)
	if err != nil || got == nil || got.State != StateFailedValidation {
		t.Fatalf("grab after failed validation: %+v err=%v", got, err)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "settled" {
		t.Fatalf("permit status = %q, want settled", status)
	}
}

func TestMarkQuarantinedLegacyNoPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.Allocate(ctx, "pdf.example.org", "Legacy")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := svc.MarkQuarantined(ctx, g.ID, "/tmp/q"); err != nil {
		t.Fatalf("MarkQuarantined legacy: %v", err)
	}
	got, _ := svc.Get(ctx, g.ID)
	if got.State != StateQuarantined {
		t.Fatalf("state = %q, want quarantined", got.State)
	}
}

func TestMarkAbandonedSettlesPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.AllocateEffect(ctx, "pdf.example.org", "Paper", 7, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("AllocateEffect: %v", err)
	}
	if err := svc.MarkAbandoned(ctx, g.ID, "interrupted"); err != nil {
		t.Fatalf("MarkAbandoned: %v", err)
	}
	got, _ := svc.Get(ctx, g.ID)
	if got.State != StateAbandoned {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatalf("permit lookup: %v", err)
	}
	if status != "settled" {
		t.Fatalf("permit status = %q, want settled", status)
	}
}

func TestMarkAbandonedLegacyNoPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.Allocate(ctx, "pdf.example.org", "LegacyAbandon")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := svc.MarkAbandoned(ctx, g.ID, "gone"); err != nil {
		t.Fatalf("MarkAbandoned legacy: %v", err)
	}
	got, _ := svc.Get(ctx, g.ID)
	if got.State != StateAbandoned {
		t.Fatalf("state = %q, want abandoned", got.State)
	}
}

func TestAbandonStaleAwaitingRetainsOccupying(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	// Occupying grab with held permit.
	occupying, err := svc.AllocateEffect(ctx, "pdf.example.org", "Occupying", 1, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("AllocateEffect occupying: %v", err)
	}
	// Stale non-occupying grab (no permit) inserted with old updated_at.
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	db := s.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		"grab_stale_00000000000001", "pdf.example.org", "Stale", string(StateAwaitingFile), old, old); err != nil {
		t.Fatalf("insert stale grab: %v", err)
	}
	// Make occupying grab also stale by bumping its updated_at to old.
	if _, err := db.ExecContext(ctx, `UPDATE pdf_grabs SET updated_at=? WHERE id=?`, old, occupying.ID); err != nil {
		t.Fatalf("bump occupying updated_at: %v", err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	if err := svc.AbandonStaleAwaiting(ctx, cutoff); err != nil {
		t.Fatalf("AbandonStaleAwaiting: %v", err)
	}
	stale, _ := svc.Get(ctx, "grab_stale_00000000000001")
	if stale == nil || stale.State != StateAbandoned {
		t.Fatalf("stale grab state = %v, want abandoned", stale)
	}
	retained, _ := svc.Get(ctx, occupying.ID)
	if retained == nil || retained.State != StateAwaitingFile {
		t.Fatalf("occupying grab state = %v, want retained awaiting_file", retained)
	}
}
func TestAbandonStaleAwaitingLeavesLegacyBlockerBusyUntilExactEvidence(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	const grabID = "grab-stale-legacy-blocked"
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES (?, 'pdf.example.org', 'Stale legacy', 'awaiting_file', ?, ?)`,
		grabID, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, grab_id, cleanup_only, status, created_at, updated_at)
		VALUES ('blocker-stale-legacy', 'pdf_grab', NULL, 'pdf_grab:pdf.example.org', ?, 1, 'unresolved', ?, ?)`,
		grabID, now, now); err != nil {
		t.Fatal(err)
	}

	if err := svc.AbandonStaleAwaiting(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.Get(ctx, grabID)
	if err != nil {
		t.Fatal(err)
	}
	if stale == nil || stale.State != StateAwaitingFile {
		t.Fatalf("stale legacy grab = %+v, want unresolved awaiting_file", stale)
	}
	if _, err := svc.AllocateEffect(ctx, "other.example.org", "Blocked", 1,
		"pdf_grab:other.example.org", time.Now().Add(time.Hour), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("allocation while stale legacy blocker remains = %v, want ErrBusy", err)
	}

	if err := svc.MarkQuarantined(ctx, grabID, "/tmp/late.pdf"); err != nil {
		t.Fatal(err)
	}
	settled, err := svc.Get(ctx, grabID)
	if err != nil {
		t.Fatal(err)
	}
	if settled == nil || settled.State != StateQuarantined {
		t.Fatalf("exact late file grab = %+v, want quarantined", settled)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT status FROM legacy_effect_blockers WHERE grab_id=?`, grabID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "settled" {
		t.Fatalf("legacy blocker after exact late file = %q, want settled", status)
	}
}
func TestAllocateEffectExactRequestReplayRequiresHeldGeneration(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	first, err := svc.AllocateEffect(ctx, "pdf.example.org", "Replay", 7, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil, "pdf-request-001")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := svc.AllocateEffect(ctx, "pdf.example.org", "Replay", 7, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil, "pdf-request-001")
	if err != nil || replay == nil || replay.ID != first.ID || replay.Outcome != "steering" {
		t.Fatalf("exact replay = %+v err=%v, want same steering grant", replay, err)
	}
	replaced, err := svc.AllocateEffect(ctx, "pdf.example.org", "Replay", 8, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil, "pdf-request-001")
	if err != nil || replaced == nil || replaced.ID != first.ID || replaced.Outcome != "existing" {
		t.Fatalf("holder-replaced replay = %+v err=%v, want non-authorizing existing", replaced, err)
	}
	if first.EffectRequestID != "pdf-request-001" {
		t.Fatalf("effect request id = %q, want persisted request identity", first.EffectRequestID)
	}
}

func TestMarkAbandonedForRequestFencesKnownGrabID(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.DB().SetMaxOpenConns(1)
	svc := New(s, nil)
	g, err := svc.AllocateEffect(ctx, "pdf.example.org", "Known", 11, "pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil, "pdf-request-002")
	if err != nil {
		t.Fatal(err)
	}
	if g == nil || g.State != StateAwaitingFile {
		t.Fatalf("known grab = %+v, want awaiting_file", g)
	}

	// Keep the unrelated grab separate from the effect-backed grab above.
	unrelated, err := svc.Allocate(ctx, "other.example.org", "Unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if unrelated == nil || unrelated.State != StateAwaitingFile {
		t.Fatalf("unrelated grab = %+v, want awaiting_file", unrelated)
	}
	if err := svc.MarkAbandonedForRequest(ctx, g.ID, "foreign-request", 11, "interrupted"); err == nil {
		t.Fatal("foreign request abandoned known grab")
	}
	// Consume and close the read before starting the next transaction. With a
	// single SQLite connection, leaving rows open here would make BeginTx wait
	// forever even though the fenced request must simply leave occupancy held.
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "held" {
		t.Fatalf("foreign abandon permit status = %q, want held", status)
	}
	live, err := svc.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live == nil || live.State != StateAwaitingFile {
		t.Fatalf("foreign abandon grab = %+v, want awaiting_file", live)
	}
	if err := svc.MarkAbandonedForRequest(ctx, g.ID, "pdf-request-002", 11, "interrupted"); err != nil {
		t.Fatal(err)
	}
	abandoned, err := svc.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if abandoned == nil || abandoned.State != StateAbandoned {
		t.Fatalf("exact request grab = %+v, want abandoned", abandoned)
	}
	unchanged, err := svc.Get(ctx, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged == nil || unchanged.State != StateAwaitingFile {
		t.Fatalf("unrelated grab after abandonment = %+v, want awaiting_file", unchanged)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "settled" {
		t.Fatalf("exact request permit status = %q, want settled", status)
	}
}
func TestAllocateEffectSettledLegacyPDFTombstoneAllowsFreshRequest(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := store.Now()
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES ('grab-legacy-settled', 'pdf.example.org', 'Historical PDF', 'failed_validation', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, grab_id, cleanup_only,
		   status, created_at, updated_at)
		VALUES ('legacy-settled-pdf', 'pdf_grab', NULL, 'pdf_grab:pdf.example.org',
		        'grab-legacy-settled', 1, 'settled', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	svc := New(s, nil)
	fresh, err := svc.AllocateEffect(ctx, "pdf.example.org", "Historical PDF", 7,
		"pdf_grab:pdf.example.org", time.Now().Add(time.Hour), nil, "fresh-pdf-request")
	if err != nil || fresh == nil || fresh.ID == "grab-legacy-settled" {
		t.Fatalf("fresh PDF after settled tombstone = %+v err=%v", fresh, err)
	}
	var oldPermits, oldEvents, totalEvents int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_permits WHERE grab_id='grab-legacy-settled'`).Scan(&oldPermits); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind='browser.pdf_grab_started' AND job_id IS NULL`).Scan(&totalEvents); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_permits WHERE grab_id=?`, fresh.ID).Scan(&oldEvents); err != nil {
		t.Fatal(err)
	}
	if oldPermits != 0 || oldEvents != 1 || totalEvents != 1 {
		t.Fatalf("settled PDF tombstone old_permits=%d fresh_permits=%d started_events=%d, want 0/1/1", oldPermits, oldEvents, totalEvents)
	}
}
func TestLegacyPDFGrabTransitionsSettleExactBlocker(t *testing.T) {
	ctx := context.Background()
	const (
		targetID = "grab-legacy-transition-target"
		wrongID  = "grab-legacy-transition-wrong"
		jobID    = "job_00000000000000000000000002"
	)
	type testCase struct {
		name  string
		apply func(context.Context, *Service, string) error
		want  State
		seed  func(*sql.DB, string) error
	}
	cases := []testCase{
		{
			name: "quarantined",
			apply: func(ctx context.Context, svc *Service, id string) error {
				return svc.MarkQuarantined(ctx, id, "/tmp/"+id)
			},
			want: StateQuarantined,
		},
		{
			name: "job_created",
			apply: func(ctx context.Context, svc *Service, id string) error {
				return svc.MarkJobCreated(ctx, id, jobID, "job_created")
			},
			want: StateJobCreated,
			seed: func(db *sql.DB, now string) error {
				if _, err := db.Exec(`INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "request-legacy-transition", now); err != nil {
					return err
				}
				_, err := db.Exec(`INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
					jobID, "request-legacy-transition", "awaiting_human", `{}`, now, now)
				return err
			},
		},
		{
			name: "failed_validation",
			apply: func(ctx context.Context, svc *Service, id string) error {
				return svc.MarkFailedValidation(ctx, id, "not a PDF")
			},
			want: StateFailedValidation,
		},
		{
			name: "parked_no_identifier",
			apply: func(ctx context.Context, svc *Service, id string) error {
				return svc.MarkParkedNoIdentifier(ctx, id)
			},
			want: StateParkedNoIdentifier,
		},
		{
			name: "abandoned",
			apply: func(ctx context.Context, svc *Service, id string) error {
				return svc.MarkAbandoned(ctx, id, "interrupted")
			},
			want: StateAbandoned,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(ctx, storetest.DataDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			now := store.Now()
			for _, id := range []string{targetID, wrongID} {
				if _, err := s.DB().ExecContext(ctx, `
					INSERT INTO pdf_grabs(id,url_host,title,state,created_at,updated_at)
					VALUES(?,?,?,'awaiting_file',?,?)`,
					id, "legacy.example.org", id, now, now); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().ExecContext(ctx, `
					INSERT INTO legacy_effect_blockers
					  (id,effect_kind,job_id,safety_domain_id,grab_id,cleanup_only,status,created_at,updated_at)
					VALUES(?, 'pdf_grab', NULL, ?, ?, 1, 'unresolved', ?, ?)`,
					"blocker-"+id, "pdf_grab:legacy.example.org", id, now, now); err != nil {
					t.Fatal(err)
				}
			}
			if tc.seed != nil {
				if err := tc.seed(s.DB(), now); err != nil {
					t.Fatal(err)
				}
			}
			svc := New(s, nil)
			if err := tc.apply(ctx, svc, targetID); err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(ctx, targetID)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.State != tc.want {
				t.Fatalf("target grab = %+v, want state %q", got, tc.want)
			}
			var targetStatus, wrongStatus string
			if err := s.DB().QueryRowContext(ctx,
				`SELECT status FROM legacy_effect_blockers WHERE grab_id=?`, targetID).Scan(&targetStatus); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().QueryRowContext(ctx,
				`SELECT status FROM legacy_effect_blockers WHERE grab_id=?`, wrongID).Scan(&wrongStatus); err != nil {
				t.Fatal(err)
			}
			if targetStatus != "settled" {
				t.Fatalf("target blocker status = %q, want settled", targetStatus)
			}
			if wrongStatus != "unresolved" {
				t.Fatalf("wrong blocker status = %q, want unresolved", wrongStatus)
			}

			// The unrelated unresolved blocker must still enforce global
			// admission; once that exact row is independently settled, a fresh
			// allocation is admitted.
			if _, err := svc.AllocateEffect(ctx, "fresh.example.org", "Fresh", 1,
				"pdf_grab:fresh.example.org", time.Now().Add(time.Hour), nil); !errors.Is(err, ErrBusy) {
				t.Fatalf("fresh allocation with wrong blocker err=%v, want ErrBusy", err)
			}
			if _, err := s.DB().ExecContext(ctx,
				`UPDATE legacy_effect_blockers SET status='settled', updated_at=? WHERE effect_kind='pdf_grab' AND grab_id=? AND status='unresolved'`,
				store.Now(), wrongID); err != nil {
				t.Fatal(err)
			}
			fresh, err := svc.AllocateEffect(ctx, "fresh.example.org", "Fresh", 1,
				"pdf_grab:fresh.example.org", time.Now().Add(time.Hour), nil)
			if err != nil {
				t.Fatalf("fresh allocation after exact blocker cleanup: %v", err)
			}
			if fresh == nil || fresh.ID == targetID || fresh.ID == wrongID {
				t.Fatalf("fresh allocation = %+v, want a new grab", fresh)
			}
		})
	}
}

func TestByJobID(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000099"

	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "request-by-job-id", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "request-by-job-id", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}

	g, err := svc.Allocate(ctx, "pdf.example.org", "ByJobID paper")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkJobCreated(ctx, g.ID, jobID, "job_created"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ByJobID(ctx, jobID)
	if err != nil {
		t.Fatalf("ByJobID hit: %v", err)
	}
	if got == nil {
		t.Fatal("ByJobID hit: got nil grab")
	}
	if got.ID != g.ID {
		t.Fatalf("ByJobID id = %q, want %q", got.ID, g.ID)
	}
	if got.JobID != jobID {
		t.Fatalf("ByJobID job_id = %q, want %q", got.JobID, jobID)
	}
	if got.State != StateJobCreated {
		t.Fatalf("ByJobID state = %q, want %q", got.State, StateJobCreated)
	}

	miss, err := svc.ByJobID(ctx, "job_00000000000000000000000000")
	if err != nil {
		t.Fatalf("ByJobID miss: %v", err)
	}
	if miss != nil {
		t.Fatalf("ByJobID miss: got %+v, want (nil, nil)", miss)
	}

	empty, err := svc.ByJobID(ctx, "")
	if err != nil {
		t.Fatalf("ByJobID empty job id: %v", err)
	}
	if empty != nil {
		t.Fatalf("ByJobID empty job id: got %+v, want (nil, nil)", empty)
	}

	// ByJobID orders by created_at DESC and returns one row; pin the newest match.
	const dupJobID = "job_00000000000000000000000098"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "request-dup-job-id", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		dupJobID, "request-dup-job-id", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	newer := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	const olderGrabID = "grab_by_job_id_older01"
	const newerGrabID = "grab_by_job_id_newer01"
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, job_id, outcome, created_at, updated_at)
		VALUES (?, 'pdf.example.org', 'older', 'job_created', ?, 'job_created', ?, ?)`,
		olderGrabID, dupJobID, older, older); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, job_id, outcome, created_at, updated_at)
		VALUES (?, 'pdf.example.org', 'newer', 'job_created', ?, 'job_created', ?, ?)`,
		newerGrabID, dupJobID, newer, newer); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.ByJobID(ctx, dupJobID)
	if err != nil {
		t.Fatalf("ByJobID duplicate job_id: %v", err)
	}
	if latest == nil {
		t.Fatal("ByJobID duplicate job_id: got nil grab")
	}
	if latest.ID != newerGrabID {
		t.Fatalf("ByJobID duplicate job_id id = %q, want newest %q", latest.ID, newerGrabID)
	}
}
