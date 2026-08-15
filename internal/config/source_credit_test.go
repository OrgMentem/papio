// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSourcesShipsOpenAlexDailyCreditFraction(t *testing.T) {
	sources := defaultSources()
	openAlex, ok := sources[SourceOpenAlex]
	if !ok {
		t.Fatal("openalex missing from defaultSources")
	}
	if openAlex.DailyCreditFraction != DefaultDailyCreditFraction {
		t.Fatalf("daily_credit_fraction = %v, want %v", openAlex.DailyCreditFraction, DefaultDailyCreditFraction)
	}
	if openAlex.DailyCreditFraction == 0 {
		t.Fatal("daily_credit_fraction must be non-zero so the fuse is not inert")
	}
}

func TestDefaultSourcesDisableKeepAlives(t *testing.T) {
	for name, s := range defaultSources() {
		if !s.DisableKeepAlives() {
			t.Fatalf("sources.%s disable_keep_alives effective = false, want true", name)
		}
		if s.AllowKeepAlives {
			t.Fatalf("sources.%s allow_keep_alives = true, want false", name)
		}
	}
}

func TestLoadRejectsOutOfRangeDailyCreditFraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Sources[SourceOpenAlex] = Source{DailyCreditFraction: 1.5, AllowKeepAlives: cfg.Sources[SourceOpenAlex].AllowKeepAlives}
	err := Save(cfg, path)
	if err == nil || !strings.Contains(err.Error(), "daily_credit_fraction") {
		t.Fatalf("save err = %v, want daily_credit_fraction rejection", err)
	}
}

func TestLoadRoundTripSourceCreditFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
access_mode = "conservative"
[sources.openalex]
daily_credit_fraction = 0.25
daily_credit_limit = 5000
allow_keep_alives = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	oa := got.Sources[SourceOpenAlex]
	if oa.DailyCreditFraction != 0.25 || oa.DailyCreditLimit != 5000 {
		t.Fatalf("openalex credit fields = fraction=%v limit=%d", oa.DailyCreditFraction, oa.DailyCreditLimit)
	}
	if !oa.AllowKeepAlives {
		t.Fatal("allow_keep_alives = true not preserved")
	}
	if oa.DisableKeepAlives() {
		t.Fatal("DisableKeepAlives() = true, want false when allow_keep_alives=true")
	}
}

func TestLoadAbsentSourceKeepsKeepAlivesDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.unpaywall]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{SourceCrossrefMetadata, SourceOpenAlex} {
		s := cfg.SourcePolicy(name)
		if !s.DisableKeepAlives() {
			t.Fatalf("source %s DisableKeepAlives() = false, want true for absent table", name)
		}
		if s.AllowKeepAlives {
			t.Fatalf("source %s AllowKeepAlives = true, want false for absent table", name)
		}
	}
}
