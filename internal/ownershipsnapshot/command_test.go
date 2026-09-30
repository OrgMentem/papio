// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ownershipsnapshot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/ownership"
)

// commandScript drives every command-source test. The script is written once,
// before any run, and its behavior is switched through a mode file: rewriting
// an executable while another test forks can fail with ETXTBSY on Linux.
const commandScript = `#!/bin/sh
dir=$(dirname "$0")
echo run >> "$dir/runs"
case "$(cat "$dir/mode")" in
ok) cat "$dir/data" ;;
exit) cat "$dir/data"; exit 3 ;;
sleep) sleep 30 ;;
flood) while :; do cat "$dir/data"; done ;;
malformed) printf '@article{broken,\n title = {unterminated\n' ;;
wait) while [ ! -f "$dir/release" ]; do sleep 0.01; done; cat "$dir/data" ;;
esac
`

type commandFixture struct {
	t      *testing.T
	dir    string
	script string
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "holdings.sh")
	if err := os.WriteFile(script, []byte(commandScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &commandFixture{t: t, dir: dir, script: script}
	fixture.setMode("ok")
	fixture.setData(oneEntryBibTeX("10.1000/one"))
	return fixture
}

func (f *commandFixture) setMode(mode string) {
	f.t.Helper()
	writeFile(f.t, f.dir, "mode", mode)
}

func (f *commandFixture) setData(body string) {
	f.t.Helper()
	writeFile(f.t, f.dir, "data", body)
}

// runs counts real executions of the script, not provider bookkeeping.
func (f *commandFixture) runs() int {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func (f *commandFixture) provider(clock *fakeClock, change func(*config.LibrarySource)) *commandProvider {
	f.t.Helper()
	source := config.LibrarySource{
		Name:   "papis-live",
		Kind:   config.LibraryKindCommand,
		Argv:   []string{"/bin/sh", f.script},
		Format: "bibtex",
		Claim:  config.LibraryClaimPDFPresent,
	}
	if change != nil {
		change(&source)
	}
	provider, err := NewProvider(source, clock.Now)
	if err != nil {
		f.t.Fatalf("NewProvider: %v", err)
	}
	return provider.(*commandProvider)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestCommandSourceRerunsOnlyAfterRefreshInterval(t *testing.T) {
	fixture := newCommandFixture(t)
	clock := newFakeClock()
	provider := fixture.provider(clock, func(source *config.LibrarySource) { source.RefreshSeconds = 60 })
	queries := []ownership.Query{doiQuery("10.1000/one"), doiQuery("10.1000/two")}

	claims, health := provider.Lookup(context.Background(), queries)
	if !health.Complete || health.EntryCount != 1 {
		t.Fatalf("health = %+v, want one complete entry", health)
	}
	if len(claims[0]) != 1 || len(claims[1]) != 0 {
		t.Fatalf("claims = %+v, want only 10.1000/one", claims)
	}
	if got := ownership.Decide(queries[0], ownership.WorkResult{Claims: claims[0]}); !got.Suppress {
		t.Fatal("a fresh pdf_present claim from a command must suppress acquisition")
	}

	// The library changes, but the last run is younger than the interval.
	fixture.setData(oneEntryBibTeX("10.1000/two"))
	clock.Advance(59 * time.Second)
	claims, health = provider.Lookup(context.Background(), queries)
	if !health.Complete || len(claims[0]) != 1 || len(claims[1]) != 0 {
		t.Fatalf("within the interval: health = %+v, claims = %+v, want the cached run", health, claims)
	}
	if got := fixture.runs(); got != 1 {
		t.Fatalf("runs within the refresh interval = %d, want 1", got)
	}

	clock.Advance(time.Second)
	claims, health = provider.Lookup(context.Background(), queries)
	if !health.Complete {
		t.Fatalf("health = %+v, want complete", health)
	}
	if len(claims[0]) != 0 || len(claims[1]) != 1 {
		t.Fatalf("after the interval: claims = %+v, want only 10.1000/two", claims)
	}
	if got := fixture.runs(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestCommandSourceConcurrentLookupsShareOneRun(t *testing.T) {
	fixture := newCommandFixture(t)
	fixture.setMode("wait")
	provider := fixture.provider(newFakeClock(), nil)
	queries := []ownership.Query{doiQuery("10.1000/one")}

	results := make(chan ownership.SourceHealth, 9)
	var callers sync.WaitGroup
	for range 9 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, health := provider.Lookup(context.Background(), queries)
			results <- health
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for fixture.runs() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFile(t, fixture.dir, "release", "")
	callers.Wait()
	close(results)
	for health := range results {
		if !health.Complete {
			t.Fatalf("health = %+v, want complete", health)
		}
	}
	if got := fixture.runs(); got != 1 {
		t.Fatalf("runs = %d, want 1 shared by nine concurrent lookups", got)
	}
}

// Every way a run can go wrong must leave the source incomplete. An empty
// answer here would read as "not held" and re-acquire the whole library.
func TestCommandSourceFailuresFailClosed(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"sleep", ownership.FailureTimeout},
		{"exit", ownership.FailureExit},
		{"flood", ownership.FailureTruncated},
		{"malformed", ownership.FailureParse},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			fixture := newCommandFixture(t)
			fixture.setMode(tc.mode)
			provider := fixture.provider(newFakeClock(), func(source *config.LibrarySource) {
				source.TimeoutSeconds = 1
				source.MaxOutputBytes = 4096
			})
			queries := []ownership.Query{doiQuery("10.1000/one")}

			start := time.Now()
			result := ownership.Aggregate(context.Background(), []ownership.Provider{provider}, queries)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("lookup took %v; the run must end at its timeout or output cap", elapsed)
			}
			if result.Complete() {
				t.Fatalf("result = %+v, want incomplete", result)
			}
			health := result.Sources[0]
			if health.FailureCode != tc.want {
				t.Fatalf("FailureCode = %q, want %q", health.FailureCode, tc.want)
			}
			if len(result.Works[0].Claims) != 0 {
				t.Fatalf("claims = %+v, want none from a failed first run", result.Works[0].Claims)
			}
		})
	}
}

