// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package update checks public papio-family release feeds at most once a day.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	releasesURL      = "https://api.github.com/repos/orgmentem/papio/releases/latest"
	ZotioReleasesURL = "https://api.github.com/repos/OrgMentem/zotio/releases/latest"
	cacheName        = "update-cache.json"
	zotioCacheName   = "update-cache-zotio.json"
	checkEvery       = 24 * time.Hour
	// maxReleaseBody bounds the release payload the same way every resolver
	// bounds an upstream response body.
	maxReleaseBody = 1 << 20
	// cacheLockRetry and cacheLockWait bound the wait for the cross-process
	// cache lock. Every critical section is one small file read plus one
	// atomic rename, so a healthy holder releases the lock almost immediately.
	cacheLockRetry = 5 * time.Millisecond
	cacheLockWait  = 2 * time.Second
)

// Info describes the latest release known to a checker.
type Info struct {
	LatestVersion string
	URL           string
	CheckedAt     time.Time
}

// Options configures a Checker. ReleasesURL, Client, and Now primarily make
// the checker deterministic in tests; production callers normally use New.
type Options struct {
	DataDir     string
	ReleasesURL string
	Client      *http.Client
	Now         func() time.Time
	CacheName   string
}

// Checker caches the release metadata in the papio data directory.
type Checker struct {
	dataDir     string
	cacheName   string
	releasesURL string
	client      *http.Client
	now         func() time.Time
	// mu guards cache file access inside this process. It is never held while
	// a refresh is in flight. Cross-process coordination needs the advisory
	// file lock in the data directory as well; see updateCache.
	mu sync.Mutex
	// refreshMu serializes stale-cache refreshes, including the HTTP request.
	// Check takes refreshMu only after releasing mu.
	refreshMu sync.Mutex
}

type cache struct {
	ETag          string    `json:"etag,omitempty"`
	LatestVersion string    `json:"latest_version,omitempty"`
	URL           string    `json:"url,omitempty"`
	CheckedAt     time.Time `json:"checked_at,omitempty"`
	// LastAttemptAt records every completed check, successful or not, so an
	// endpoint outage defers the next request for the full cadence instead of
	// re-issuing one on every invocation. It is written without disturbing the
	// previously cached release metadata, and an older cache file that lacks
	// the field simply reads as zero.
	LastAttemptAt    time.Time `json:"last_attempt_at,omitempty"`
	LastNaggedAt     time.Time `json:"last_nagged_at,omitempty"`
	InstalledVersion string    `json:"installed_version,omitempty"`
}

type release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// New creates a checker that uses the public GitHub releases endpoint.
func New(dataDir string) *Checker {
	return NewWithOptions(Options{DataDir: dataDir})
}

// NewZotio creates a checker for the public Zotio releases endpoint. Its
// metadata and rate limit are independent from papio's cache.
func NewZotio(dataDir string) *Checker {
	return NewWithOptions(Options{
		DataDir:     dataDir,
		ReleasesURL: ZotioReleasesURL,
		CacheName:   zotioCacheName,
	})
}

// NewWithOptions creates a checker with explicit transport and clock seams.
func NewWithOptions(options Options) *Checker {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	endpoint := options.ReleasesURL
	if endpoint == "" {
		endpoint = releasesURL
	}
	cacheFile := options.CacheName
	if cacheFile == "" {
		cacheFile = cacheName
	}
	return &Checker{
		dataDir:     options.DataDir,
		cacheName:   cacheFile,
		releasesURL: endpoint,
		client:      client,
		now:         now,
	}
}

