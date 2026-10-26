// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/work"
)

func TestConfirmBindsThePickedJob(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_conf_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_conf_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Pick")

	// The human picks the candidate the predicate would only have offered for
	// review. That is the whole point of asking: they know which of two
	// versions they wanted, and the rule does not.
	res := b.ConfirmGrabCandidate(ctx, grabID, preprintJob)
	if res.Outcome != "job_created" {
		t.Fatalf("outcome = %q detail = %q, want job_created", res.Outcome, res.Detail)
	}
	if res.JobID != preprintJob {
		t.Fatalf("job = %q, want the picked job %q", res.JobID, preprintJob)
	}
	got, err := b.grabs.Get(ctx, grabID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.State != grab.StateJobCreated || got.JobID != preprintJob {
		t.Fatalf("grab = %+v, want bound to the picked job", got)
	}

	// The audit trail must record that a human decided this, and must NOT
	// claim it as an autonomous filing: grabs.binds exists to answer "what did
	// papio do while nobody was looking", and a confirmed pick is the opposite.
	if !strings.Contains(got.BindProvenance, "operator_confirm") {
		t.Fatalf("provenance = %q, want method operator_confirm", got.BindProvenance)
	}
	binds, err := b.grabs.ListAutonomousBinds(ctx, 10)
	if err != nil {
		t.Fatalf("ListAutonomousBinds: %v", err)
	}
	for _, bind := range binds {
		if bind.GrabID == grabID {
			t.Fatalf("confirmed pick %s appears in the autonomous bind audit: %+v", grabID, bind)
		}
	}
	// Confirming twice must not double-file: the grab has left the parked state.
	if again := b.ConfirmGrabCandidate(ctx, grabID, preprintJob); again.Outcome != "wrong_state" {
		t.Fatalf("second confirm outcome = %q, want wrong_state", again.Outcome)
	}
}

// TestConfirmFenceRejectionKeepsGrabRetryable pins that a pick whose job
// leaves the candidate pool between the preflight read and the bind commit is
// reported as a conflict WITHOUT stranding the grab: it stays parked, so the
// operator can ask grabs.suggest again and confirm a different job.
func TestConfirmFenceRejectionKeepsGrabRetryable(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	vorJob := parkManualDownload(t, jobs, "wr_fence_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_fence_pre", preprint)
	excerpt := exoticExcerpt(vor)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), excerpt, "Fence Retry")

	// Validate runs after the preflight pool read and before the bind
	// transaction, so cancelling the picked job from inside it is exactly the
	// "job left the pool mid-confirm" race.
	inner := b.svc.Validate
	cancelled := false
	b.svc.Validate = func(c context.Context, path, mime string, w work.Work) (pdf.ValidationReport, error) {
		if !cancelled {
			cancelled = true
			if err := jobs.Transition(c, preprintJob, job.StateAwaitingHuman, job.StateCancelled, map[string]any{"reason": "test_cancel"}); err != nil {
				t.Fatalf("cancel picked job: %v", err)
			}
		}
		return inner(c, path, mime, w)
	}

	res := b.ConfirmGrabCandidate(ctx, grabID, preprintJob)
	if res.Outcome != "conflict" {
		t.Fatalf("outcome = %q detail = %q, want conflict", res.Outcome, res.Detail)
	}
	got, err := b.grabs.Get(ctx, grabID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("state after rejected fence = %q, want parked_no_identifier so the grab stays retryable", got.State)
	}
	if retry := b.ConfirmGrabCandidate(ctx, grabID, vorJob); retry.Outcome != "job_created" {
		t.Fatalf("retry outcome = %q detail = %q, want job_created", retry.Outcome, retry.Detail)
	}
}

