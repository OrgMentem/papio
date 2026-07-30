// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ownershipsnapshot

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/ownership"
)

const twoEntryBibTeX = `@article{a,
  title = {First Paper},
  doi = {10.1000/one},
}

@article{b,
  title = {Second Paper},
  doi = {10.1000/two},
  eprint = {2401.00002},
  archiveprefix = {arXiv},
}
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestProvider(t *testing.T, path, claim string, now func() time.Time) *fileProvider {
	t.Helper()
	provider, err := NewProvider(config.LibrarySource{
		Name:   "papis",
		Kind:   config.LibraryKindFile,
		Path:   path,
		Format: "bibtex",
		Claim:  claim,
	}, now)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return provider.(*fileProvider)
}

func doiQuery(doi string) ownership.Query {
	return ownership.Query{Identifiers: []ownership.Identifier{{Kind: ownership.KindDOI, Value: doi}}}
}

func oneEntryBibTeX(doi string) string {
	return "@article{one,\n  title = {Paper},\n  doi = {" + doi + "},\n}\n"
}

func sourceRecordsWithOneDOI(count int) string {
	var records strings.Builder
	for i := 0; i < count; i++ {
		records.WriteString("@article{k")
		records.WriteString(string(rune('a' + i%26)))
		records.WriteString(string(rune('a' + i/26)))
		records.WriteString(",\n  title = {Paper},")
		if i == 0 {
			records.WriteString("\n  doi = {10.1000/one},")
		}
		records.WriteString("\n}\n\n")
	}
	return records.String()
}

func TestNewProviderRejectsUnsupportedConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		source config.LibrarySource
	}{
		{"missing name", config.LibrarySource{Kind: config.LibraryKindFile, Path: "x.bib", Claim: config.LibraryClaimPDFPresent}},
		{"unsupported kind", config.LibrarySource{Name: "n", Kind: "command", Path: "x.bib", Claim: config.LibraryClaimPDFPresent}},
		{"missing path", config.LibrarySource{Name: "n", Kind: config.LibraryKindFile, Claim: config.LibraryClaimPDFPresent}},
		{"missing claim", config.LibrarySource{Name: "n", Kind: config.LibraryKindFile, Path: "x.bib"}},
		{"unknown claim", config.LibrarySource{Name: "n", Kind: config.LibraryKindFile, Path: "x.bib", Claim: "maybe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewProvider(tc.source, time.Now); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewProviderExpandsHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}
	provider, err := NewProvider(config.LibrarySource{
		Name:  "papis",
		Kind:  config.LibraryKindFile,
		Path:  "~/papers.bib",
		Claim: config.LibraryClaimPDFPresent,
	}, time.Now)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got, want := provider.(*fileProvider).path, filepath.Join(home, "papers.bib"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestLookupMatchesAndCachesByRevision(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)

	queries := []ownership.Query{doiQuery("10.1000/one"), doiQuery("10.9999/absent")}
	claims, health := provider.Lookup(context.Background(), queries)
	if !health.Complete {
		t.Fatalf("health = %+v, want complete", health)
	}
	if health.EntryCount != 2 {
		t.Fatalf("EntryCount = %d, want 2", health.EntryCount)
	}
	if len(claims[0]) != 1 || claims[0][0].Artifact != ownership.ArtifactPresent {
		t.Fatalf("claims[0] = %+v", claims[0])
	}
	if len(claims[1]) != 0 {
		t.Fatalf("a miss must produce no claim, got %+v", claims[1])
	}
	if got := ownership.Decide(queries[0], ownership.WorkResult{Claims: claims[0]}); !got.Suppress {
		t.Fatalf("decision = %+v, want suppression", got)
	}

	// An unchanged file must not be re-read: search, batch, and watches all hit
	// this provider and a per-lookup read would stampede the file.
	if _, _ = provider.Lookup(context.Background(), queries); provider.reads != 1 {
		t.Fatalf("reads = %d, want 1 across two lookups", provider.reads)
	}
}

func TestLookupRefreshesWhenTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	queries := []ownership.Query{doiQuery("10.1000/three")}

	if claims, _ := provider.Lookup(context.Background(), queries); len(claims[0]) != 0 {
		t.Fatalf("unexpected claim before the entry existed: %+v", claims[0])
	}

	body := twoEntryBibTeX + "\n@article{c,\n  title = {Third Paper},\n  doi = {10.1000/three},\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime: a same-second rewrite of a different size already
	// changes the fingerprint, but be explicit so the test cannot flake.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	claims, health := provider.Lookup(context.Background(), queries)
	if !health.Complete || health.EntryCount != 3 {
		t.Fatalf("health = %+v, want complete with 3 entries", health)
	}
	if len(claims[0]) != 1 {
		t.Fatalf("claims after refresh = %+v", claims[0])
	}
}

// The central invariant: an unreadable source is incomplete, never "holds
// nothing". Callers keep the right to re-acquire.
func TestMissingFileIsIncompleteNotEmpty(t *testing.T) {
	dir := t.TempDir()
	provider := newTestProvider(t, filepath.Join(dir, "absent.bib"), config.LibraryClaimPDFPresent, time.Now)
	claims, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if health.Complete {
		t.Fatal("a missing file must not report a complete read")
	}
	if health.FailureCode != ownership.FailureUnreadable {
		t.Fatalf("FailureCode = %q, want %q", health.FailureCode, ownership.FailureUnreadable)
	}
	if len(claims[0]) != 0 {
		t.Fatalf("claims = %+v", claims[0])
	}
}

// A previously loaded index keeps answering after the file goes away: positive
// evidence survives a transient read failure, or one bad stat becomes a
// duplicate download.
func TestLastKnownGoodIndexKeepsAnswering(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	clock := time.Now()
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, func() time.Time { return clock })
	queries := []ownership.Query{doiQuery("10.1000/one")}
	if _, health := provider.Lookup(context.Background(), queries); !health.Complete {
		t.Fatalf("initial load failed: %+v", health)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	claims, health := provider.Lookup(context.Background(), queries)
	if health.Complete {
		t.Fatal("the source must report incomplete once unreadable")
	}
	if len(claims[0]) != 1 {
		t.Fatalf("last-known-good claims lost: %+v", claims[0])
	}
	if !health.Stale || !claims[0][0].Stale {
		t.Fatal("a failed refresh must retain only stale annotation")
	}
	if got := ownership.Decide(queries[0], ownership.WorkResult{Claims: claims[0]}); got.Suppress {
		t.Fatal("a failed refresh must not suppress a requested acquisition")
	}

	// Past the freshness window the same claim may annotate but not suppress.
	clock = clock.Add(freshnessWindow + time.Minute)
	claims, health = provider.Lookup(context.Background(), queries)
	if !health.Stale {
		t.Fatalf("health = %+v, want stale", health)
	}
	if len(claims[0]) != 1 || !claims[0][0].Stale {
		t.Fatalf("claims = %+v, want one stale claim", claims[0])
	}
	if got := ownership.Decide(queries[0], ownership.WorkResult{Claims: claims[0]}); got.Suppress {
		t.Fatal("a stale claim must not suppress acquisition")
	}
}

// A valid empty export is a real state and must stay distinguishable from an
// unreadable one.
func TestEmptyButValidSourceIsComplete(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", "")
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if !health.Complete {
		t.Fatalf("health = %+v, want complete", health)
	}
	if health.EntryCount != 0 {
		t.Fatalf("EntryCount = %d, want 0", health.EntryCount)
	}
	if health.FailureCode != "" {
		t.Fatalf("FailureCode = %q, want empty", health.FailureCode)
	}
}

func TestOversizedSourceIsRefusedRatherThanPartlyRead(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	provider.maxBytes = 4

	_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if health.Complete {
		t.Fatal("an oversized source must not report a complete read")
	}
	if health.FailureCode != ownership.FailureTruncated {
		t.Fatalf("FailureCode = %q, want %q", health.FailureCode, ownership.FailureTruncated)
	}
	if provider.reads != 0 {
		t.Fatalf("reads = %d, want 0 — the cap is checked before reading", provider.reads)
	}
}

func TestMalformedSourceRetainsThePriorIndex(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	queries := []ownership.Query{doiQuery("10.1000/one")}
	if _, health := provider.Lookup(context.Background(), queries); !health.Complete {
		t.Fatal("initial load failed")
	}

	if err := os.WriteFile(path, []byte("@article{broken,\n title = {unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	claims, health := provider.Lookup(context.Background(), queries)
	if health.Complete {
		t.Fatal("a parse failure must not report a complete read")
	}
	if health.FailureCode != ownership.FailureParse {
		t.Fatalf("FailureCode = %q, want %q", health.FailureCode, ownership.FailureParse)
	}
	if len(claims[0]) != 1 {
		t.Fatalf("prior index lost on parse failure: %+v", claims[0])
	}
}

// A truncated write parses cleanly and simply contains less; accepting it would
// quietly disable de-duplication for the whole library.
func TestCatastrophicCountCollapseIsRejected(t *testing.T) {
	dir := t.TempDir()
	var large strings.Builder
	for i := 0; i < 40; i++ {
		large.WriteString("@article{k")
		large.WriteString(string(rune('a' + i%26)))
		large.WriteString(string(rune('a' + i/26)))
		large.WriteString(",\n  title = {Paper},\n  doi = {10.1000/n")
		large.WriteString(string(rune('a' + i%26)))
		large.WriteString(string(rune('a' + i/26)))
		large.WriteString("},\n}\n\n")
	}
	path := writeFile(t, dir, "refs.bib", large.String())
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	queries := []ownership.Query{doiQuery("10.1000/naa")}
	_, health := provider.Lookup(context.Background(), queries)
	if !health.Complete || health.EntryCount < collapseFloor {
		t.Fatalf("initial load = %+v, want a complete load above the collapse floor", health)
	}

	if err := os.WriteFile(path, []byte("@article{one,\n  title = {Paper},\n  doi = {10.1000/naa},\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	claims, health := provider.Lookup(context.Background(), queries)
	if health.Complete {
		t.Fatal("a collapsed entry count must not report a complete read")
	}
	if health.FailureCode != ownership.FailureCountCollapse {
		t.Fatalf("FailureCode = %q, want %q", health.FailureCode, ownership.FailureCountCollapse)
	}
	if len(claims[0]) != 1 {
		t.Fatalf("prior index lost on collapse: %+v", claims[0])
	}
}

// record_present is the citations-only declaration: it may annotate a search
// result but must never suppress an acquisition, because a citation without a
// PDF is exactly what a backfill user wants acquired.
func TestRecordPresentClaimDoesNotSuppress(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimRecordPresent, time.Now)
	query := doiQuery("10.1000/one")

	claims, health := provider.Lookup(context.Background(), []ownership.Query{query})
	if !health.Complete {
		t.Fatalf("health = %+v", health)
	}
	if len(claims[0]) != 1 || claims[0][0].Artifact != ownership.ArtifactUnknown {
		t.Fatalf("claims = %+v, want one unknown-artifact claim", claims[0])
	}
	got := ownership.Decide(query, ownership.WorkResult{Claims: claims[0]})
	if got.Suppress {
		t.Fatal("a record_present source must not suppress acquisition")
	}
	if !got.RecordPresent {
		t.Fatal("a record_present source must still annotate the record as known")
	}
}

// A bibliographic export cannot say which manifestation it holds, so it must not
// answer a request that asked for a specific one.
func TestSnapshotCannotSatisfyAnExplicitPublishedRequest(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	query := doiQuery("10.1000/one")
	query.DesiredVersion = ownership.VersionPublished

	claims, _ := provider.Lookup(context.Background(), []ownership.Query{query})
	if len(claims[0]) != 1 {
		t.Fatalf("claims = %+v", claims[0])
	}
	if got := ownership.Decide(query, ownership.WorkResult{Claims: claims[0]}); got.Suppress {
		t.Fatal("an unknown held version must not satisfy an explicit published request")
	}
}

func TestArXivIdentifierFromBibTeXIsIndexed(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	query := ownership.Query{Identifiers: []ownership.Identifier{{Kind: ownership.KindArXiv, Value: "arXiv:2401.00002v1"}}}
	claims, _ := provider.Lookup(context.Background(), []ownership.Query{query})
	if len(claims[0]) != 1 {
		t.Fatalf("claims = %+v, want the arXiv entry matched across a version suffix", claims[0])
	}
}

func TestFormatIsDetectedWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider, err := NewProvider(config.LibrarySource{
		Name:  "papis",
		Kind:  config.LibraryKindFile,
		Path:  path,
		Claim: config.LibraryClaimPDFPresent,
	}, time.Now)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if !health.Complete || health.EntryCount != 2 {
		t.Fatalf("health = %+v, want a complete detected-format load", health)
	}
}

func TestConcurrentLookupsReadOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", twoEntryBibTeX)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	queries := []ownership.Query{doiQuery("10.1000/one")}

	reading := make(chan struct{})
	release := make(chan struct{})
	provider.read = func(ctx context.Context, reader io.Reader) ([]byte, error) {
		close(reading)
		select {
		case <-release:
			return readBounded(ctx, reader)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	results := make(chan ownership.SourceHealth, 9)
	go func() {
		_, health := provider.Lookup(context.Background(), queries)
		results <- health
	}()
	<-reading

	var callers sync.WaitGroup
	for i := 0; i < 8; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			_, health := provider.Lookup(context.Background(), queries)
			results <- health
		}()
	}

	provider.mu.RLock()
	reads := provider.reads
	provider.mu.RUnlock()
	if reads != 1 {
		t.Fatalf("reads while one refresh is blocked = %d, want 1", reads)
	}
	close(release)
	callers.Wait()
	for i := 0; i < 9; i++ {
		if health := <-results; !health.Complete {
			t.Fatalf("health = %+v, want complete", health)
		}
	}

	provider.mu.RLock()
	reads = provider.reads
	provider.mu.RUnlock()
	if reads != 1 {
		t.Fatalf("reads = %d, want 1 for nine synchronized lookups", reads)
	}
}

func TestGrowthDuringReadIsBoundedAndRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", "x")
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	provider.maxBytes = 4

	var bytesRead int
	provider.read = func(ctx context.Context, reader io.Reader) ([]byte, error) {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0o600); err != nil {
			return nil, err
		}
		data, err := readBounded(ctx, reader)
		bytesRead = len(data)
		return data, err
	}

	_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if health.Complete || health.FailureCode != ownership.FailureTruncated {
		t.Fatalf("health = %+v, want bounded truncated failure", health)
	}
	if bytesRead != int(provider.maxBytes+1) {
		t.Fatalf("bytes read = %d, want %d", bytesRead, provider.maxBytes+1)
	}
}

func TestNonRegularSourceIsUnreadableWithoutReading(t *testing.T) {
	provider := newTestProvider(t, t.TempDir(), config.LibraryClaimPDFPresent, time.Now)
	read := false
	provider.read = func(context.Context, io.Reader) ([]byte, error) {
		read = true
		return nil, nil
	}

	_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
	if health.Complete || health.FailureCode != ownership.FailureUnreadable {
		t.Fatalf("health = %+v, want unreadable non-regular source", health)
	}
	if read {
		t.Fatal("a non-regular source must be rejected before reading")
	}
}

func TestFIFOIsRejectedWithoutWaitingForAWriter(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("nonblocking FIFO open is implemented on Darwin and Linux")
	}
	path := filepath.Join(t.TempDir(), "refs.bib")
	if output, err := exec.Command("mkfifo", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, output)
	}
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	read := false
	provider.read = func(context.Context, io.Reader) ([]byte, error) {
		read = true
		return nil, io.ErrUnexpectedEOF
	}

	result := make(chan ownership.SourceHealth, 1)
	go func() {
		_, health := provider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")})
		result <- health
	}()
	select {
	case health := <-result:
		if health.Complete || health.FailureCode != ownership.FailureUnreadable {
			t.Fatalf("health = %+v, want unreadable FIFO", health)
		}
		if read {
			t.Fatal("a FIFO must be rejected before reading")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO lookup waited for a writer")
	}
}

func TestSameSizeSameMtimeReplacementAndRewriteRefresh(t *testing.T) {
	dir := t.TempDir()
	one := oneEntryBibTeX("10.1000/one")
	two := oneEntryBibTeX("10.1000/two")
	if len(one) != len(two) {
		t.Fatalf("fixture sizes differ: %d and %d", len(one), len(two))
	}
	path := writeFile(t, dir, "refs.bib", one)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	oneQuery := doiQuery("10.1000/one")
	twoQuery := doiQuery("10.1000/two")
	if _, health := provider.Lookup(context.Background(), []ownership.Query{oneQuery}); !health.Complete {
		t.Fatalf("initial health = %+v", health)
	}

	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := writeFile(t, dir, "replacement.bib", two)
	if err := os.Chtimes(replacement, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	claims, health := provider.Lookup(context.Background(), []ownership.Query{oneQuery, twoQuery})
	if !health.Complete || len(claims[0]) != 0 || len(claims[1]) != 1 {
		t.Fatalf("claims after same-size replacement = %+v, health = %+v", claims, health)
	}

	replaced, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(one), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, replaced.ModTime(), replaced.ModTime()); err != nil {
		t.Fatal(err)
	}
	claims, health = provider.Lookup(context.Background(), []ownership.Query{oneQuery, twoQuery})
	if !health.Complete || len(claims[0]) != 1 || len(claims[1]) != 0 {
		t.Fatalf("claims after same-size rewrite = %+v, health = %+v", claims, health)
	}
	if provider.reads != 3 {
		t.Fatalf("reads = %d, want 3 across initial, replacement, and rewrite", provider.reads)
	}
}

// The identity seam makes this metadata-less-platform behavior testable on
// every GOOS. A replacement between the original read and pathname validation
// must leave the prior snapshot stale rather than publishing unlinked bytes as
// current evidence.
func TestUnknownIdentityRejectsInFlightSameRevisionReplacement(t *testing.T) {
	dir := t.TempDir()
	one := oneEntryBibTeX("10.1000/one")
	two := oneEntryBibTeX("10.1000/two")
	if len(one) != len(two) {
		t.Fatalf("fixture sizes differ: %d and %d", len(one), len(two))
	}
	path := writeFile(t, dir, "refs.bib", one)
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	provider.identity = func(*os.File, os.FileInfo) fileID { return fileID{} }
	oneQuery := doiQuery("10.1000/one")
	twoQuery := doiQuery("10.1000/two")
	if _, health := provider.Lookup(context.Background(), []ownership.Query{oneQuery}); !health.Complete {
		t.Fatalf("initial health = %+v", health)
	}

	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := writeFile(t, dir, "replacement.bib", two)
	if err := os.Chtimes(replacement, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if replacementInfo.Size() != original.Size() || !replacementInfo.ModTime().Equal(original.ModTime()) {
		t.Fatalf("replacement revision = size %d, mtime %v; want size %d, mtime %v", replacementInfo.Size(), replacementInfo.ModTime(), original.Size(), original.ModTime())
	}

	provider.read = func(ctx context.Context, reader io.Reader) ([]byte, error) {
		data, err := readBounded(ctx, reader)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(replacement, path); err != nil {
			return nil, err
		}
		return data, nil
	}
	claims, health := provider.Lookup(context.Background(), []ownership.Query{oneQuery, twoQuery})
	if health.Complete || health.FailureCode != ownership.FailureUnreadable || !health.Stale {
		t.Fatalf("health after in-flight replacement = %+v", health)
	}
	if len(claims[0]) != 1 || !claims[0][0].Stale || len(claims[1]) != 0 {
		t.Fatalf("claims after in-flight replacement = %+v", claims)
	}
	if decision := ownership.Decide(oneQuery, ownership.WorkResult{Claims: claims[0]}); decision.Suppress {
		t.Fatal("unlinked old bytes must never publish a fresh suppressing claim")
	}

	provider.read = readBounded
	claims, health = provider.Lookup(context.Background(), []ownership.Query{oneQuery, twoQuery})
	if !health.Complete || len(claims[0]) != 0 || len(claims[1]) != 1 {
		t.Fatalf("claims after replacement retry = %+v, health = %+v", claims, health)
	}
}

func TestSourceRecordCountDrivesCollapseForProviderLifetime(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", sourceRecordsWithOneDOI(40))
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	query := doiQuery("10.1000/one")

	_, health := provider.Lookup(context.Background(), []ownership.Query{query})
	if !health.Complete || health.EntryCount != 40 {
		t.Fatalf("initial health = %+v, want 40 source records", health)
	}
	if provider.current.index.Len() != 1 {
		t.Fatalf("matchable index length = %d, want 1", provider.current.index.Len())
	}

	if err := os.WriteFile(path, []byte(oneEntryBibTeX("10.1000/one")), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		claims, health := provider.Lookup(context.Background(), []ownership.Query{query})
		if health.Complete || health.FailureCode != ownership.FailureCountCollapse {
			t.Fatalf("collapse attempt %d health = %+v", attempt, health)
		}
		if len(claims[0]) != 1 || !claims[0][0].Stale {
			t.Fatalf("collapse attempt %d claims = %+v", attempt, claims[0])
		}
	}
}

func TestCancelledLookupReturnsWhileRefreshIsBlocked(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "refs.bib", oneEntryBibTeX("10.1000/one"))
	provider := newTestProvider(t, path, config.LibraryClaimPDFPresent, time.Now)
	query := doiQuery("10.1000/one")
	if _, health := provider.Lookup(context.Background(), []ownership.Query{query}); !health.Complete {
		t.Fatalf("initial health = %+v", health)
	}
	if err := os.WriteFile(path, []byte(oneEntryBibTeX("10.1000/two")), 0o600); err != nil {
		t.Fatal(err)
	}

	reading := make(chan struct{})
	release := make(chan struct{})
	provider.read = func(ctx context.Context, reader io.Reader) ([]byte, error) {
		close(reading)
		select {
		case <-release:
			return readBounded(ctx, reader)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	first := make(chan ownership.SourceHealth, 1)
	go func() {
		_, health := provider.Lookup(context.Background(), []ownership.Query{query})
		first <- health
	}()
	<-reading

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		claims [][]ownership.Claim
		health ownership.SourceHealth
	}
	cancelled := make(chan result, 1)
	go func() {
		claims, health := provider.Lookup(ctx, []ownership.Query{query})
		cancelled <- result{claims: claims, health: health}
	}()
	cancel()

	select {
	case got := <-cancelled:
		if got.health.Complete || got.health.FailureCode != ownership.FailureTimeout || !got.health.Stale {
			t.Fatalf("cancelled health = %+v", got.health)
		}
		if len(got.claims[0]) != 1 || !got.claims[0][0].Stale {
			t.Fatalf("cancelled claims = %+v", got.claims)
		}
		if decision := ownership.Decide(query, ownership.WorkResult{Claims: got.claims[0]}); decision.Suppress {
			t.Fatal("a cancelled lookup must not suppress acquisition")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled lookup waited for the active refresh")
	}

	close(release)
	if health := <-first; !health.Complete {
		t.Fatalf("active refresh health = %+v", health)
	}
}
