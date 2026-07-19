// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package config loads ~/.config/papio/config.toml. The access mode is an
// explicit first-run choice: acquisition refuses to run without one (no silent
// automation default). Every job snapshots the policy it ran under.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Access modes (stack plan "Access profiles").
const (
	ModeConservative = "conservative"
	ModeAssisted     = "assisted"
	ModeMaximal      = "maximal"
)

// Source names used across config, budgets, and resolver registry.
const (
	SourceArXiv           = "arxiv"
	SourceEuropePMC       = "europepmc"
	SourceUnpaywall       = "unpaywall"
	SourceOpenAlex        = "openalex"
	SourceOpenAlexContent = "openalex_content"
	SourceCORE            = "core"
	SourceCrossrefTDM     = "crossref_tdm"
	SourceSemanticScholar = "semanticscholar"
)

// Source is one resolver's policy knobs.
type Source struct {
	Enabled       bool    `toml:"enabled"`
	APIKey        string  `toml:"api_key,omitempty"`
	RatePerSec    float64 `toml:"rate_per_sec,omitempty"`
	Burst         int     `toml:"burst,omitempty"`
	MaxCostUSD    float64 `toml:"max_cost_usd,omitempty"`     // monthly budget for paid sources
	BaseURLForDev string  `toml:"base_url_for_dev,omitempty"` // test/dev override; loopback only
}

// Fetch bounds every artifact download.
type Fetch struct {
	MaxBytes          int64 `toml:"max_bytes"`
	TimeoutSeconds    int   `toml:"timeout_seconds"`
	AllowHTTPLoopback bool  `toml:"allow_http_loopback,omitempty"` // tests/dev only
}

// PDF controls validation and OCR fallback.
type PDF struct {
	OCREnabled          bool    `toml:"ocr_enabled"`
	MinTextChars        int     `toml:"min_text_chars"`
	MaxOCRPages         int     `toml:"max_ocr_pages"`
	TitleMatchThreshold float64 `toml:"title_match_threshold"`
}

// Browser configures the Phase 2 ordinary-Chrome institutional handoff.
// Zero values disable the browser path entirely: no extension ID means the
// native host rejects every origin, and no OpenURL base means jobs never
// route to institutional access.
type Browser struct {
	// ExtensionID is the fixed Chrome extension ID allowed to talk to the
	// native host (32 chars, a-p). Empty disables the bridge.
	ExtensionID string `toml:"extension_id,omitempty"`
	// ExtensionIDs are additional Chrome-family (Chromium) extension IDs
	// allowed to reach the native host alongside ExtensionID — e.g. an Edge
	// Add-ons store copy or a second keyed build, which carry different IDs
	// than the Chrome Web Store package. Each is 32 chars a-p.
	ExtensionIDs []string `toml:"extension_ids,omitempty"`
	// FirefoxExtensionID is the Gecko add-on ID allowed to reach the native
	// host. Empty disables the Firefox bridge.
	FirefoxExtensionID string `toml:"firefox_extension_id,omitempty"`
	// OpenURLBase is the default institution's OpenURL resolver base (https).
	OpenURLBase string `toml:"openurl_base_url,omitempty"`
	// ShibbolethEntityID is the default institution's Shibboleth IdP entityID;
	// empty disables federated login-routing.
	ShibbolethEntityID string `toml:"shibboleth_entity_id,omitempty"`
	// ProquestAccountID is the institution's ProQuest account id used to unlock
	// ProQuest's OpenURL link-resolver; empty disables.
	ProquestAccountID string `toml:"proquest_account_id,omitempty"`
	// Resolvers contains named institutional access profiles. Each named
	// profile carries its own OpenURL base and optional federated-login
	// identity, so a multi-institution user routes each job to the right
	// library. The top-level OpenURLBase / ShibbolethEntityID /
	// ProquestAccountID fields above are the implicit "default" profile.
	Resolvers map[string]Institution `toml:"resolvers,omitempty"`
	// AdoptionRoot is the directory Chrome downloads into for adoption;
	// the daemon rejects reported paths outside <root>/<job_id>/.
	// Default: <data_dir>/adoptions.
	AdoptionRoot string `toml:"download_adoption_root,omitempty"`
	// ActionExpirySeconds bounds how long one browser handoff stays open.
	ActionExpirySeconds int `toml:"action_expiry_seconds,omitempty"`
}

