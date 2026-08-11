// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package captures

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreAndListRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	at := time.Date(2026, time.July, 27, 12, 34, 56, 789000000, time.UTC)
	store := New(dataDir, Retention{MaxPerHost: 10, MaxAge: 14 * 24 * time.Hour})
	store.now = func() time.Time { return at }
	html := []byte("<!doctype html><title>fixture</title>")

	path, err := store.Store(ctx, "sagepub.com", "observed", "sage", "1.2.3", html)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(html) {
		t.Fatalf("stored HTML = %q, %v; want %q", got, err, html)
	}

	listed, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("captures = %d, want 1", len(listed))
	}
	got := listed[0]
	if got.Host != "sagepub.com" || got.Scenario != "observed" || got.AdapterID != "sage" || got.AdapterVersion != "1.2.3" {
		t.Fatalf("capture metadata = %#v", got)
	}
	if !got.Timestamp.Equal(at) || got.Path != path || got.Size != int64(len(html)) {
		t.Fatalf("capture = %#v, want timestamp=%s path=%q size=%d", got, at, path, len(html))
	}
}

func TestStoreSanitizedRecordsHashAndProvenance(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	html := []byte("<!-- papio-fixture provider=\"sage\" scenario=\"observed\" origin=\"https://sage.example/\" captured=\"2026-08-10T00:00:00Z\" -->\n<html>safe</html>")
	path, err := store.StoreSanitized(ctx, "sage.example", "observed", "sage", "1.2.3", html)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != path {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].SHA256 == "" || rows[0].SanitizerProvenance != SanitizerProvenance || rows[0].SanitizerVersion != SanitizerVersion {
		t.Fatalf("sanitized metadata = %#v", rows[0])
	}
	if _, err := store.StoreSanitized(ctx, "sage.example", "observed", "sage", "1.2.3", []byte("<html>raw</html>")); err == nil {
		t.Fatal("raw HTML was accepted through trusted ingress")
	}
}
func TestUpdateJobMarksOnlyDaemonCorrelatedEvidenceIndependent(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	html := []byte("<!-- papio-fixture provider=\"sage\" scenario=\"success\" origin=\"https://sage.example/article\" captured=\"2026-08-10T00:00:00Z\" -->\n<html>safe</html>")
	path, err := store.StoreSanitized(ctx, "sage.example", "success", "sage", "1.2.3", html)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(ctx)
	if err != nil || len(rows) != 1 || rows[0].IndependentEvidence {
		t.Fatalf("caller-labelled capture evidence = %#v, %v; want untrusted", rows, err)
	}
	if err := store.UpdateJob(ctx, "job-correlated", path, path); err != nil {
		t.Fatal(err)
	}
	rows, err = store.List(ctx)
	if err != nil || len(rows) != 1 || !rows[0].IndependentEvidence {
		t.Fatalf("correlated capture evidence = %#v, %v; want independent", rows, err)
	}
}

func TestStorePrunesRetention(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		ctx := context.Background()
		store := New(t.TempDir(), Retention{MaxPerHost: 2, MaxAge: 14 * 24 * time.Hour})
		now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
		store.now = func() time.Time { return now }

		now = now.Add(-2 * time.Minute)
		oldest, err := store.Store(ctx, "sagepub.com", "observed", "sage", "", []byte("oldest"))
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err := store.Store(ctx, "sagepub.com", "success", "sage", "", []byte("middle")); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err := store.Store(ctx, "sagepub.com", "drift", "sage", "", []byte("newest")); err != nil {
			t.Fatal(err)
		}

		listed, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 2 || listed[0].Scenario != "drift" || listed[1].Scenario != "success" {
			t.Fatalf("retained captures = %#v, want newest two", listed)
		}
		if _, err := os.Stat(oldest); !os.IsNotExist(err) {
			t.Fatalf("oldest capture remains after count pruning: %v", err)
		}
	})

	t.Run("age", func(t *testing.T) {
		ctx := context.Background()
		store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 24 * time.Hour})
		now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
		store.now = func() time.Time { return now }

		now = now.Add(-48 * time.Hour)
		old, err := store.Store(ctx, "sagepub.com", "observed", "sage", "", []byte("old"))
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(48 * time.Hour)
		if _, err := store.Store(ctx, "sagepub.com", "success", "sage", "", []byte("fresh")); err != nil {
			t.Fatal(err)
		}

		listed, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].Scenario != "success" {
			t.Fatalf("retained captures = %#v, want fresh capture only", listed)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("expired capture remains after age pruning: %v", err)
		}
	})
}

func TestStoreHostCannotEscapeCapturesDirectory(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store := New(dataDir, Retention{MaxPerHost: 10, MaxAge: 14 * 24 * time.Hour})

	path, err := store.Store(ctx, "../../outside", "observed", "", "", []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dataDir, capturesDir)
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		t.Fatalf("capture path %q escapes %q", path, root)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "outside")); !os.IsNotExist(err) {
		t.Fatalf("host traversal created an outside path: %v", err)
	}
}

func TestPurge(t *testing.T) {
	ctx := context.Background()
	store := New(t.TempDir(), Retention{MaxPerHost: 10, MaxAge: 14 * 24 * time.Hour})
	for _, host := range []string{"sagepub.com", "wiley.com"} {
		if _, err := store.Store(ctx, host, "observed", "", "", []byte(host)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.Purge(ctx, "sagepub.com")
	if err != nil || removed != 1 {
		t.Fatalf("purge host = (%d, %v), want (1, nil)", removed, err)
	}
	removed, err = store.Purge(ctx, "")
	if err != nil || removed != 1 {
		t.Fatalf("purge all = (%d, %v), want (1, nil)", removed, err)
	}
}
