package watch

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestEditPreservesAlertIdentityHistoryAndPendingDigest(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	input := testWatchInput("unchanged query")
	input.Mode = ModeAlert
	w := createWatch(t, s, input)
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	entries := []DigestEntry{{WorkKey: "10.1000/pending", DOI: "10.1000/pending", Title: "Pending Work"}, {WorkKey: "10.1000/answered", DOI: "10.1000/answered", Title: "Answered Work"}}
	if _, err := s.RecordDigest(ctx, w.ID, at, entries); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeDigestEntry(ctx, w.ID, entries[1].WorkKey); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRun(ctx, w.ID, at); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Digest(ctx, w.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	label, collection, cadence, cap, oa := "Revised label", "Reading", 168, 3, true
	updated, err := s.Edit(ctx, EditInput{ID: w.ID, Label: &label, Collection: &collection, CadenceHours: &cadence, PerRunCap: &cap, OAOnly: &oa})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != before.ID || updated.Mode != ModeAlert || updated.Query != before.Query || updated.LastRunAt != before.LastRunAt || updated.CreatedAt != before.CreatedAt || updated.CadenceHours != cadence || updated.PerRunCap != cap || updated.Collection != collection || !updated.Filters.OAOnly {
		t.Fatalf("updated watch = %+v, before = %+v", updated, before)
	}
	// A new store wrapper models the durable state read after a restart.
	restarted := NewStore(s.S)
	after, err := restarted.Digest(ctx, w.ID, 100)
	if err != nil || !reflect.DeepEqual(after, pending) {
		t.Fatalf("pending digest changed: %+v, %v", after, err)
	}
	if count, err := restarted.RecordDigest(ctx, w.ID, at.Add(time.Hour), entries); err != nil || count != 0 {
		t.Fatalf("re-reported = %d, %v", count, err)
	}
	due, err := restarted.Due(ctx, at.Add(24*time.Hour))
	if err != nil || len(due) != 0 {
		t.Fatalf("old cadence still applies: %+v, %v", due, err)
	}
}

func TestEditInvalidSettingsLeaveAllWatchFieldsUnchanged(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	w := createWatch(t, s, testWatchInput("atomic edit"))
	label, invalid := "Must not commit", -1
	if _, err := s.Edit(ctx, EditInput{ID: w.ID, Label: &label, CadenceHours: &invalid}); err == nil {
		t.Fatal("invalid edit succeeded")
	}
	after, err := s.Get(ctx, w.ID)
	if err != nil || !reflect.DeepEqual(after, w) {
		t.Fatalf("invalid edit changed watch: %+v, %v", after, err)
	}
}
