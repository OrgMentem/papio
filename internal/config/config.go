// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package config loads ~/.config/papio/config.toml. The access mode is an
// explicit first-run choice: acquisition refuses to run without one (no silent
// automation default). Every job snapshots the policy it ran under.
package config

import (
	"fmt"
	"os"
	"path/filepath"
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

// Config is the loaded, validated configuration.
type Config struct {
	AccessMode string            `toml:"access_mode"`
	Email      string            `toml:"email"`
	DataDir    string            `toml:"data_dir"`
	Fetch      Fetch             `toml:"fetch"`
	PDF        PDF               `toml:"pdf"`
	Sources    map[string]Source `toml:"sources"`

	// Path this config was loaded from ("" for defaults).
	Path string `toml:"-"`
}

// Dir returns the papio config directory, honoring PAPIO_CONFIG_DIR for tests.
func Dir() string {
	if d := os.Getenv("PAPIO_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "papio")
}

// Default returns the baseline configuration. AccessMode is deliberately empty:
// callers that acquire must see ErrAccessModeUnset until the user chooses.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DataDir: filepath.Join(home, ".local", "share", "papio"),
		Fetch:   Fetch{MaxBytes: 100 << 20, TimeoutSeconds: 120},
		PDF:     PDF{OCREnabled: true, MinTextChars: 400, MaxOCRPages: 4, TitleMatchThreshold: 0.6},
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
		return cfg, fmt.Errorf("parsing config %s (unknown fields are rejected): %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.DataDir = expandHome(cfg.DataDir)
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
	for name, s := range c.Sources {
		if s.BaseURLForDev != "" && !strings.HasPrefix(s.BaseURLForDev, "http://127.0.0.1") && !strings.HasPrefix(s.BaseURLForDev, "http://localhost") {
			return fmt.Errorf("sources.%s.base_url_for_dev must be loopback", name)
		}
	}
	return nil
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
