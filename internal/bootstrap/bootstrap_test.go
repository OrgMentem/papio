// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
	"papio/internal/zotio"
)

func TestNewWiresResolverOrderAndCoreServices(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Errorf("close system: %v", err)
		}
	})
	var names []string
	for _, entry := range system.App.Resolvers {
		if entry.Adapter == nil {
			t.Fatal("nil resolver adapter")
		}
		names = append(names, entry.Adapter.Name())
	}
	want := []string{
		config.SourceArXiv,
		config.SourceEuropePMC,
		config.SourceUnpaywall,
		config.SourceOpenAlex,
		config.SourceSemanticScholar,
		config.SourceCORE,
		config.SourceCrossrefTDM,
		config.SourceOpenAIRE,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("resolver order = %v, want %v", names, want)
	}
	if system.Pulse == nil {
		t.Fatal("bootstrap left pulse service unwired")
	}
	pulseField := reflect.ValueOf(system.Browser).Elem().FieldByName("pulse")
	if !pulseField.IsValid() || pulseField.IsNil() {
		t.Fatal("bootstrap did not wire pulse service into browser bridge")
	}
	maintenance, ok := system.Scheduler.Config.Maintenance.(daemon.MaintenanceRunners)
	if !ok {
		t.Fatalf("maintenance = %T, want daemon.MaintenanceRunners", system.Scheduler.Config.Maintenance)
	}
	foundReminder := false
	for _, runner := range maintenance {
		if _, ok := runner.(*app.ActionReminder); ok {
			foundReminder = true
			break
		}
	}
	if !foundReminder {
		t.Fatal("bootstrap did not register the action reminder")
	}
	if system.Zotio.AutoEnrich {
		t.Fatal("bootstrap ignored zotio.auto_enrich=false")
	}
	if system.PDFCapability.PDFToPPM != "" || system.PDFCapability.Tesseract != "" {
		t.Fatal("OCR helpers remained enabled when pdf.ocr_enabled=false")
	}
}

func TestNewImportsEveryUnresolvedLegacyBrowserEffectBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false

	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacyJob := func(label string) string {
		id, createErr := first.Jobs.CreateRequest(
			ctx, label, work.Work{DOI: "10.1002/" + label}, "", "", job.Policy{
				AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
			}, nil, job.PrincipalUnknown,
		)
		if createErr != nil {
			t.Fatalf("create %s job: %v", label, createErr)
		}
		return id
	}
	genericJob := legacyJob("legacy-effect-generic")
	if err := first.Jobs.RecordEvent(ctx, genericJob, "browser.provider_drive_epoch_started", map[string]any{
		"drive_attempt_id": "legacy-bootstrap-generic", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:generic",
	}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	directJob := legacyJob("legacy-effect-direct")
	if err := first.Jobs.RecordEvent(ctx, directJob, "browser.direct_route", map[string]any{
		"phase": "offered", "drive_attempt_id": "legacy-bootstrap-direct", "ordinal": int64(1),
		"route_revision": "route-1", "safety_domain": "institution:direct",
	}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	institutionalJob := legacyJob("legacy-effect-institutional")
	execSQL := func(query string, args ...any) {
		if _, err := first.Store.DB().ExecContext(ctx, query, args...); err != nil {
			first.Close()
			t.Fatalf("seed schema-33 browser state: %v", err)
		}
	}
	execSQL(`
		INSERT INTO institution_profiles
		  (id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES ('legacy-bootstrap-profile', 'legacy profile', 1, 'digest', 'auth-claim',
		        '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z')`)
	execSQL(`
		INSERT INTO browser_candidates
		  (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		   route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
		   adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES ('legacy-bootstrap-candidate', ?, 1, 'legacy-bootstrap-profile', 1, 1, 'institutional', 'doi',
		        'pre-route', 'institution:institutional', 'adapter-1', 'effect-1', 'claimed',
		        '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z')`, institutionalJob)
	execSQL(`
		INSERT INTO materialization_claims
		  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
		   phase, route_issuance_ordinal, effect_ordinal, created_at, updated_at)
		VALUES ('legacy-bootstrap-claim', 'legacy-bootstrap-candidate', 1, 'browser_tab', 'legacy-bootstrap-binding',
		        'route_issued', 2, 3, '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z')`)
	execSQL(`
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES ('legacy-bootstrap-grab', 'example.test', 'legacy grab', 'awaiting_file',
		        '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z')`)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal("bootstrap rejected valid legacy state:", err)
	}
	defer restarted.Close()
	count, err := restarted.Jobs.UnresolvedLegacyEffectBlockerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("startup imported %d unresolved browser effects, want four", count)
	}
	var generic, direct, grab, institutional int
	if err := restarted.Store.DB().QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(effect_kind='generic_drive'),0),
		  COALESCE(SUM(effect_kind='direct_get'),0),
		  COALESCE(SUM(effect_kind='pdf_grab'),0),
		  COALESCE(SUM(effect_kind='institutional'),0)
		FROM legacy_effect_blockers WHERE status='unresolved'`).Scan(&generic, &direct, &grab, &institutional); err != nil {
		t.Fatal(err)
	}
	if generic != 1 || direct != 1 || grab != 1 || institutional != 1 {
		t.Fatalf("startup imported kinds generic=%d direct=%d grab=%d institutional=%d", generic, direct, grab, institutional)
	}
	freshJob, err := restarted.Jobs.CreateRequest(
		ctx, "post-import-admission", work.Work{DOI: "10.1002/post-import-admission"}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
		nil, job.PrincipalUnknown,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Jobs.Transition(ctx, freshJob, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Jobs.Transition(ctx, freshJob, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Jobs.OpenHumanAction(ctx, freshJob, "openurl_handoff", "post-import admission", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	_, outcome, err := restarted.Jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
		Identity: job.EffectPermitIdentity{
			JobID: freshJob, Kind: job.EffectKindGenericDrive, DriveAttemptID: "new-attempt",
			Ordinal: 0, Strategy: "generic", Revision: "1",
		},
		JobAttemptRevision: 1, BrowserHolderGeneration: 1, SafetyDomainID: "institution:new",
		LeaseUntil:    time.Now().Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "effect.authorized"},
	})
	if !errors.Is(err, job.ErrEffectPermitBusy) || outcome != job.EffectPermitBusyOutcome {
		t.Fatalf("admission outcome=%v err=%v, want busy from imported blocker", outcome, err)
	}
}

func TestNewRejectsMalformedLegacyBrowserEffectBeforeBrowserConstruction(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false

	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := first.Jobs.CreateRequest(ctx, "legacy-effect-malformed", work.Work{DOI: "10.1002/malformed"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalUnknown)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Jobs.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_started", map[string]any{
		"drive_attempt_id": "legacy-bootstrap-malformed", "ordinal": int64(0),
		"strategy": "generic", "revision": "1",
	}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(ctx, cfg)
	if err == nil || restarted != nil {
		if restarted != nil {
			restarted.Close()
		}
		t.Fatalf("malformed legacy state bootstrap = system=%v err=%v, want fatal startup error", restarted, err)
	}
	if !strings.Contains(err.Error(), "importing legacy browser effects") ||
		!strings.Contains(err.Error(), "unclassifiable legacy provider drive effect") {
		t.Fatalf("malformed bootstrap error = %v, want importer context", err)
	}
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var blockers int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_effect_blockers`).Scan(&blockers); err != nil {
		t.Fatal(err)
	}
	if blockers != 0 {
		t.Fatalf("failed startup left %d imported blockers, want transaction rollback", blockers)
	}
}

