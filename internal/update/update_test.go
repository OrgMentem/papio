// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCheckFetchesFreshReleaseAndCachesETag(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/releases/latest" || r.URL.RawQuery != "" {
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("Accept = %q", got)
		}
		w.Header().Set("ETag", `"release-1"`)
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://github.com/orgmentem/papio/releases/tag/v1.2.3"}`))
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL + "/releases/latest", Client: server.Client(), Now: func() time.Time { return now }})
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || info.URL != "https://github.com/orgmentem/papio/releases/tag/v1.2.3" || !info.CheckedAt.Equal(now) {
		t.Fatalf("info = %#v", info)
	}
	cached := checker.readCache()
	if cached.ETag != `"release-1"` || cached.LatestVersion != "1.2.3" {
		t.Fatalf("cache = %#v", cached)
	}
}

func TestCheckReusesCacheAfterNotModified(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `"release-1"` {
			t.Fatalf("If-None-Match = %q", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cache{ETag: `"release-1"`, LatestVersion: "1.2.3", URL: "https://example.test/release", CheckedAt: now.Add(-checkEvery)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || !info.CheckedAt.Equal(now) {
		t.Fatalf("info = %#v", info)
	}
	if got := checker.readCache().CheckedAt; !got.Equal(now) {
		t.Fatalf("cached checked_at = %v, want %v", got, now)
	}
}

func TestCheckSkipsNetworkForFreshCache(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		t.Fatal("fresh cache made a network request")
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cache{LatestVersion: "1.2.3", URL: "https://example.test/release", CheckedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || calls.Load() != 0 {
		t.Fatalf("info = %#v; calls = %d", info, calls.Load())
	}
}

func TestCheckMalformedResponseSoftFailsToCachedInfo(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":`))
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cache{LatestVersion: "1.2.2", URL: "https://example.test/release", CheckedAt: now.Add(-checkEvery)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.2" || !info.CheckedAt.Equal(now.Add(-checkEvery)) {
		t.Fatalf("info = %#v", info)
	}
}

func TestCachedReturnsWhileRefreshIsInFlight(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
	})}
	checker := NewWithOptions(Options{
		DataDir: t.TempDir(), ReleasesURL: "https://example.test/releases", Client: client,
		Now: func() time.Time { return now },
	})
	if err := checker.writeCache(cache{
		LatestVersion: "1.2.3",
		URL:           "https://example.test/releases/v1.2.3",
		CheckedAt:     now.Add(-checkEvery),
	}); err != nil {
		t.Fatal(err)
	}

	checkDone := make(chan struct{})
	checkErr := make(chan error, 1)
	go func() {
		_, err := checker.Check(context.Background())
		checkErr <- err
		close(checkDone)
	}()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-checkDone:
		case <-time.After(time.Second):
			t.Error("Check did not finish after releasing fake client")
		}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Check did not reach fake client")
	}

	cached := make(chan *Info, 1)
	go func() {
		cached <- checker.Cached()
	}()
	select {
	case info := <-cached:
		if info == nil || info.LatestVersion != "1.2.3" {
			t.Fatalf("Cached() = %#v", info)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Cached blocked on refresh request")
	}

	close(release)
	select {
	case <-checkDone:
		if err := <-checkErr; err != nil {
			t.Fatalf("Check: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Check did not return after fake client was released")
	}
}

func TestTryMarkNaggedOncePerDay(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	checker := New(t.TempDir())
	if !checker.TryMarkNagged(now) {
		t.Fatal("first nag was not recorded")
	}
	if checker.TryMarkNagged(now.Add(time.Hour)) {
		t.Fatal("second nag within a day was recorded")
	}
	if !checker.TryMarkNagged(now.Add(checkEvery)) {
		t.Fatal("nag after a day was not recorded")
	}
}

func TestVersionAndUpgradeHints(t *testing.T) {
	if !IsNewer("v1.2.3", "1.2.2") || IsNewer("1.2.3", "1.2.3") {
		t.Fatal("unexpected version comparison")
	}
	if got := UpgradeHint("/opt/homebrew/bin/papio", "https://example.test/releases"); got != "brew upgrade papio" {
		t.Fatalf("homebrew hint = %q", got)
	}
	if got := UpgradeHint("/Applications/papio", "https://example.test/releases"); got != "https://example.test/releases" {
		t.Fatalf("release hint = %q", got)
	}
	if got := UpgradeHint(`C:\Users\x\scoop\apps\papio\current\papio.exe`, "https://example.test/releases"); got != "scoop update papio" {
		t.Fatalf("scoop hint = %q", got)
	}
}

func TestTargetsUseSeparateCachesAndRateLimits(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	var papioCalls, zotioCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/papio":
			papioCalls.Add(1)
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://example.test/papio"}`))
		case "/zotio":
			zotioCalls.Add(1)
			_, _ = w.Write([]byte(`{"tag_name":"v4.5.6","html_url":"https://example.test/zotio"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dataDir := t.TempDir()
	papio := NewWithOptions(Options{
		DataDir: dataDir, ReleasesURL: server.URL + "/papio", CacheName: cacheName,
		Client: server.Client(), Now: func() time.Time { return now },
	})
	zotio := NewWithOptions(Options{
		DataDir: dataDir, ReleasesURL: server.URL + "/zotio", CacheName: zotioCacheName,
		Client: server.Client(), Now: func() time.Time { return now },
	})
	for _, checker := range []*Checker{papio, zotio} {
		if _, err := checker.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
		if _, err := checker.Check(context.Background()); err != nil {
			t.Fatalf("cached Check: %v", err)
		}
	}
	if papioCalls.Load() != 1 || zotioCalls.Load() != 1 {
		t.Fatalf("calls papio=%d zotio=%d", papioCalls.Load(), zotioCalls.Load())
	}
	if papio.cachePath() == zotio.cachePath() {
		t.Fatalf("targets share cache path %q", papio.cachePath())
	}
}

func TestZotioCheckerPersistsInstalledVersionWithoutChecking(t *testing.T) {
	checker := NewZotio(t.TempDir())
	checker.RememberInstalledVersion(" 1.2.3 ")
	if got := checker.InstalledVersion(); got != "1.2.3" {
		t.Fatalf("installed version = %q", got)
	}
	if info := checker.Cached(); info != nil {
		t.Fatalf("cached release = %#v", info)
	}
}
