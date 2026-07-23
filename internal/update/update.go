// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package update checks public papio-family release feeds at most once a day.
package update

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	releasesURL          = "https://api.github.com/repos/orgmentem/papio/releases/latest"
	ReleasesPageURL      = "https://github.com/orgmentem/papio/releases/latest"
	ZotioReleasesURL     = "https://api.github.com/repos/OrgMentem/zotio/releases/latest"
	ZotioReleasesPageURL = "https://github.com/OrgMentem/zotio/releases/latest"
	cacheName            = "update-cache.json"
	zotioCacheName       = "update-cache-zotio.json"
	checkEvery           = 24 * time.Hour
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
	// mu guards cache file access. It is never held while a refresh is in flight.
	mu sync.Mutex
	// refreshMu serializes stale-cache refreshes, including the HTTP request.
	// Check takes refreshMu only after releasing mu.
	refreshMu sync.Mutex
}

type cache struct {
	ETag             string    `json:"etag,omitempty"`
	LatestVersion    string    `json:"latest_version,omitempty"`
	URL              string    `json:"url,omitempty"`
	CheckedAt        time.Time `json:"checked_at,omitempty"`
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
// cache, and decoding failures are intentionally soft: callers receive any
// previously cached result (or nil) and no user-facing error.
func (c *Checker) Check(ctx context.Context) (*Info, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	cached := c.readCache()
	now := c.now().UTC()
	if checkedRecently(cached.CheckedAt, now) {
		info := cached.info()
		c.mu.Unlock()
		return info, nil
	}
	c.mu.Unlock()

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Another Check may have refreshed the cache before this one obtained the
	// refresh lock, so avoid issuing a duplicate request.
	c.mu.Lock()
	cached = c.readCache()
	now = c.now().UTC()
	if checkedRecently(cached.CheckedAt, now) {
		info := cached.info()
		c.mu.Unlock()
		return info, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.releasesURL, nil)
	if err != nil {
		return cached.info(), nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return cached.info(), nil
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		if cached.info() == nil {
			return nil, nil
		}
		c.mu.Lock()
		cached = c.readCache()
		cached.CheckedAt = now
		_ = c.writeCache(cached)
		info := cached.info()
		c.mu.Unlock()
		return info, nil
	}
	if response.StatusCode != http.StatusOK {
		return cached.info(), nil
	}

	var latest release
	if err := json.NewDecoder(response.Body).Decode(&latest); err != nil {
		return cached.info(), nil
	}
	latest.TagName = strings.TrimPrefix(latest.TagName, "v")
	if latest.TagName == "" || latest.HTMLURL == "" {
		return cached.info(), nil
	}
	c.mu.Lock()
	cached = c.readCache()
	cached.ETag = response.Header.Get("ETag")
	cached.LatestVersion = latest.TagName
	cached.URL = latest.HTMLURL
	cached.CheckedAt = now
	_ = c.writeCache(cached)
	info := cached.info()
	c.mu.Unlock()
	return info, nil
}

// TryMarkNagged atomically records a displayed update prompt when none has
// been displayed in the past day. A persistence failure suppresses the prompt
// so the caller never turns a missing cache into repeated stderr noise.
func (c *Checker) TryMarkNagged(now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	cached := c.readCache()
	now = now.UTC()
	if checkedRecently(cached.LastNaggedAt, now) {
		return false
	}
	cached.LastNaggedAt = now
	return c.writeCache(cached) == nil
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
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := c.readCache()
	cached.InstalledVersion = strings.TrimSpace(version)
	_ = c.writeCache(cached)
}

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

func checkedRecently(then, now time.Time) bool {
	return !then.IsZero() && now.Sub(then) < checkEvery
}
