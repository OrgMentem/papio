// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package grab

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestGrabDismissalReleasesConnectionBeforeCleanup(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	g, err := svc.Allocate(ctx, "pdf.example.org", "Dismiss without locking database")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	cleanupFailure := errors.New("cleanup interrupted")
	err = svc.DeleteParked(ctx, g.ID, func(string) error {
		// This read needs the store's only connection while cleanup is active.
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		row, err := svc.Get(readCtx, g.ID)
		if err != nil {
			return err
		}
		if row == nil || row.State != StateDismissing {
			t.Errorf("cleanup started without a durable claim: %+v", row)
		}
		if err := svc.MarkJobCreated(readCtx, g.ID, "irrelevant-job", "job_created"); !errors.Is(err, ErrStateChanged) {
			t.Errorf("bind during cleanup = %v, want state conflict", err)
		}
		if err := svc.MarkIdentified(readCtx, g.ID); !errors.Is(err, ErrStateChanged) {
			t.Errorf("identify during cleanup = %v, want state conflict", err)
		}
		pending, err := svc.PendingNotifications(readCtx, 10)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Errorf("dismissed grab still advertises needs_identifier: %+v", pending)
		}
		return cleanupFailure
	})
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("cleanup could not use the database: %v", err)
	}
	pending, err := svc.PendingRecovery(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != g.ID || pending[0].State != StateDismissing {
		t.Fatalf("unfinished claim missing from recovery: %+v, %v", pending, err)
	}
	if err := svc.DeleteParked(ctx, g.ID, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if row, err := svc.Get(ctx, g.ID); err != nil || row != nil {
		t.Fatalf("completed dismissal retained its row: %+v, %v", row, err)
	}
}