func TestCommandSourceOutputAtTheCapIsAccepted(t *testing.T) {
	fixture := newCommandFixture(t)
	body := oneEntryBibTeX("10.1000/one")
	provider := fixture.provider(newFakeClock(), func(source *config.LibrarySource) {
		source.MaxOutputBytes = int64(len(body))
	})
	if _, health := provider.Lookup(context.Background(), nil); !health.Complete {
		t.Fatalf("health = %+v, want complete at exactly the cap", health)
	}

	over := fixture.provider(newFakeClock(), func(source *config.LibrarySource) {
		source.MaxOutputBytes = int64(len(body)) - 1
	})
	if _, health := over.Lookup(context.Background(), nil); health.Complete || health.FailureCode != ownership.FailureTruncated {
		t.Fatalf("health = %+v, want truncated one byte past the cap", health)
	}
}

func TestCommandSourceFailedRefreshKeepsOnlyBoundedStaleAnnotation(t *testing.T) {
	fixture := newCommandFixture(t)
	clock := newFakeClock()
	provider := fixture.provider(clock, func(source *config.LibrarySource) { source.RefreshSeconds = 60 })
	queries := []ownership.Query{doiQuery("10.1000/one")}
	if _, health := provider.Lookup(context.Background(), queries); !health.Complete {
		t.Fatalf("initial run failed: %+v", health)
	}
	firstSuccess := clock.Now()

	fixture.setMode("exit")
	clock.Advance(time.Minute)
	claims, health := provider.Lookup(context.Background(), queries)
	if health.Complete || health.FailureCode != ownership.FailureExit {
		t.Fatalf("health = %+v, want incomplete exit failure", health)
	}
	if !health.Stale || len(claims[0]) != 1 || !claims[0][0].Stale {
		t.Fatalf("claims = %+v, health = %+v, want the last good claim as stale annotation", claims[0], health)
	}
	if got := ownership.Decide(queries[0], ownership.WorkResult{Claims: claims[0]}); got.Suppress {
		t.Fatal("a failed refresh must not suppress a requested acquisition")
	}
	if !health.LastSuccess.Equal(firstSuccess) {
		t.Fatalf("LastSuccess = %v, want %v", health.LastSuccess, firstSuccess)
	}

	// A failing command is not rerun on every lookup.
	_, health = provider.Lookup(context.Background(), queries)
	if health.Complete || health.FailureCode != ownership.FailureExit {
		t.Fatalf("health = %+v, want the remembered failure", health)
	}
	if got := fixture.runs(); got != 2 {
		t.Fatalf("runs = %d, want 2: the retry backoff must hold", got)
	}

	// Past the refresh interval plus the freshness window the old index is
	// no longer evidence of anything.
	clock.Advance(freshnessWindow + time.Second)
	claims, health = provider.Lookup(context.Background(), queries)
	if health.Complete || len(claims[0]) != 0 {
		t.Fatalf("claims = %+v, health = %+v, want no claims from an expired index", claims[0], health)
	}
	if !health.LastSuccess.Equal(firstSuccess) {
		t.Fatalf("LastSuccess = %v, want it kept for diagnostics", health.LastSuccess)
	}

	fixture.setMode("ok")
	clock.Advance(commandRetryBackoff)
	claims, health = provider.Lookup(context.Background(), queries)
	if !health.Complete || health.Stale || len(claims[0]) != 1 || claims[0][0].Stale {
		t.Fatalf("claims = %+v, health = %+v, want a fresh complete read once the command recovers", claims[0], health)
	}
}

func TestNewProviderRejectsUnusableCommandSource(t *testing.T) {
	cases := []struct {
		name   string
		source config.LibrarySource
	}{
		{"missing argv", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Claim: config.LibraryClaimPDFPresent}},
		{"blank program", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Argv: []string{" "}, Claim: config.LibraryClaimPDFPresent}},
		{"bare program name", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Argv: []string{"cat"}, Claim: config.LibraryClaimPDFPresent}},
		{"path beside argv", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Argv: []string{"/bin/cat"}, Path: "/tmp/x.bib", Claim: config.LibraryClaimPDFPresent}},
		{"timeout out of range", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Argv: []string{"/bin/cat"}, TimeoutSeconds: -1, Claim: config.LibraryClaimPDFPresent}},
		{"missing claim", config.LibrarySource{Name: "n", Kind: config.LibraryKindCommand, Argv: []string{"/bin/cat"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewProvider(tc.source, time.Now); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestEnumerateLibraryRecordsRunsCommandSource(t *testing.T) {
	fixture := newCommandFixture(t)
	source := config.LibrarySource{
		Name:           "papis-live",
		Kind:           config.LibraryKindCommand,
		Argv:           []string{"/bin/sh", fixture.script},
		Claim:          config.LibraryClaimPDFPresent,
		TimeoutSeconds: 1,
	}
	records, err := EnumerateLibraryRecords(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].DOI != "10.1000/one" {
		t.Fatalf("records = %+v, want 10.1000/one", records)
	}

	fixture.setMode("exit")
	if records, err := EnumerateLibraryRecords(context.Background(), source); err == nil {
		t.Fatalf("records = %+v, want an error from a failing command", records)
	}
}
