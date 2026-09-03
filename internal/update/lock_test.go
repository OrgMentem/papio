// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Two papio processes, the daemon and a CLI invocation, each build their own
// Checker over the same data directory. Instance mutexes coordinate nothing
// between them, so both used to read the same stale cache, both used to see no
// recent nag, and the user got the update prompt twice.
func TestTryMarkNaggedOncePerDataDirectory(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	daemon := New(dataDir)
	cli := New(dataDir)

	results := make([]bool, 2)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for index, checker := range []*Checker{daemon, cli} {
		done.Add(1)
		go func(index int, checker *Checker) {
			defer done.Done()
			start.Wait()
			results[index] = checker.TryMarkNagged(now)
		}(index, checker)
	}
	start.Done()
	done.Wait()

	marked := 0
	for _, ok := range results {
		if ok {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("checkers that recorded the nag = %d, want 1 (results %v)", marked, results)
	}
	if got := New(dataDir).readCache().LastNaggedAt; !got.Equal(now) {
		t.Fatalf("persisted last_nagged_at = %v, want %v", got, now)
	}
}

// A cache write derived from a snapshot read before another process wrote the
// file used to clobber that process's field. updateCache re-reads inside the
// lock, so a mutation only replaces what it names.
func TestUpdateCacheKeepsFieldsWrittenAfterTheSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	writer := New(dataDir)
	other := New(dataDir)

	// The snapshot the release path would carry across its HTTP request.
	snapshot := writer.readCache()
	if snapshot.InstalledVersion != "" {
		t.Fatalf("fresh cache installed_version = %q", snapshot.InstalledVersion)
	}

	other.RememberInstalledVersion("2.0.0")

	merged, err := writer.updateCache(func(cached *cache) bool {
		cached.LatestVersion = "9.9.9"
		cached.URL = "https://example.test/releases/v9.9.9"
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged.InstalledVersion != "2.0.0" {
		t.Fatalf("merged installed_version = %q, want 2.0.0", merged.InstalledVersion)
	}
	persisted := New(dataDir).readCache()
	if persisted.InstalledVersion != "2.0.0" || persisted.LatestVersion != "9.9.9" {
		t.Fatalf("persisted cache = %#v, want both fields", persisted)
	}
}

// The release path reads the cache, issues its GET, and only then persists.
// A second papio process that records a Zotio preflight during that request
// must not lose its field, and the release path must not be holding the cache
// lock while the request is in flight.
func TestCheckKeepsInstalledVersionWrittenDuringTheRequest(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	preflights := New(dataDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		preflights.RememberInstalledVersion("2.0.0")
		w.Header().Set("ETag", `"release-1"`)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example.test/releases/v9.9.9"}`))
	}))
	defer server.Close()

	releases := NewWithOptions(Options{
		DataDir:     dataDir,
		ReleasesURL: server.URL,
		Client:      server.Client(),
		Now:         func() time.Time { return now },
	})
	info := releases.Check(context.Background())
	if info == nil || info.LatestVersion != "9.9.9" {
		t.Fatalf("check info = %#v, want 9.9.9", info)
	}
	persisted := New(dataDir).readCache()
	if persisted.LatestVersion != "9.9.9" || persisted.InstalledVersion != "2.0.0" {
		t.Fatalf("persisted cache = %#v, want both the release and the preflight version", persisted)
	}
}

// The same loss happens without an ordering seam once two processes interleave
// their read-modify-write cycles: whichever writes last wins the whole file.
func TestConcurrentCheckersKeepEachOthersFields(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	dataDir := t.TempDir()
	releases := New(dataDir)
	preflights := New(dataDir)

	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		for round := range 50 {
			releases.recordAttempt(now.Add(time.Duration(round) * time.Second))
		}
	}()
	go func() {
		defer done.Done()
		for range 50 {
			preflights.RememberInstalledVersion("2.0.0")
		}
	}()
	done.Wait()

	persisted := New(dataDir).readCache()
	if persisted.InstalledVersion != "2.0.0" {
		t.Fatalf("installed_version = %q, want 2.0.0 (cache %#v)", persisted.InstalledVersion, persisted)
	}
	if persisted.LastAttemptAt.IsZero() {
		t.Fatalf("last_attempt_at was lost (cache %#v)", persisted)
	}
}

// The advisory lock must be cross-process, so a Checker waits for a lock held
// on an independent descriptor instead of writing through it.
func TestUpdateCacheWaitsForAnotherHolder(t *testing.T) {
	dataDir := t.TempDir()
	holder := New(dataDir)
	waiter := New(dataDir)

	release, err := holder.acquireCacheLock()
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		close(entered)
		_, err := waiter.updateCache(func(cached *cache) bool {
			cached.InstalledVersion = "2.0.0"
			return true
		})
		written <- err
	}()

	<-entered
	select {
	case err := <-written:
		t.Fatalf("write completed while the lock was held (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := waiter.readCache().InstalledVersion; got != "" {
		t.Fatalf("installed_version = %q while the lock was held", got)
	}

	release()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got := waiter.readCache().InstalledVersion; got != "2.0.0" {
		t.Fatalf("installed_version after release = %q, want 2.0.0", got)
	}
}