// Check returns cached release metadata or refreshes it with one GET. Network,
// cache, and decoding failures are intentionally soft: the caller receives any
// previously cached result, or nil.
func (c *Checker) Check(ctx context.Context) *Info {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	cached := c.readCache()
	now := c.now().UTC()
	if cached.fresh(now) {
		info := cached.info()
		c.mu.Unlock()
		return info
	}
	c.mu.Unlock()

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Another Check may have refreshed the cache before this one obtained the
	// refresh lock, so avoid issuing a duplicate request.
	c.mu.Lock()
	cached = c.readCache()
	now = c.now().UTC()
	if cached.fresh(now) {
		info := cached.info()
		c.mu.Unlock()
		return info
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.releasesURL, nil)
	if err != nil {
		return cached.info()
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	response, err := c.client.Do(req)
	if err != nil {
		c.recordAttempt(now)
		return cached.info()
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		merged, err := c.updateCache(func(cached *cache) bool {
			cached.CheckedAt = now
			cached.LastAttemptAt = now
			return true
		})
		if err != nil {
			cached.CheckedAt = now
			return cached.info()
		}
		return merged.info()
	}
	if response.StatusCode != http.StatusOK {
		c.recordAttempt(now)
		return cached.info()
	}

	var latest release
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseBody)).Decode(&latest); err != nil {
		c.recordAttempt(now)
		return cached.info()
	}
	latest.TagName = strings.TrimPrefix(latest.TagName, "v")
	if latest.TagName == "" || latest.HTMLURL == "" {
		c.recordAttempt(now)
		return cached.info()
	}
	etag := response.Header.Get("ETag")
	merged, err := c.updateCache(func(cached *cache) bool {
		cached.ETag = etag
		cached.LatestVersion = latest.TagName
		cached.URL = latest.HTMLURL
		cached.CheckedAt = now
		cached.LastAttemptAt = now
		return true
	})
	if err != nil {
		// Persistence is soft: the caller still receives the release this
		// request just fetched.
		return &Info{LatestVersion: latest.TagName, URL: latest.HTMLURL, CheckedAt: now}
	}
	return merged.info()
}

// recordAttempt persists the attempt clock after a check that reached a
// conclusion but produced no usable release, leaving any previously cached
// release metadata untouched.
func (c *Checker) recordAttempt(now time.Time) {
	_, _ = c.updateCache(func(cached *cache) bool {
		cached.LastAttemptAt = now
		return true
	})
}

// TryMarkNagged records a displayed update prompt when none has been displayed
// in the past day. The decision is one cross-process compare-and-set on the
// cache file, so two papio processes that both see an available update yield
// exactly one prompt. A persistence failure suppresses the prompt so the
// caller never turns a missing cache into repeated stderr noise.
func (c *Checker) TryMarkNagged(now time.Time) bool {
	if c == nil {
		return false
	}
	now = now.UTC()
	marked := false
	_, err := c.updateCache(func(cached *cache) bool {
		if checkedRecently(cached.LastNaggedAt, now) {
			return false
		}
		cached.LastNaggedAt = now
		marked = true
		return true
	})
	return marked && err == nil
}

// IsNewer compares the numeric major.minor.patch cores used by papio version
// strings. Pre-release/build suffixes do not affect the daily update nudge.
func IsNewer(latest, current string) bool {
	return compareVersion(latest, current) > 0
}

// UpgradeHint returns papio's channel-appropriate update instruction.
func UpgradeHint(executable, releaseURL string) string {
	return UpgradeHintFor(executable, "papio", releaseURL)
}