// TestConfirmRefusesAPickTheDocumentContradicts covers the one place a human's
// pick loses. A parked capture can carry a conclusive front-matter DOI: the
// mismatch park path (processSettledGrab's errGrabIdentityMismatch loop) parks a
// grab whose printed DOI named a ready bundle that MatchIdentity rejected. Those
// bytes state their own identity, so a pick naming a different work is refused —
// the human is authority on which pending paper they meant, never on what the
// bytes are.
func TestConfirmRefusesAPickTheDocumentContradicts(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_veto_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_veto_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Veto")

	// Confirm re-reads the bytes, so the report it sees is what decides. These
	// bytes conclusively name a third work inside the blind front-matter window.
	contradicting := "Some Other Paper Entirely\nBartholomew Quincy\nDOI: 10.7777/contradiction.1\n\nAbstract\nBody.\n"
	if got := pdf.FrontMatterDOIs(contradicting); len(got) == 0 {
		t.Fatalf("fixture has no conclusive front-matter DOI; the veto could not fire")
	}
	b.svc.Validate = validateForExcerpt(contradicting)

	res := b.ConfirmGrabCandidate(ctx, grabID, preprintJob)
	if res.Outcome != "refused_identity" {
		t.Fatalf("outcome = %q detail = %q, want refused_identity", res.Outcome, res.Detail)
	}
	if res.Detail == "" {
		t.Fatal("refusal carries no detail; the operator cannot tell what the document claimed")
	}
	got, _ := b.grabs.Get(ctx, grabID)
	if got.State != grab.StateParkedNoIdentifier || got.JobID != "" {
		t.Fatalf("grab = %+v, want untouched after a refused pick", got)
	}
	if got.BindProvenance != "" {
		t.Fatalf("provenance = %q, want none written for a refusal", got.BindProvenance)
	}
}

func TestConfirmGuards(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_guard_vor", vor)
	parkManualDownload(t, jobs, "wr_guard_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Guards")

	if got := b.ConfirmGrabCandidate(ctx, "grab_does_not_exist", "job_whatever"); got.Outcome != "unknown_grab" {
		t.Fatalf("unknown grab outcome = %q, want unknown_grab", got.Outcome)
	}
	// A job outside the candidate-eligible pool must be unreachable. The pool
	// is what the suggestion list was drawn from, so a stale page cannot bind
	// captured bytes to an arbitrary job id it happens to know.
	if got := b.ConfirmGrabCandidate(ctx, grabID, "job_00000000000000000000000999"); got.Outcome != "unknown_job" {
		t.Fatalf("absent job outcome = %q, want unknown_job", got.Outcome)
	}
}