type autoImporterFunc func(context.Context, string) (string, string, string, error)

func (f autoImporterFunc) PlanAndApply(ctx context.Context, jobID string) (string, string, string, error) {
	return f(ctx, jobID)
}

func TestSerialAutoImporterSerializesConcurrentCalls(t *testing.T) {
	var active, maxActive, calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if current > 1 {
			return "failed", "", "", errors.New("concurrent call")
		}
		return "attached", "parent", "attachment", nil
	})
	serial := newSerialAutoImporter(importer)

	const workers = 20
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _, _, err := serial.PlanAndApply(context.Background(), "job")
			if err != nil {
				errs <- err
				return
			}
			if status != "attached" {
				errs <- errors.New("unexpected status")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent calls = %d, want 1", got)
	}
	if got := calls.Load(); got != workers {
		t.Fatalf("calls = %d, want %d", got, workers)
	}
}

func TestSerialAutoImporterRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		if calls.Add(1) == 1 {
			return "failed", "", "", errors.New("temporary failure")
		}
		return "attached", "parent", "attachment", nil
	})
	serial := newSerialAutoImporter(importer)
	serial.backoff = time.Millisecond

	status, parentKey, attachmentKey, err := serial.PlanAndApply(context.Background(), "job")
	if err != nil {
		t.Fatal(err)
	}
	if status != "attached" || parentKey != "parent" || attachmentKey != "attachment" {
		t.Fatalf("result = (%q, %q, %q), want attached result", status, parentKey, attachmentKey)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}
func TestSerialAutoImporterReleasesLockDuringRetryBackoff(t *testing.T) {
	firstFailed := make(chan struct{})
	otherStarted := make(chan struct{})
	var firstOnce, otherOnce sync.Once
	importer := autoImporterFunc(func(_ context.Context, jobID string) (string, string, string, error) {
		switch jobID {
		case "retry":
			firstOnce.Do(func() { close(firstFailed) })
			return "failed", "", "", errors.New("temporary failure")
		case "other":
			otherOnce.Do(func() { close(otherStarted) })
			return "attached", "parent", "attachment", nil
		default:
			t.Fatalf("unexpected job ID %q", jobID)
			return "", "", "", nil
		}
	})
	serial := newSerialAutoImporter(importer)
	serial.backoff = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retryDone := make(chan error, 1)
	go func() {
		_, _, _, err := serial.PlanAndApply(ctx, "retry")
		retryDone <- err
	}()

	<-firstFailed
	otherDone := make(chan error, 1)
	go func() {
		_, _, _, err := serial.PlanAndApply(context.Background(), "other")
		otherDone <- err
	}()
	select {
	case <-otherStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("other import remained blocked by retry backoff")
	}
	if err := <-otherDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-retryDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("retry error = %v, want context cancellation", err)
	}
}

func TestSerialAutoImporterClassifiesFinalError(t *testing.T) {
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		return "failed", "", "", errors.New("zotio stderr: unknown item field at /Users/reader/private.json")
	})
	serial := newSerialAutoImporter(importer)
	serial.backoff = time.Millisecond

	_, _, _, err := serial.PlanAndApply(context.Background(), "job")
	info := zotio.ErrorInfoFrom(err)
	if info.Class != zotio.ErrorClassZoteroFieldValidation || info.Hint != "unknown item field" {
		t.Fatalf("classified retry error = %+v", info)
	}
}

func TestSerialAutoImporterStopsRetryWhenContextCancelled(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	var calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		return "failed", "", "", errors.New("temporary failure")
	})
	serial := newSerialAutoImporter(importer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, _, err := serial.PlanAndApply(ctx, "job")
		result <- err
	}()

	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff did not stop after context cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}