// UpgradeHintFor returns the channel-appropriate instruction for a formula.
func UpgradeHintFor(executable, formula, releaseURL string) string {
	clean := filepath.Clean(executable)
	if strings.HasPrefix(clean, "/opt/homebrew/") || strings.HasPrefix(clean, "/usr/local/Cellar/") {
		return "brew upgrade " + formula
	}
	// Scoop installs under <scoop>/apps/<name> and shims under <scoop>/shims.
	// Normalize separators so detection is independent of the host that built
	// the string (Windows uses backslashes).
	if normalized := strings.ToLower(strings.ReplaceAll(clean, `\`, "/")); strings.Contains(normalized, "/scoop/") {
		return "scoop update " + formula
	}
	return releaseURL
}

func compareVersion(left, right string) int {
	parse := func(value string) [3]int {
		var parts [3]int
		value = strings.TrimPrefix(value, "v")
		for i, raw := range strings.SplitN(value, ".", 3) {
			// Drop pre-release/build suffixes ("3-dev", "3+build") so dev
			// builds compare by their numeric core, as documented on IsNewer.
			if cut := strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }); cut >= 0 {
				raw = raw[:cut]
			}
			parts[i], _ = strconv.Atoi(raw)
		}
		return parts
	}
	a, b := parse(left), parse(right)
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func (c *Checker) cachePath() string {
	return filepath.Join(c.dataDir, c.cacheName)
}

func (c *Checker) readCache() cache {
	data, err := os.ReadFile(c.cachePath())
	if err != nil {
		return cache{}
	}
	var cached cache
	if json.Unmarshal(data, &cached) != nil {
		return cache{}
	}
	return cached
}

// CachedState returns release metadata and the last successful local Zotio
// preflight version without making a request or starting a subprocess.
func (c *Checker) CachedState() (*Info, string) {
	if c == nil {
		return nil, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := c.readCache()
	return cached.info(), cached.InstalledVersion
}

// Cached returns release metadata already on disk without making a request.
func (c *Checker) Cached() *Info {
	info, _ := c.CachedState()
	return info
}

// InstalledVersion returns the last Zotio version recorded by a successful
// local preflight. It never runs a subprocess or makes a network request.
func (c *Checker) InstalledVersion() string {
	_, version := c.CachedState()
	return version
}

// RememberInstalledVersion records a successful local Zotio preflight. Cache
// failures are deliberately soft, matching release-check persistence behavior.
func (c *Checker) RememberInstalledVersion(version string) {
	if c == nil || strings.TrimSpace(version) == "" {
		return
	}
	trimmed := strings.TrimSpace(version)
	_, _ = c.updateCache(func(cached *cache) bool {
		cached.InstalledVersion = trimmed
		return true
	})
}

// updateCache performs one read-modify-write cycle on the cache file while
// holding both the instance mutex and the advisory file lock in the data
// directory. It re-reads the cache from disk inside the lock, so mutate sees
// the newest state and every field it leaves alone survives, including fields
// another papio process wrote after this one read the cache. mutate reports
// whether the cache has to be written back, which makes the whole cycle a
// cross-process compare-and-set.
func (c *Checker) updateCache(mutate func(cached *cache) bool) (cache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	release, err := c.acquireCacheLock()
	if err != nil {
		return cache{}, err
	}
	defer release()
	cached := c.readCache()
	if !mutate(&cached) {
		return cached, nil
	}
	return cached, c.writeCache(cached)
}

// cacheLockPath keeps the lock beside its own cache file so the papio and
// zotio checkers never contend with each other.
func (c *Checker) cacheLockPath() string {
	return c.cachePath() + ".lock"
}

// acquireCacheLock takes the cross-process advisory lock that guards the cache
// file. The wait uses the real clock, never the injected one, because a test
// clock does not advance. The lock is never held across an HTTP request.
func (c *Checker) acquireCacheLock() (func(), error) {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create update cache directory: %w", err)
	}
	file, err := os.OpenFile(c.cacheLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open update cache lock: %w", err)
	}
	deadline := time.Now().Add(cacheLockWait)
	for {
		locked, err := tryLockFile(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock update cache: %w", err)
		}
		if locked {
			return func() {
				_ = unlockFile(file)
				_ = file.Close()
			}, nil
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("lock update cache: another papio process held it for %s", cacheLockWait)
		}
		time.Sleep(cacheLockRetry)
	}
}

// writeCache atomically replaces the cache file. Callers mutate the cache
// through updateCache, which supplies both the instance mutex and the
// cross-process lock; writeCache itself performs no locking.
func (c *Checker) writeCache(cached cache) error {
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(c.dataDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dataDir, ".update-cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.cachePath())
}

func (cached cache) info() *Info {
	if cached.LatestVersion == "" || cached.URL == "" {
		return nil
	}
	return &Info{
		LatestVersion: cached.LatestVersion,
		URL:           cached.URL,
		CheckedAt:     cached.CheckedAt,
	}
}

// fresh reports whether this cache was checked, or attempted, recently enough
// that issuing another request would break the at-most-once-a-day cadence.
func (cached cache) fresh(now time.Time) bool {
	return checkedRecently(cached.CheckedAt, now) || checkedRecently(cached.LastAttemptAt, now)
}

// checkedRecently treats a future timestamp as NOT recent: a clock rollback or
// a restored cache would otherwise suppress every check until the future
// instant plus the cadence had elapsed.
func checkedRecently(then, now time.Time) bool {
	if then.IsZero() || then.After(now) {
		return false
	}
	return now.Sub(then) < checkEvery
}
