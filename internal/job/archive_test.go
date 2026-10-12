// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestArchiveRetainsIdentityArtifactsAndRestoresActionableReady(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_archive", testWork(), "", "", testPolicy(), nil, PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Archive(ctx, id, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued archive = %v", err)
	}
	sha := strings.Repeat("a", 64)
	if err := js.UpsertArtifact(ctx, Artifact{SHA256: sha, SizeBytes: 5000, MIME: "application/pdf", IdentityResult: "pass", Path: "/fixture/immutable.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateReady, nil, WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	before, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	components, err := js.Components(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Archive(ctx, id, false); !errors.Is(err, ErrArchiveConfirmation) {
		t.Fatalf("unconfirmed archive = %v", err)
	}
	if err := js.RecordEvent(ctx, id, "hook.filing_reserved", map[string]any{"reservation_id": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Archive(ctx, id, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("archive during hook = %v", err)
	}
	if err := js.RecordEvent(ctx, id, "hook.filing_released", map[string]any{"reservation_id": "unrelated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Archive(ctx, id, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrelated release allowed archive: %v", err)
	}
	if err := js.RecordEvent(ctx, id, "hook.filing_released", map[string]any{"reservation_id": "first"}); err != nil {
		t.Fatal(err)
	}
	archived, err := js.Archive(ctx, id, true)
	if err != nil || archived.Disposition != DispositionArchived || !archived.Changed {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	again, err := js.Archive(ctx, id, true)
	if err != nil || again.Changed {
		t.Fatalf("archive replay = %+v, %v", again, err)
	}
	after, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("archive changed acquisition: before=%+v after=%+v", before, after)
	}
	afterComponents, err := js.Components(ctx, id)
	if err != nil || !reflect.DeepEqual(components, afterComponents) {
		t.Fatalf("artifact references changed: %+v, %v", afterComponents, err)
	}
	ready, err := js.List(ctx, StateReady, 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("actionable ready = %+v, %v", ready, err)
	}
	if err := js.Transition(ctx, id, StateReady, StateImported, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived import transition = %v", err)
	}
	cached, _, err := js.FindArtifactByDOI(ctx, testWork().DOI)
	if err != nil || cached == nil || cached.SHA256 != sha {
		t.Fatalf("archived cache = %+v, %v", cached, err)
	}
	restored, err := js.Restore(ctx, id)
	if err != nil || restored.Disposition != DispositionActive || !restored.Changed {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	ready, err = js.List(ctx, StateReady, 10)
	if err != nil || len(ready) != 1 || ready[0].ID != id {
		t.Fatalf("restored ready = %+v, %v", ready, err)
	}
	var archivedEvents, restoredEvents int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT SUM(kind = 'acquisition.archived'), SUM(kind = 'acquisition.restored') FROM events WHERE job_id = ?`, id).Scan(&archivedEvents, &restoredEvents); err != nil {
		t.Fatal(err)
	}
	if archivedEvents != 1 || restoredEvents != 1 {
		t.Fatalf("audit counts = %d, %d", archivedEvents, restoredEvents)
	}
}