// ChromiumExtensionIDs returns the deduplicated Chrome-family extension IDs
// allowed to reach the native host: the primary ExtensionID first, then any
// additional ExtensionIDs. Empty entries are dropped. Every Chromium browser
// (Chrome, Edge, Vivaldi, Brave, Opera, …) shares this same allowlist.
func (b Browser) ChromiumExtensionIDs() []string {
	seen := make(map[string]bool)
	ids := make([]string, 0, 1+len(b.ExtensionIDs))
	for _, id := range append([]string{b.ExtensionID}, b.ExtensionIDs...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// Institution is one library's institutional-access identity: its OpenURL
// link-resolver base plus the optional Shibboleth entityID and ProQuest
// account id used to auto-route provider login walls without a manual WAYF
// selection. Named institutions live in Browser.Resolvers; the default
// institution is expressed by the top-level Browser fields.
type Institution struct {
	// OpenURLBase is the institution's OpenURL resolver base (https).
	OpenURLBase string `toml:"openurl_base_url"`
	// ShibbolethEntityID is the institution's Shibboleth IdP entityID; empty
	// disables federated login-routing for this profile.
	ShibbolethEntityID string `toml:"shibboleth_entity_id,omitempty"`
	// ProquestAccountID unlocks this institution's ProQuest link-resolver;
	// empty disables the accountid append for this profile.
	ProquestAccountID string `toml:"proquest_account_id,omitempty"`
}

// UnmarshalText lets a resolver profile be written as a bare OpenURL base
// string — the shorthand `name = "https://…"` — in addition to a full
// institution table. go-toml routes scalar string values here and decodes
// tables through the struct fields normally (including DisallowUnknownFields),
// so the string form keeps pre-existing single-base resolver configs loading
// without a migration while the table form adds the per-profile login identity.
func (i *Institution) UnmarshalText(text []byte) error {
	i.OpenURLBase = string(text)
	return nil
}

// Zotio configures the credential-owning Zotero CLI boundary. papio invokes
// this executable but never reads or stores Zotero credentials itself.
type Zotio struct {
	Executable     string `toml:"executable"`
	TimeoutSeconds int    `toml:"timeout_seconds"`
	AttachmentMode string `toml:"attachment_mode"`
	AutoImport     bool   `toml:"auto_import"`
	AutoEnrich     bool   `toml:"auto_enrich"`
}

// Notify configures best-effort notifications from the daemon: local desktop
// notifications and an optional remote webhook. Both are fire-and-forget; a
// delivery failure never fails the work that triggered it.
type Notify struct {
	Enabled bool `toml:"enabled"`
	// WebhookURL, when set, receives every notification as a JSON POST in
	// addition to (not instead of) the local desktop channel.
	WebhookURL string `toml:"webhook_url"`
	// WebhookSecret, when set, is sent as "Authorization: Bearer <secret>".
	WebhookSecret string `toml:"webhook_secret"`
}

// Discovery selects which discovery backends serve search and watches, in
// merge-preference order. Empty means OpenAlex only (the historical default).
// Per-backend API keys and dev base URLs live in the existing [sources] map
// under the same name (e.g. sources.semanticscholar.api_key).
type Discovery struct {
	Sources []string `toml:"sources"`
}

// Updates configures the optional daily release check.
type Updates struct {
	Check bool `toml:"check"`
}

// Config is the loaded, validated configuration.
type Config struct {
	AccessMode string            `toml:"access_mode"`
	Email      string            `toml:"email"`
	DataDir    string            `toml:"data_dir"`
	Fetch      Fetch             `toml:"fetch"`
	PDF        PDF               `toml:"pdf"`
	Browser    Browser           `toml:"browser"`
	Zotio      Zotio             `toml:"zotio"`
	Notify     Notify            `toml:"notify"`
	Updates    Updates           `toml:"updates"`
	Discovery  Discovery         `toml:"discovery"`
	Sources    map[string]Source `toml:"sources"`

	// Path this config was loaded from ("" for defaults).
	Path string `toml:"-"`
}

// Dir returns the papio config directory, honoring PAPIO_CONFIG_DIR for tests.
func Dir() string {
	if d := os.Getenv("PAPIO_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "papio")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "papio")
}

// defaultDataDir is the baseline data directory: %LOCALAPPDATA%\papio on Windows
// (non-roaming, the right home for a database), ~/.local/share/papio elsewhere.
func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "papio")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "papio")
}