func TestConfirmStagingDoesNotHoldSessionLockAndLatchesTimeout(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSyncAs(t, b, sessA, hello())
	runSyncAs(t, b, sessB, hello())
	runSyncAs(t, b, testSessionID, hello())
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_stage_vor", vor)
	picked := parkManualDownload(t, jobs, "wr_stage_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Blocked staging")
	otherGrab := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Latched confirmation")

	previousDeadline := AdoptionScanDeadline
	AdoptionScanDeadline = time.Second
	t.Cleanup(func() { AdoptionScanDeadline = previousDeadline })
	entered := make(chan struct{})
	release := make(chan struct{})
	var workers sync.WaitGroup
	var calls atomic.Int32
	jobDir := filepath.Join(cfg.EffectiveAdoptionRoot(), picked)
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir != jobDir {
			return os.ReadDir(dir)
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil, os.ErrPermission
	}
	t.Cleanup(func() {
		close(release)
		workers.Wait()
	})
	type reply struct {
		frames []json.RawMessage
		err    error
	}
	confirmed := make(chan reply, 1)
	request := inFrame(t, protocol.MsgPdfGrabConfirmRequest, "", protocol.PdfGrabConfirmRequestPayload{
		RequestID: "confirm-blocked-stage", GrabID: grabID, JobID: picked,
	})
	workers.Add(1)
	go func() {
		defer workers.Done()
		frames, err := b.Sync(ctx, sessB, false, []json.RawMessage{request})
		confirmed <- reply{frames, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation never reached adoption staging")
	}

	polled := make(chan reply, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		frames, err := b.Sync(ctx, testSessionID, false, nil)
		polled <- reply{frames, err}
	}()
	select {
	case got := <-polled:
		if got.err != nil || len(got.frames) != 0 {
			t.Fatalf("pending session poll = %+v, want an empty successful reply", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked grab staging holds the bridge mutex")
	}

	select {
	case got := <-confirmed:
		if got.err != nil || len(got.frames) != 1 {
			t.Fatalf("confirmation = %+v, want one structured response", got)
		}
		message, err := protocol.DecodeBrowserMessage(got.frames[0])
		if err != nil {
			t.Fatal(err)
		}
		outcome, ok := message.Payload.(*protocol.PdfGrabConfirmResponsePayload)
		if !ok || outcome.Outcome != "job_created" || outcome.JobID != picked {
			t.Fatalf("deferred confirmation = %+v, want the committed bind", message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("confirmation did not return after the staging deadline")
	}
	got, err := b.grabs.Get(ctx, grabID)
	if err != nil || got == nil || got.State != grab.StateJobCreated || got.JobID != picked {
		t.Fatalf("committed grab = %+v, err = %v", got, err)
	}
	if _, err := os.Stat(got.QuarantinePath); err != nil {
		t.Fatalf("deferred staging lost the quarantine copy: %v", err)
	}
	events, err := jobs.Events(ctx, picked)
	if err != nil {
		t.Fatal(err)
	}
	deferred := false
	for _, event := range events {
		deferred = deferred || event["kind"] == "browser.adoption_deferred"
	}
	if !deferred {
		t.Fatal("the committed bind has no deferred-adoption event")
	}
	if again := b.ConfirmGrabCandidate(ctx, otherGrab, picked); again.Outcome != "unavailable" || !strings.Contains(again.Detail, "downloads folder") {
		t.Fatalf("confirmation while latched = %+v, want the folder health refusal", again)
	}
	untouched, err := b.grabs.Get(ctx, otherGrab)
	if err != nil || untouched == nil || untouched.State != grab.StateParkedNoIdentifier || untouched.JobID != "" {
		t.Fatalf("latched confirmation changed the parked grab: %+v, %v", untouched, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("stalled staging calls = %d, want one outstanding operation", calls.Load())
	}
}

func TestConfirmRevalidatesAfterConcurrentGrabChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		row     bool
		source  bool
	}{
		{name: "dismiss bound grab", outcome: "job_created", row: true, source: true},
		{name: "grab row removed", outcome: "conflict", source: true},
		{name: "quarantine removed", outcome: "failed", row: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			runSyncAs(t, b, sessA, hello())
			runSyncAs(t, b, sessB, hello())
			vor, preprint := versionPair()
			parkManualDownload(t, jobs, "wr_recheck_vor", vor)
			picked := parkManualDownload(t, jobs, "wr_recheck_pre", preprint)
			grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Concurrent confirmation")
			before, err := b.grabs.Get(ctx, grabID)
			if err != nil || before == nil {
				t.Fatalf("parked grab = %+v, %v", before, err)
			}
			originalBytes, err := os.ReadFile(before.QuarantinePath)
			if err != nil {
				t.Fatal(err)
			}

			previousDeadline := AdoptionScanDeadline
			AdoptionScanDeadline = 5 * time.Second
			t.Cleanup(func() { AdoptionScanDeadline = previousDeadline })
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			jobDir := filepath.Join(cfg.EffectiveAdoptionRoot(), picked)
			b.readDir = func(dir string) ([]os.DirEntry, error) {
				if dir != jobDir {
					return os.ReadDir(dir)
				}
				close(entered)
				<-release
				// Keep ingestion deferred so this test can assert that the
				// committed grab retains its only recovery source.
				return nil, os.ErrPermission
			}
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				workers.Wait()
			})
			type reply struct {
				frames []json.RawMessage
				err    error
			}
			confirmed := make(chan reply, 1)
			request := inFrame(t, protocol.MsgPdfGrabConfirmRequest, "", protocol.PdfGrabConfirmRequestPayload{
				RequestID: "confirm-concurrent-change", GrabID: grabID, JobID: picked,
			})
			workers.Add(1)
			go func() {
				defer workers.Done()
				frames, err := b.Sync(ctx, sessB, false, []json.RawMessage{request})
				confirmed <- reply{frames, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("confirmation never reached post-bind staging")
			}

			changed := make(chan error, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				b.mu.Lock()
				defer b.mu.Unlock()
				var err error
				switch tc.name {
				case "dismiss bound grab":
					source := parkedGrabItemSource{store: jobs.S, grabs: b.grabs}
					var dismissed bool
					dismissed, err = source.DismissPdfGrab(ctx, grabID)
					if dismissed || !errors.Is(err, sql.ErrNoRows) {
						err = fmt.Errorf("dismiss committed grab = %v, %w; want the conflict refusal", dismissed, err)
					} else {
						err = nil // triage maps this refused dismissal to DecisionConflict
					}
				case "grab row removed":
					_, err = jobs.S.DB().ExecContext(ctx, `DELETE FROM pdf_grabs WHERE id = ?`, grabID)
				case "quarantine removed":
					err = os.Remove(before.QuarantinePath)
				}
				changed <- err
			}()
			select {
			case err := <-changed:
				if err != nil {
					t.Errorf("concurrent change: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the concurrent change could not acquire the session lock during staging")
			}
			releaseOnce.Do(func() { close(release) })

			select {
			case got := <-confirmed:
				if got.err != nil || len(got.frames) != 1 {
					t.Fatalf("confirmation = %+v, want one structured response", got)
				}
				message, err := protocol.DecodeBrowserMessage(got.frames[0])
				if err != nil {
					t.Fatal(err)
				}
				outcome, ok := message.Payload.(*protocol.PdfGrabConfirmResponsePayload)
				if !ok || outcome.Outcome != tc.outcome || outcome.JobID != picked {
					t.Fatalf("confirmation after %s = %+v, want %s for %s", tc.name, message, tc.outcome, picked)
				}
				if tc.outcome != "job_created" && outcome.Detail == "" {
					t.Fatal("the structured refusal has no explanation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("confirmation did not finish after staging resumed")
			}

			after, err := b.grabs.Get(ctx, grabID)
			if err != nil || (after != nil) != tc.row {
				t.Fatalf("grab after %s = %+v, %v", tc.name, after, err)
			}
			if after != nil && (after.State != grab.StateJobCreated || after.JobID != picked || after.QuarantinePath != before.QuarantinePath) {
				t.Fatalf("committed bind changed during %s: %+v", tc.name, after)
			}
			remaining, err := os.ReadFile(before.QuarantinePath)
			if tc.source {
				if err != nil || !bytes.Equal(remaining, originalBytes) {
					t.Fatalf("recovery source changed during %s: %v", tc.name, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("removed quarantine returned %v, want not-exist", err)
			}
			row, err := jobs.Get(ctx, picked)
			if err != nil || row == nil || row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" {
				t.Fatalf("job after %s = %+v, %v", tc.name, row, err)
			}
		})
	}
}

func TestConcurrentConfirmKeepsWinnerStagingOwnedDuringRecovery(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	vor, preprint := versionPair()
	picked := parkManualDownload(t, jobs, "wr_owner_vor", vor)
	parkManualDownload(t, jobs, "wr_owner_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Concurrent staging owners")
	g, err := b.grabs.Get(ctx, grabID)
	if err != nil || g == nil {
		t.Fatalf("parked grab = %+v, %v", g, err)
	}
	digest, err := fileDigest(g.QuarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	previousDeadline := AdoptionScanDeadline
	AdoptionScanDeadline = 5 * time.Second
	t.Cleanup(func() { AdoptionScanDeadline = previousDeadline })
	winnerEntered, loserEntered, stagingEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseWinner, releaseLoser, releaseStaging := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var winnerOnce, loserOnce, stagingOnce sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		winnerOnce.Do(func() { close(releaseWinner) })
		loserOnce.Do(func() { close(releaseLoser) })
		stagingOnce.Do(func() { close(releaseStaging) })
		workers.Wait()
	})
	validate := b.svc.Validate
	var validations, stages atomic.Int32
	b.svc.Validate = func(ctx context.Context, path, mime string, target work.Work) (pdf.ValidationReport, error) {
		if path == g.QuarantinePath {
			switch validations.Add(1) {
			case 1:
				close(winnerEntered)
				<-releaseWinner
			case 2:
				close(loserEntered)
				<-releaseLoser
			}
		}
		return validate(ctx, path, mime, target)
	}
	jobDir := filepath.Join(cfg.EffectiveAdoptionRoot(), picked)
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == jobDir && stages.Add(1) == 1 {
			close(stagingEntered)
			<-releaseStaging
		}
		return os.ReadDir(dir)
	}
	start := func() <-chan GrabConfirmResult {
		result := make(chan GrabConfirmResult, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			result <- b.ConfirmGrabCandidate(ctx, grabID, picked)
		}()
		return result
	}
	await := func(entered <-chan struct{}, phase string) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("confirmation never reached %s", phase)
		}
	}
	winner := start()
	await(winnerEntered, "winner validation")
	loser := start()
	await(loserEntered, "loser validation")
	winnerOnce.Do(func() { close(releaseWinner) })
	await(stagingEntered, "winner staging")
	loserOnce.Do(func() { close(releaseLoser) })
	select {
	case result := <-loser:
		if result.Outcome != "conflict" {
			t.Fatalf("losing confirmation = %+v, want conflict", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("losing confirmation did not finish")
	}

	recovered := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		b.recoverCommittedGrabStaging(ctx)
		close(recovered)
	}()
	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("recovery competed for the winner's still-owned staging")
	}
	if _, err := os.Stat(g.QuarantinePath); err != nil {
		t.Fatalf("recovery removed the live winner's source: %v", err)
	}
	stagingOnce.Do(func() { close(releaseStaging) })
	select {
	case result := <-winner:
		if result.Outcome != "job_created" {
			t.Fatalf("winning confirmation = %+v, want job_created", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winning confirmation did not finish")
	}
	row, err := jobs.Get(ctx, picked)
	if err != nil || row == nil || row.State != job.StateReady || row.ArtifactSHA256 != digest {
		t.Fatalf("confirmed job = %+v, %v; want the captured PDF ready", row, err)
	}
	if err := b.svc.Artifacts.Verify(digest); err != nil {
		t.Fatalf("confirmed artifact: %v", err)
	}
	bound, err := b.grabs.Get(ctx, grabID)
	if err != nil || bound == nil || bound.State != grab.StateJobCreated || bound.JobID != picked {
		t.Fatalf("confirmed grab = %+v, %v", bound, err)
	}
	if _, err := os.Stat(g.QuarantinePath); !os.IsNotExist(err) {
		t.Fatalf("successful confirmation left its quarantine: %v", err)
	}
}

func TestConfirmRecognizesCommittedGrabRecovery(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	vor, preprint := versionPair()
	picked := parkManualDownload(t, jobs, "wr_recovered_vor", vor)
	parkManualDownload(t, jobs, "wr_recovered_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Recovered confirmation")
	jobDir := filepath.Join(cfg.EffectiveAdoptionRoot(), picked)
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == jobDir {
			return nil, os.ErrPermission
		}
		return os.ReadDir(dir)
	}
	if result := b.ConfirmGrabCandidate(ctx, grabID, picked); result.Outcome != "job_created" {
		t.Fatalf("deferred confirmation = %+v", result)
	}
	b.readDir = os.ReadDir
	g, err := b.grabs.Get(ctx, grabID)
	if err != nil || g == nil {
		t.Fatalf("bound grab = %+v, %v", g, err)
	}
	name, _, digest, err := b.stageGrabCopy(g.QuarantinePath, jobDir, filepath.Base(g.QuarantinePath))
	if err != nil {
		t.Fatal(err)
	}
	b.recoverCommittedGrabStaging(ctx)
	row, err := jobs.Get(ctx, picked)
	if err != nil || row == nil || row.State != job.StateReady || row.ArtifactSHA256 != digest {
		t.Fatalf("recovered job = %+v, %v", row, err)
	}
	if _, err := os.Stat(g.QuarantinePath); !os.IsNotExist(err) {
		t.Fatalf("recovery did not consume quarantine: %v", err)
	}
	b.mu.Lock()
	result := b.revalidateGrabConfirmation(ctx, g, picked, name, digest)
	otherBytes := b.revalidateGrabConfirmation(ctx, g, picked, name, strings.Repeat("0", 64))
	b.mu.Unlock()
	if result.Outcome != "job_created" || result.JobID != picked {
		t.Fatalf("confirmation after recovery = %+v, want job_created", result)
	}
	if otherBytes.Outcome == "job_created" || otherBytes.Outcome == "" {
		t.Fatalf("a different ready artifact satisfied confirmation: %+v", otherBytes)
	}
}