// Default returns the baseline configuration. AccessMode is deliberately empty:
// callers that acquire must see ErrAccessModeUnset until the user chooses.
func Default() Config {
	return Config{
		DataDir: defaultDataDir(),
		Fetch:   Fetch{MaxBytes: 100 << 20, TimeoutSeconds: 120},
		PDF:     PDF{OCREnabled: true, MinTextChars: 400, MaxOCRPages: 4, TitleMatchThreshold: 0.6},
		Browser: Browser{ActionExpirySeconds: 1800},
		Zotio:   Zotio{Executable: "zotio", TimeoutSeconds: 120, AttachmentMode: "stored", AutoImport: false, AutoEnrich: true},
		Notify:  Notify{Enabled: true},
		Sources: map[string]Source{
			SourceArXiv:           {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceEuropePMC:       {Enabled: true, RatePerSec: 2, Burst: 2},
			SourceUnpaywall:       {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceOpenAlex:        {Enabled: false, RatePerSec: 2, Burst: 2},
			SourceOpenAlexContent: {Enabled: false},
			SourceCORE:            {Enabled: false, RatePerSec: 0.4, Burst: 1},
			SourceCrossrefTDM:     {Enabled: false, RatePerSec: 1, Burst: 1},
		},
	}
}

// ErrAccessModeUnset is returned by RequireAccessMode until first-run setup.
type ErrAccessModeUnset struct{ Path string }

func (e *ErrAccessModeUnset) Error() string {
	return fmt.Sprintf("access_mode is not set in %s: choose conservative, assisted, or maximal (explicit first-run decision; no silent automation default)", e.Path)
}

// Load reads path (or the default location when path is empty), layering the
// file over Default(). A missing file yields defaults with AccessMode unset.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = filepath.Join(Dir(), "config.toml")
	}
	cfg.Path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var missing *toml.StrictMissingError
		if errors.As(err, &missing) {
			fields := make([]string, 0, len(missing.Errors))
			for _, decodeErr := range missing.Errors {
				fields = append(fields, strings.Join(decodeErr.Key(), "."))
			}
			return cfg, fmt.Errorf("config %s contains fields this papio build does not recognize (%s). This usually means the config was written for a newer papio — update papio, or remove those fields: %w", path, strings.Join(fields, ", "), err)
		}
		return cfg, fmt.Errorf("parsing config %s (unknown fields are rejected): %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	cfg.Browser.AdoptionRoot = expandHome(cfg.Browser.AdoptionRoot)
	cfg.Zotio.Executable = expandHome(cfg.Zotio.Executable)
	return cfg, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func (c *Config) validate() error {
	switch c.AccessMode {
	case "", ModeConservative, ModeAssisted, ModeMaximal:
	default:
		return fmt.Errorf("invalid access_mode %q (conservative, assisted, or maximal)", c.AccessMode)
	}
	if c.Fetch.MaxBytes < 1<<20 {
		return fmt.Errorf("fetch.max_bytes %d below 1 MiB floor", c.Fetch.MaxBytes)
	}
	if c.Fetch.TimeoutSeconds < 5 {
		return fmt.Errorf("fetch.timeout_seconds %d below 5s floor", c.Fetch.TimeoutSeconds)
	}
	if c.PDF.TitleMatchThreshold <= 0 || c.PDF.TitleMatchThreshold > 1 {
		return fmt.Errorf("pdf.title_match_threshold must be in (0,1]")
	}
	if c.Browser.ExtensionID != "" && !extensionIDRE.MatchString(c.Browser.ExtensionID) {
		return fmt.Errorf("browser.extension_id must be 32 chars a-p")
	}
	for _, id := range c.Browser.ExtensionIDs {
		if !extensionIDRE.MatchString(id) {
			return fmt.Errorf("browser.extension_ids entries must each be 32 chars a-p")
		}
	}
	if c.Browser.FirefoxExtensionID != "" && !firefoxExtensionIDRE.MatchString(c.Browser.FirefoxExtensionID) {
		return fmt.Errorf("browser.firefox_extension_id must be a Gecko email-like ID or braced GUID")
	}
	if c.Browser.OpenURLBase != "" {
		if err := validateOpenURLBase(c.Browser.OpenURLBase); err != nil {
			return fmt.Errorf("browser.openurl_base_url %w", err)
		}
	}
	if c.Browser.ShibbolethEntityID != "" {
		if err := validateOpenURLBase(c.Browser.ShibbolethEntityID); err != nil {
			return fmt.Errorf("browser.shibboleth_entity_id %w", err)
		}
	}
	if c.Browser.ProquestAccountID != "" && (len(c.Browser.ProquestAccountID) > 64 || !proquestAccountIDRE.MatchString(c.Browser.ProquestAccountID)) {
		return fmt.Errorf("browser.proquest_account_id must be digits (max 64)")
	}
	for name, inst := range c.Browser.Resolvers {
		if !resolverNameRE.MatchString(name) {
			return fmt.Errorf("browser.resolvers.%s name must be lowercase alphanumeric", name)
		}
		if err := validateOpenURLBase(inst.OpenURLBase); err != nil {
			return fmt.Errorf("browser.resolvers.%s.openurl_base_url %w", name, err)
		}
		if inst.ShibbolethEntityID != "" {
			if err := validateOpenURLBase(inst.ShibbolethEntityID); err != nil {
				return fmt.Errorf("browser.resolvers.%s.shibboleth_entity_id %w", name, err)
			}
		}
		if inst.ProquestAccountID != "" && (len(inst.ProquestAccountID) > 64 || !proquestAccountIDRE.MatchString(inst.ProquestAccountID)) {
			return fmt.Errorf("browser.resolvers.%s.proquest_account_id must be digits (max 64)", name)
		}
	}
	if c.Browser.ActionExpirySeconds < 0 {
		return fmt.Errorf("browser.action_expiry_seconds must be >= 0")
	}
	if strings.TrimSpace(c.Zotio.Executable) == "" {
		return fmt.Errorf("zotio.executable is required")
	}
	if c.Zotio.TimeoutSeconds < 5 || c.Zotio.TimeoutSeconds > 600 {
		return fmt.Errorf("zotio.timeout_seconds must be in 5..600")
	}
	if c.Zotio.AttachmentMode != "stored" && c.Zotio.AttachmentMode != "linked-file" {
		return fmt.Errorf("zotio.attachment_mode must be stored or linked-file")
	}
	for name, s := range c.Sources {
		if s.BaseURLForDev != "" && !strings.HasPrefix(s.BaseURLForDev, "http://127.0.0.1") && !strings.HasPrefix(s.BaseURLForDev, "http://localhost") {
			return fmt.Errorf("sources.%s.base_url_for_dev must be loopback", name)
		}
	}
	if c.Notify.WebhookURL != "" {
		u, err := url.Parse(c.Notify.WebhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("notify.webhook_url must be an absolute http(s) URL")
		}
	}
	if c.Notify.WebhookSecret != "" && c.Notify.WebhookURL == "" {
		return fmt.Errorf("notify.webhook_secret is set but notify.webhook_url is empty")
	}
	seenDiscovery := map[string]bool{}
	for _, name := range c.Discovery.Sources {
		if name != SourceOpenAlex && name != SourceSemanticScholar {
			return fmt.Errorf("discovery.sources entry %q must be %s or %s", name, SourceOpenAlex, SourceSemanticScholar)
		}
		if seenDiscovery[name] {
			return fmt.Errorf("discovery.sources lists %q twice", name)
		}
		seenDiscovery[name] = true
	}
	return nil
}

// extensionIDRE matches Chrome's a-p base16 extension ID alphabet.
var extensionIDRE = regexp.MustCompile(`^[a-p]{32}$`)

// firefoxExtensionIDRE matches Gecko's email-like or braced-GUID add-on ID.
var firefoxExtensionIDRE = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+$|^\{[0-9a-fA-F-]{36}\}$`)

var resolverNameRE = regexp.MustCompile(`^[a-z0-9]+$`)
var proquestAccountIDRE = regexp.MustCompile(`^[0-9]+$`)

func validateOpenURLBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.TrimSpace(base) != base {
		return fmt.Errorf("must be an absolute https URL")
	}
	return nil
}

// EffectiveAdoptionRoot returns the configured adoption root or its default.
func (c *Config) EffectiveAdoptionRoot() string {
	if c.Browser.AdoptionRoot != "" {
		return c.Browser.AdoptionRoot
	}
	return filepath.Join(c.DataDir, "adoptions")
}

// RequireAccessMode returns the mode or ErrAccessModeUnset.
func (c *Config) RequireAccessMode() (string, error) {
	if c.AccessMode == "" {
		return "", &ErrAccessModeUnset{Path: c.Path}
	}
	return c.AccessMode, nil
}

// FetchTimeout is Fetch.TimeoutSeconds as a duration.
func (c *Config) FetchTimeout() time.Duration {
	return time.Duration(c.Fetch.TimeoutSeconds) * time.Second
}

// SourcePolicy returns the effective source policy (zero value when absent).
func (c *Config) SourcePolicy(name string) Source {
	return c.Sources[name]
}

// InstitutionFor returns the institutional-access identity for a resolver
// profile. The empty name (and the explicit "default" name) select the default
// institution expressed by the top-level Browser fields; any other name selects
// a named profile from Browser.Resolvers. The boolean reports whether a usable
// profile exists (default: a non-empty OpenURL base; named: map presence).
func (c *Config) InstitutionFor(name string) (Institution, bool) {
	if name == "" || name == "default" {
		inst := Institution{
			OpenURLBase:        c.Browser.OpenURLBase,
			ShibbolethEntityID: c.Browser.ShibbolethEntityID,
			ProquestAccountID:  c.Browser.ProquestAccountID,
		}
		return inst, inst.OpenURLBase != ""
	}
	inst, ok := c.Browser.Resolvers[name]
	return inst, ok
}

// OpenURLBaseFor returns the configured OpenURL base for a resolver profile.
// Its boolean result distinguishes a configured profile from one that is merely
// unavailable because no base has been configured.
func (c *Config) OpenURLBaseFor(name string) (string, bool) {
	inst, ok := c.InstitutionFor(name)
	return inst.OpenURLBase, ok
}

// ResolverNames returns the selectable resolver profiles in stable order.
func (c *Config) ResolverNames() []string {
	names := make([]string, 0, len(c.Browser.Resolvers)+1)
	if c.Browser.OpenURLBase != "" {
		names = append(names, "default")
	}
	for name := range c.Browser.Resolvers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ResolverOrigins returns the distinct https origins of every configured OpenURL
// resolver base (default plus each named profile), sorted. The extension needs a
// host permission on a resolver origin to steer its "full text options" menu; it
// requests exactly these origins and drops any already covered by a static
// host_permission. Institution identity therefore lives only in the user's
// config, never in extension code.
func (c *Config) ResolverOrigins() []string {
	seen := make(map[string]struct{})
	origins := make([]string, 0, len(c.Browser.Resolvers)+1)
	add := func(base string) {
		if base == "" {
			return
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return
		}
		origin := "https://" + host
		if port := u.Port(); port != "" && port != "443" {
			n, convErr := strconv.Atoi(port)
			if convErr != nil || n < 1 || n > 65535 {
				return
			}
			origin += ":" + port
		}
		if _, dup := seen[origin]; dup {
			return
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	add(c.Browser.OpenURLBase)
	for _, inst := range c.Browser.Resolvers {
		add(inst.OpenURLBase)
	}
	slices.Sort(origins)
	// Match the protocol cap so every accepted config yields a valid hello_ack.
	if len(origins) > 32 {
		origins = origins[:32]
	}
	return origins
}

// Save validates and atomically writes cfg as a user-only TOML file. An empty
// path uses the default config location. API keys may be present, so neither
// temporary nor final files are group/world-readable.
func Save(cfg Config, path string) error {
	if path == "" {
		path = filepath.Join(Dir(), "config.toml")
	}
	if _, err := cfg.RequireAccessMode(); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	cfg.Path = path
	return nil
}
