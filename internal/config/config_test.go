// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"papio/internal/routes"

	toml "github.com/pelletier/go-toml/v2"
)

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	cfg := Default()
	if !cfg.Browser.DirectRoutesEnabled {
		t.Fatal("direct routes disabled by default")
	}
	cfg.AccessMode = ModeConservative
	cfg.Email = "researcher@example.test"
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Sources[SourceOpenAlex] = Source{Enabled: true, APIKey: "secret", RatePerSec: 2, Burst: 1}
	cfg.Zotio.Executable = filepath.Join(t.TempDir(), "zotio")
	cfg.Zotio.AttachmentMode = "linked-file"
	cfg.Hooks.OnReady = "true"
	cfg.Browser.DirectRoutesEnabled = false
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}
	parent, _ := os.Stat(filepath.Dir(path))
	if parent.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode = %v, want 0700", parent.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessMode != cfg.AccessMode || got.Email != cfg.Email ||
		got.Sources[SourceOpenAlex].APIKey != "secret" ||
		got.Zotio.Executable != cfg.Zotio.Executable ||
		got.Zotio.AttachmentMode != "linked-file" || got.Hooks.OnReady != "true" ||
		got.Browser.DirectRoutesEnabled != cfg.Browser.DirectRoutesEnabled || got.Path != path {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestSaveRequiresExplicitAccessMode(t *testing.T) {
	err := Save(Default(), filepath.Join(t.TempDir(), "config.toml"))
	var unset *ErrAccessModeUnset
	if !errors.As(err, &unset) {
		t.Fatalf("save err = %v, want ErrAccessModeUnset", err)
	}
}

func TestSaveAllowsEmptyZotioExecutableUnlessAutoImport(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Zotio.Executable = ""
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("empty zotio.executable rejected: %v", err)
	}
	cfg.Zotio.AutoImport = true
	err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "zotio.auto_import requires zotio.executable") {
		t.Fatalf("auto_import without executable err = %v", err)
	}
}

func TestSaveValidatesExceptionTagsAndRecheckWindow(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	if cfg.Zotio.ExceptionTags {
		t.Fatal("default zotio.exception_tags = true, want false")
	}
	if cfg.Zotio.UnavailableRecheckDays != 14 {
		t.Fatalf("default zotio.unavailable_recheck_days = %d, want 14", cfg.Zotio.UnavailableRecheckDays)
	}
	cfg.Zotio.Executable = ""
	cfg.Zotio.ExceptionTags = true
	err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "zotio.exception_tags requires zotio.executable") {
		t.Fatalf("exception_tags without executable err = %v", err)
	}
	cfg.Zotio.Executable = "zotio"
	cfg.Zotio.UnavailableRecheckDays = 0
	err = Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "zotio.unavailable_recheck_days must be in 1..365") {
		t.Fatalf("recheck range err = %v", err)
	}
	cfg.Zotio.UnavailableRecheckDays = 30
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("valid exception-tag config rejected: %v", err)
	}
}
func TestCapturesDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	if !cfg.Captures.Enabled || cfg.Captures.MaxPerHost != 10 || cfg.Captures.MaxAgeDays != 14 {
		t.Fatalf("default captures = %+v, want enabled with 10 captures for 14 days", cfg.Captures)
	}
	cfg.Captures.MaxPerHost = 0
	err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "captures.max_per_host must be in 1..1000") {
		t.Fatalf("invalid captures.max_per_host err = %v", err)
	}
	cfg.Captures.MaxPerHost = 10
	cfg.Captures.MaxAgeDays = 366
	err = Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "captures.max_age_days must be in 1..365") {
		t.Fatalf("invalid captures.max_age_days err = %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[captures]\nenabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Captures.Enabled || loaded.Captures.MaxPerHost != 10 || loaded.Captures.MaxAgeDays != 14 {
		t.Fatalf("loaded captures = %+v, want disabled with default retention", loaded.Captures)
	}
}

func TestSaveValidatesHooksTimeoutOnlyWhenOnReadySet(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Hooks.TimeoutSeconds = 0
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("hooks timeout validated while on_ready empty: %v", err)
	}
	cfg.Hooks.OnReady = "true"
	err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "hooks.timeout_seconds must be in 5..600") {
		t.Fatalf("out-of-range hooks timeout err = %v", err)
	}
	cfg.Hooks.TimeoutSeconds = 120
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("valid hooks config rejected: %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\nunknown_option=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestLoadRetainsDefaultCrossrefMetadataPolicyWhenSourceIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.unpaywall]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SourcePolicy(SourceCrossrefMetadata); got != (Source{Enabled: true, RatePerSec: 1, Burst: 1}) {
		t.Fatalf("crossref metadata policy = %+v", got)
	}
	if got := cfg.SourcePolicy(SourceRetractionWatch); got != (Source{Enabled: true, RatePerSec: 1, Burst: 1}) {
		t.Fatalf("retraction watch policy = %+v", got)
	}
}

// A config written by an older `papio init`/`config save` lists every default
// source, including the removed openalex_content. Upgrading must not make that
// file unparseable, so the name is tolerated and dropped rather than rejected.
func TestLoadToleratesAndDropsRemovedOpenAlexContentSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.openalex_content]\nenabled=true\n[sources.unpaywall]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config listing the removed openalex_content source must still load: %v", err)
	}
	if _, ok := cfg.Sources["openalex_content"]; ok {
		t.Error("removed source openalex_content survived Load; it must be dropped so the next config save rewrites without it")
	}
	if !cfg.Sources[SourceUnpaywall].Enabled {
		t.Error("dropping the removed source must not disturb the sources beside it")
	}
}

func TestLoadRejectsMisspelledSourceName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.unpaywal]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("misspelled source name accepted")
	}
	if !strings.Contains(err.Error(), "sources.unpaywal") || !strings.Contains(err.Error(), "not a recognized source name") {
		t.Fatalf("misspelled source rejection error = %q", err)
	}
}

func TestLoadAcceptsSemanticScholarSourceForAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.semanticscholar]\napi_key='shh'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("discovery-only semanticscholar source rejected: %v", err)
	}
	if got := cfg.SourcePolicy(SourceSemanticScholar); got.APIKey != "shh" {
		t.Fatalf("semanticscholar policy = %+v", got)
	}
}

func TestLoadAcceptsValidSourceKnob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.unpaywall]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("valid sources.unpaywall config rejected: %v", err)
	}
	if got := cfg.SourcePolicy(SourceUnpaywall); !got.Enabled {
		t.Fatalf("unpaywall policy = %+v", got)
	}
}

func TestLoadExplainsUnknownBrowserField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[browser]\nbogus_option=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("unknown browser config field accepted")
	}
	if !strings.Contains(err.Error(), "browser.bogus_option") || !strings.Contains(err.Error(), "update papio") {
		t.Fatalf("unknown browser config error = %q", err)
	}
	var missing *toml.StrictMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("unknown browser config error = %v, want wrapped StrictMissingError", err)
	}
}

func TestLoadKeepsGenericParseErrorForInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("invalid TOML accepted")
	}
	if !strings.Contains(err.Error(), "parsing config") || strings.Contains(err.Error(), "update papio") {
		t.Fatalf("invalid TOML error = %q", err)
	}
}

func TestSaveRejectsInvalidZotioAttachmentMode(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Zotio.AttachmentMode = "copy"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
		t.Fatal("invalid Zotio attachment mode accepted")
	}
}

func TestSaveRejectsInvalidFirefoxExtensionID(t *testing.T) {
	for _, id := range []string{
		"not-an-addon-id",
		"papio@",
		"{not-a-guid}",
		"papio@orgmentem.com ",
	} {
		t.Run(id, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.Browser.FirefoxExtensionID = id
			if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
				t.Fatalf("invalid Firefox extension ID %q accepted", id)
			}
		})
	}
}

func TestSaveAcceptsFirefoxExtensionIDs(t *testing.T) {
	for _, id := range []string{
		"papio@orgmentem.com",
		"{01234567-89ab-cdef-0123-456789abcdef}",
	} {
		t.Run(id, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.Browser.FirefoxExtensionID = id
			if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
				t.Fatalf("valid Firefox extension ID %q rejected: %v", id, err)
			}
		})
	}
}

// TestSaveValidatesBrowserIDs collapses two structurally identical Save
// validation checks that share the same shape: build Default(), set
// AccessMode to conservative, set one Browser field to a valid value and
// expect Save to succeed, then loop over invalid values and expect Save
// to fail for each. The table carries the field setter, the valid value,
// and the list of invalid values so a failure still names which field and
// which invalid value was accepted.
func TestSaveValidatesBrowserIDs(t *testing.T) {
	tests := []struct {
		name    string
		set     func(*Config, string)
		valid   string
		invalid []string
	}{
		{
			name:    "browser.shibboleth_entity_id",
			set:     func(c *Config, v string) { c.Browser.ShibbolethEntityID = v },
			valid:   "https://idp.example.edu/entity",
			invalid: []string{"http://idp.example.edu/entity", "https://"},
		},
		{
			name:    "browser.proquest_account_id",
			set:     func(c *Config, v string) { c.Browser.ProquestAccountID = v },
			valid:   "12345",
			invalid: []string{"12345x", strings.Repeat("1", 65)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			tc.set(&cfg, tc.valid)
			if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
				t.Fatalf("valid %s %q rejected: %v", tc.name, tc.valid, err)
			}
			for _, iv := range tc.invalid {
				t.Run(iv, func(t *testing.T) {
					cfg := Default()
					cfg.AccessMode = ModeConservative
					tc.set(&cfg, iv)
					if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
						t.Fatalf("invalid %s %q accepted", tc.name, iv)
					}
				})
			}
		})
	}
}

// TestBooleanConfigDefaultsAndToggles collapses four boolean default/toggle
// checks that share an identical shape: assert the default of one boolean
// config field, write a TOML snippet that toggles it, load the file, and
// assert it flipped. Each default is a deliberate product decision —
//
//	zotio.auto_import off (import requires explicit opt-in),
//	zotio.auto_enrich on (enrichment is expected by default),
//	notify.enabled on (users expect notifications unless disabled),
//	updates.check off (update checks are opt-in) —
//
// and the toggle leg proves strict TOML decoding actually reaches the field.
// Because decoding is strict (unknown fields are rejected), each TOML snippet
// must contain only the real table/field name — copied verbatim from the
// original per-field tests.
func TestBooleanConfigDefaultsAndToggles(t *testing.T) {
	tests := []struct {
		name        string
		get         func(Config) bool
		wantDefault bool
		toml        string
	}{
		{
			name:        "zotio.auto_import defaults off and loads true",
			get:         func(c Config) bool { return c.Zotio.AutoImport },
			wantDefault: false,
			toml:        "access_mode='conservative'\n[zotio]\nauto_import=true\n",
		},
		{
			name:        "zotio.auto_enrich defaults on and loads false",
			get:         func(c Config) bool { return c.Zotio.AutoEnrich },
			wantDefault: true,
			toml:        "access_mode='conservative'\n[zotio]\nauto_enrich=false\n",
		},
		{
			name:        "notify.enabled defaults on and loads false",
			get:         func(c Config) bool { return c.Notify.Enabled },
			wantDefault: true,
			toml:        "access_mode='conservative'\n[notify]\nenabled=false\n",
		},
		{
			name:        "updates.check defaults off and loads true",
			get:         func(c Config) bool { return c.Updates.Check },
			wantDefault: false,
			toml:        "access_mode='conservative'\n[updates]\ncheck=true\n",
		},
		{
			name:        "openalex sibling_title_search defaults off and loads true",
			get:         func(c Config) bool { return c.Sources[SourceOpenAlex].SiblingTitleSearch },
			wantDefault: false,
			toml:        "access_mode='conservative'\n[sources.openalex]\nsibling_title_search=true\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.get(Default()); got != test.wantDefault {
				t.Fatalf("default %s = %v, want %v", test.name, got, test.wantDefault)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(test.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := test.get(cfg); got == test.wantDefault {
				t.Fatalf("loaded %s = %v, want %v", test.name, got, !test.wantDefault)
			}
		})
	}
}

func TestBrowserResolverProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`access_mode = "conservative"
[browser]
openurl_base_url = "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE"
shibboleth_entity_id = "https://idp.example.edu/entity"
proquest_account_id = "12345"

[browser.resolvers.institute]
openurl_base_url = "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"
shibboleth_entity_id = "https://idp.example-institute.edu/idp/shibboleth"
proquest_account_id = "67890"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cfg.OpenURLBaseFor("institute"); !ok || got != "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS" {
		t.Fatalf("institute resolver = %q, %t", got, ok)
	}
	if got, ok := cfg.OpenURLBaseFor(""); !ok || got != "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE" {
		t.Fatalf("default resolver = %q, %t", got, ok)
	}
	if names := cfg.ResolverNames(); len(names) != 2 || names[0] != "default" || names[1] != "institute" {
		t.Fatalf("resolver names = %v", names)
	}
	// Each profile carries its own institutional identity; a named institution
	// never inherits the default institution's entityID/accountid.
	def, _ := cfg.InstitutionFor("")
	if def.ShibbolethEntityID != "https://idp.example.edu/entity" || def.ProquestAccountID != "12345" {
		t.Fatalf("default institution = %+v", def)
	}
	institute, ok := cfg.InstitutionFor("institute")
	if !ok || institute.ShibbolethEntityID != "https://idp.example-institute.edu/idp/shibboleth" || institute.ProquestAccountID != "67890" {
		t.Fatalf("institute institution = %+v, %t", institute, ok)
	}
}

func TestBrowserDefaultResolverValidatesAndMatchesOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`access_mode = "conservative"
[browser]
openurl_base_url = "https://example.primo.exlibrisgroup.com/nde/openurl"
default_resolver = "college"

[browser.resolvers.college]
openurl_base_url = "https://onesearch.library.example-college.edu/discovery/openurl"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.DefaultResolver != "college" {
		t.Fatalf("default_resolver = %q, want college", cfg.Browser.DefaultResolver)
	}
	if got := cfg.ResolverProfilesForOrigin("https://onesearch.library.example-college.edu"); !slices.Equal(got, []string{"college"}) {
		t.Fatalf("origin profiles = %v, want [college]", got)
	}
	if got := cfg.ResolverProfilesForOrigin("https://example.primo.exlibrisgroup.com"); !slices.Equal(got, []string{"default"}) {
		t.Fatalf("default origin profiles = %v, want [default]", got)
	}
	if got := cfg.ResolverProfilesForOrigin("https://unconfigured.example.edu"); len(got) != 0 {
		t.Fatalf("unconfigured origin profiles = %v, want none", got)
	}
}

// The operator's own institution is routinely configured twice: once at the
// top level as the default, once named so a job can request it explicitly.
// Both entries carry one openurl_base_url, so this origin serves two profiles.
// Resolving it to a single profile treated that as ambiguous and dropped every
// uncorrelated session_evidence frame for the operator's own library, which is
// how a real sign-in came to release nothing (measured live 2026-08-20).
func TestResolverProfilesForSharedOriginAttributeToEveryProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`access_mode = "conservative"
[browser]
openurl_base_url = "https://example.primo.exlibrisgroup.com/nde/openurl?institution=EX"

[browser.resolvers.example]
openurl_base_url = "https://example.primo.exlibrisgroup.com/nde/openurl?institution=EX"

[browser.resolvers.other]
openurl_base_url = "https://onesearch.library.example-college.edu/discovery/openurl"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolverProfilesForOrigin("https://example.primo.exlibrisgroup.com"); !slices.Equal(got, []string{"default", "example"}) {
		t.Fatalf("shared origin profiles = %v, want [default example]", got)
	}
	if got := cfg.ResolverProfilesForOrigin("https://onesearch.library.example-college.edu"); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("distinct origin profiles = %v, want [other]", got)
	}
	// One origin, one host permission: the request set stays deduplicated.
	if got := cfg.ResolverOrigins(); !slices.Equal(got, []string{
		"https://example.primo.exlibrisgroup.com",
		"https://onesearch.library.example-college.edu",
	}) {
		t.Fatalf("resolver origins = %v", got)
	}
}

func TestBrowserDefaultResolverRejectsUnknownProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("access_mode = \"conservative\"\n[browser]\ndefault_resolver = \"missing\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown browser.default_resolver accepted")
	}
}

func TestBrowserResolverProfilesRejectInvalidNameAndURL(t *testing.T) {
	for _, test := range []struct {
		name, profile, base string
	}{
		{name: "uppercase name", profile: "INSTITUTE", base: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
		{name: "http URL", profile: "institute", base: "http://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
		{name: "relative URL", profile: "institute", base: "/discovery/openurl"},
		// "default" is the implicit top-level institution (reserved by InstitutionFor),
		// not a valid [browser.resolvers.*] map key. A profile under this name can
		// never be reached and causes ResolverNames() to duplicate "default".
		{name: "reserved name default", profile: "default", base: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			data := []byte("access_mode = \"conservative\"\n[browser.resolvers." + test.profile + "]\nopenurl_base_url = \"" + test.base + "\"\n")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid resolver profile accepted")
			}
		})
	}
}

func TestResolverProfilesCannotAliasHyphenatedRouteFamilies(t *testing.T) {
	for _, routeFamily := range []string{
		"wiley-doi-pdfdirect",
		"sage-doi-pdf",
		"sciencedirect-pii-pdfft",
	} {
		t.Run(routeFamily, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			data := []byte("access_mode = \"conservative\"\n[browser.resolvers." + routeFamily + "]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\n")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("hyphenated route family %q was accepted as an institution profile", routeFamily)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("access_mode = \"conservative\"\n[browser.resolvers.wileydoipdfdirect1]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("validated alphanumeric profile rejected: %v", err)
	} else if _, ok := cfg.InstitutionFor("wileydoipdfdirect1"); !ok {
		t.Fatal("validated alphanumeric institution profile is not selectable")
	}
}

func TestResolverProfilesRejectEveryPackagedProviderHint(t *testing.T) {
	for _, hint := range routes.ProviderHintNames() {
		t.Run(hint, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.Browser.Resolvers = map[string]Institution{
				hint: {OpenURLBase: "https://resolver.example.edu/openurl"},
			}
			err := cfg.validate()
			if err == nil {
				t.Fatalf("packaged provider hint %q was accepted as an institution profile", hint)
			}
			if !strings.Contains(err.Error(), "provider route hint") {
				t.Fatalf("collision error for %q is not actionable: %v", hint, err)
			}
		})
	}
}

func TestBrowserResolverRejectsReservedDefaultName(t *testing.T) {
	// [browser.resolvers.default] is the exact config path that must be rejected:
	// InstitutionFor short-circuits name == "default" to the top-level Browser
	// fields, so a map entry under this key is unreachable and causes
	// ResolverNames() to duplicate "default" when the top-level base is also set.
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("access_mode = \"conservative\"\n[browser.resolvers.default]\nopenurl_base_url = \"https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected config with [browser.resolvers.default] to be rejected")
	}
	// The error must name the reserved config path and explain the alternative.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "browser.resolvers.default") {
		t.Fatalf("error should name the config path browser.resolvers.default, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "reserved") {
		t.Fatalf("error should say the name is reserved, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "top-level") {
		t.Fatalf("error should point to top-level fields, got: %s", errMsg)
	}
}

func TestBrowserResolverProfileRejectsInvalidAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("access_mode = \"conservative\"\n[browser.resolvers.institute]\nopenurl_base_url = \"https://onesearch.library.example-institute.edu/discovery/openurl\"\nproquest_account_id = \"nan\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("non-numeric named-profile account id accepted")
	}
}

func TestBrowserResolverProfilesAbsentKeepsLegacyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode = \"conservative\"\n[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Resolvers != nil {
		t.Fatalf("resolvers = %v, want nil", cfg.Browser.Resolvers)
	}
	if got, ok := cfg.OpenURLBaseFor(""); !ok || got != "https://resolver.example.edu/openurl" {
		t.Fatalf("legacy default = %q, %t", got, ok)
	}
}

func TestBrowserResolverStringShorthandLoads(t *testing.T) {
	// The pre-1.0 shorthand `name = "https://…"` must keep loading as a
	// base-only institution so existing configs need no migration.
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("access_mode = \"conservative\"\n[browser.resolvers]\nexample = 'https://example.alma.exlibrisgroup.com/view/uresolver/61EXL_INST/openurl?svc_dat=viewit'\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("string shorthand rejected: %v", err)
	}
	inst, ok := cfg.InstitutionFor("example")
	if !ok || inst.OpenURLBase != "https://example.alma.exlibrisgroup.com/view/uresolver/61EXL_INST/openurl?svc_dat=viewit" {
		t.Fatalf("example shorthand = %+v, %t", inst, ok)
	}
	if inst.ShibbolethEntityID != "" || inst.ProquestAccountID != "" {
		t.Fatalf("shorthand should leave login identity empty: %+v", inst)
	}
}

func TestResolverOrigins(t *testing.T) {
	cfg := Config{Browser: Browser{
		OpenURLBase: "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE",
		Resolvers: map[string]Institution{
			"institute": {OpenURLBase: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
			"dupe":      {OpenURLBase: "https://example.primo.exlibrisgroup.com/other/openurl"},
			"blank":     {OpenURLBase: ""},
		},
	}}
	got := cfg.ResolverOrigins()
	want := []string{"https://example.primo.exlibrisgroup.com", "https://onesearch.library.example-institute.edu"}
	if len(got) != len(want) {
		t.Fatalf("ResolverOrigins() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolverOrigins()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolverOriginsCanonicalize(t *testing.T) {
	cfg := Config{Browser: Browser{
		OpenURLBase: "https://MixedCase.Example.EDU:443/openurl",
		Resolvers: map[string]Institution{
			"ported":  {OpenURLBase: "https://library.example.edu:8443/openurl"},
			"badport": {OpenURLBase: "https://bad.example.edu:99999/openurl"},
		},
	}}
	got := cfg.ResolverOrigins()
	want := []string{"https://library.example.edu:8443", "https://mixedcase.example.edu"}
	if len(got) != len(want) {
		t.Fatalf("ResolverOrigins() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolverOrigins()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolverOriginsCap(t *testing.T) {
	resolvers := make(map[string]Institution)
	for i := range 40 {
		id := strconv.Itoa(100 + i)
		resolvers["r"+id] = Institution{OpenURLBase: "https://lib" + id + ".example.edu/openurl"}
	}
	cfg := Config{Browser: Browser{Resolvers: resolvers}}
	if got := cfg.ResolverOrigins(); len(got) != 32 {
		t.Fatalf("ResolverOrigins() len = %d, want 32 (protocol cap)", len(got))
	}
}

func TestChromiumExtensionIDsDedupesPrimaryFirst(t *testing.T) {
	b := Browser{ExtensionID: "aaaa", ExtensionIDs: []string{"bbbb", "aaaa", "", "cccc"}}
	got := b.ChromiumExtensionIDs()
	want := []string{"aaaa", "bbbb", "cccc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ChromiumExtensionIDs = %v, want %v", got, want)
	}
	if ids := (Browser{}).ChromiumExtensionIDs(); len(ids) != 0 {
		t.Fatalf("empty browser returned %v", ids)
	}
}

func TestSaveRejectsInvalidExtensionIDs(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Browser.ExtensionID = "abcdefghijklmnopabcdefghijklmnop"
	cfg.Browser.ExtensionIDs = []string{"not-a-valid-id"}
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
		t.Fatal("invalid browser.extension_ids entry accepted")
	}
}

func TestSaveAcceptsMultipleExtensionIDs(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Browser.ExtensionID = "abcdefghijklmnopabcdefghijklmnop"
	cfg.Browser.ExtensionIDs = []string{"ponmlkjihgfedcbaponmlkjihgfedcba"}
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("valid browser.extension_ids rejected: %v", err)
	}
}

func TestLibrarySourceValidation(t *testing.T) {
	valid := func() Config {
		cfg := Default()
		cfg.AccessMode = ModeConservative
		cfg.Library.Sources = []LibrarySource{{
			Name:   "owned-pdfs",
			Kind:   LibraryKindFile,
			Path:   "/tmp/owned.bib",
			Format: "bibtex",
			Claim:  LibraryClaimPDFPresent,
		}}
		return cfg
	}

	tooMany := make([]LibrarySource, MaxLibrarySources+1)
	for i := range tooMany {
		tooMany[i] = LibrarySource{
			Name:  "source-" + strconv.Itoa(i),
			Kind:  LibraryKindFile,
			Path:  "/tmp/owned.bib",
			Claim: LibraryClaimPDFPresent,
		}
	}

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "missing name",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Name = ""
				return cfg
			}(),
			want: "name is required",
		},
		{
			name: "duplicate name",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources = append(cfg.Library.Sources, cfg.Library.Sources[0])
				return cfg
			}(),
			want: "lists name \"owned-pdfs\" twice",
		},
		{
			name: "unsupported kind",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Kind = "command"
				return cfg
			}(),
			want: "kind \"command\" is not supported",
		},
		{
			name: "missing path",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Path = " "
				return cfg
			}(),
			want: "path is required",
		},
		{
			name: "unsupported format",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Format = "jsonl"
				return cfg
			}(),
			want: "format \"jsonl\" must be empty or one of",
		},
		{
			name: "missing claim",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Claim = ""
				return cfg
			}(),
			want: "claim must be",
		},
		{
			name: "unsupported claim",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources[0].Claim = "pdf_missing"
				return cfg
			}(),
			want: "claim must be",
		},
		{
			name: "too many sources",
			cfg: func() Config {
				cfg := valid()
				cfg.Library.Sources = tooMany
				return cfg
			}(),
			want: "maximum 8",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Save(test.cfg, filepath.Join(t.TempDir(), "config.toml"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Save error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLibrarySourcesRoundTrip(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Library.Sources = []LibrarySource{
		{
			Name:   "owned-pdfs",
			Kind:   LibraryKindFile,
			Path:   "~/library/owned.bib",
			Format: "bibtex",
			Claim:  LibraryClaimPDFPresent,
		},
		{
			Name:   "citations",
			Kind:   LibraryKindFile,
			Path:   filepath.Join(t.TempDir(), "citations.ris"),
			Format: "ris",
			Claim:  LibraryClaimRecordPresent,
		},
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []LibrarySource{
		{
			Name:   "owned-pdfs",
			Kind:   LibraryKindFile,
			Path:   filepath.Join(home, "library", "owned.bib"),
			Format: "bibtex",
			Claim:  LibraryClaimPDFPresent,
		},
		cfg.Library.Sources[1],
	}
	if !reflect.DeepEqual(got.Library.Sources, want) {
		t.Fatalf("library.sources = %#v, want %#v", got.Library.Sources, want)
	}
}

// TestEffectiveAccessModeIsAMonotoneCeiling pins both halves of the access-mode
// resolution, including the two cases a review caught after the first pass:
// tightening the configuration must restrain jobs already recorded, and an
// unset configuration must not silently discard the mode a job was recorded
// with.
func TestEffectiveAccessModeIsAMonotoneCeiling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		policy     string
		want       string
	}{
		{name: "no policy follows the configuration", configured: ModeAssisted, policy: "", want: ModeAssisted},
		{name: "narrowing policy is honoured", configured: ModeDelegated, policy: ModeConservative, want: ModeConservative},
		{name: "widening policy is refused", configured: ModeConservative, policy: ModeDelegated, want: ModeConservative},
		{name: "equal policy is a no-op", configured: ModeAssisted, policy: ModeAssisted, want: ModeAssisted},
		{
			// The job was recorded under delegated; the operator has since
			// revoked it. The revocation must reach work already queued.
			name:       "tightening the configuration restrains an existing job",
			configured: ModeConservative, policy: ModeDelegated, want: ModeConservative,
		},
		{
			// Deleting access_mode is the most drastic tightening an operator
			// can perform. Resolving to the job's own recorded mode would make
			// that gesture the one setting that WIDENS, leaving already-recorded
			// delegated jobs unrestrained, so an absent ceiling fails closed.
			name:       "an unset configuration fails closed rather than trusting the job",
			configured: "", policy: ModeDelegated, want: ModeConservative,
		},
		{
			// An unreadable snapshot is not evidence of permission: corrupt or
			// hand-edited policy_json must not inherit whatever the daemon allows.
			name:       "a garbage policy mode fails closed",
			configured: ModeAssisted, policy: "maximal", want: ModeConservative,
		},
		{name: "surrounding whitespace is tolerated", configured: ModeDelegated, policy: "  conservative  ", want: ModeConservative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{AccessMode: tc.configured}
			if got := cfg.EffectiveAccessMode(tc.policy); got != tc.want {
				t.Fatalf("EffectiveAccessMode(%q) with configured %q = %q, want %q",
					tc.policy, tc.configured, got, tc.want)
			}
		})
	}
}

func TestLibKeyConfigValidationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		wantErr string // empty = accept
	}{
		{
			name: "link mode with a library id on the default profile",
			body: "[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\nlibkey_mode = \"link\"\nlibkey_library_id = 1234\n",
		},
		{
			name: "link mode with a library id on a named profile",
			body: "[browser.resolvers.campus]\nopenurl_base_url = \"https://campus.example.edu/openurl\"\nlibkey_mode = \"link\"\nlibkey_library_id = 1234\n",
		},
		{
			name:    "link mode without a library id",
			body:    "[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\nlibkey_mode = \"link\"\n",
			wantErr: "libkey_library_id",
		},
		{
			name:    "library id without link mode is silently-dead config",
			body:    "[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\nlibkey_library_id = 1234\n",
			wantErr: "libkey_library_id is set",
		},
		{
			name:    "api mode is not implemented",
			body:    "[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\nlibkey_mode = \"api\"\n",
			wantErr: "not implemented",
		},
		{
			name:    "unknown mode",
			body:    "[browser]\nopenurl_base_url = \"https://resolver.example.edu/openurl\"\nlibkey_mode = \"nomad\"\n",
			wantErr: "libkey_mode must be",
		},
		{
			name:    "named-profile errors carry the profile name",
			body:    "[browser.resolvers.campus]\nopenurl_base_url = \"https://campus.example.edu/openurl\"\nlibkey_mode = \"link\"\n",
			wantErr: "browser.resolvers.campus.libkey_mode",
		},
		{
			name:    "link mode without an OpenURL base is silently-dead config",
			body:    "[browser]\nlibkey_mode = \"link\"\nlibkey_library_id = 1234\n",
			wantErr: "requires browser.openurl_base_url",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("access_mode = \"conservative\"\n"+test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("valid LibKey config rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestInstitutionForMirrorsDefaultProfileLibKeyFields(t *testing.T) {
	cfg := Config{Browser: Browser{
		OpenURLBase:     "https://resolver.example.edu/openurl",
		LibKeyMode:      "link",
		LibKeyLibraryID: 1234,
	}}
	for _, name := range []string{"", "default"} {
		inst, ok := cfg.InstitutionFor(name)
		if !ok || inst.LibKeyMode != "link" || inst.LibKeyLibraryID != 1234 {
			t.Fatalf("InstitutionFor(%q) = %+v, %t — the default profile must carry the top-level LibKey fields", name, inst, ok)
		}
	}
}

func TestDocumentDeliveryConfigValidationFailsClosed(t *testing.T) {
	validIlliadDefault := "[browser]\n" +
		"openurl_base_url = \"https://resolver.example.edu/openurl\"\n" +
		"[browser.document_delivery]\n" +
		"kind = \"illiad\"\n" +
		"base_url = \"https://illiad.example.edu/ILLiadWebPlatform\"\n" +
		"allowed_hosts = [\"illiad.example.edu\", \"illiadweb.example.edu\"]\n" +
		"submit_policy = \"auto_if_unconditional\"\n" +
		"request_classes = [\"digital_journal_article\"]\n" +
		"legal_basis = \"institution_policy\"\n" +
		"patron_attestation = \"standing_completed\"\n" +
		"patron_fee_policy = \"zero_standard\"\n" +
		"monthly_request_cap = 25\n" +
		"status_poll_minutes = 60\n" +
		"api_key = \"secret-key\"\n" +
		"patron_ref = \"configured-non-secret-reference\"\n"
	validIlliadNamed := "[browser.resolvers.campus]\n" +
		"openurl_base_url = \"https://campus.example.edu/openurl\"\n" +
		"[browser.resolvers.campus.document_delivery]\n" +
		"kind = \"illiad\"\n" +
		"base_url = \"https://illiad.example.edu/ILLiadWebPlatform\"\n" +
		"allowed_hosts = [\"illiad.example.edu\"]\n" +
		"submit_policy = \"auto_if_unconditional\"\n" +
		"request_classes = [\"digital_journal_article\"]\n" +
		"legal_basis = \"institution_policy\"\n" +
		"patron_attestation = \"standing_completed\"\n" +
		"patron_fee_policy = \"zero_standard\"\n" +
		"monthly_request_cap = 25\n" +
		"status_poll_minutes = 60\n" +
		"api_key = \"secret-key\"\n" +
		"patron_ref = \"configured-non-secret-reference\"\n"
	for _, test := range []struct {
		name    string
		body    string
		wantErr string // empty = accept
	}{
		{
			name: "full valid illiad block on the default profile",
			body: validIlliadDefault,
		},
		{
			name: "full valid illiad block on a named profile",
			body: validIlliadNamed,
		},
		{
			name: "empty allowed_hosts remains permissive",
			body: "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://unlisted.example.edu/request\"\nallowed_hosts = []\n",
		},
		{
			name: "listed base host is accepted",
			body: "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://forms.example.edu:8443/request\"\nallowed_hosts = [\"forms.example.edu\"]\n",
		},
		{
			name:    "base host outside the allowlist is rejected",
			body:    "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://unlisted.example.edu/request\"\nallowed_hosts = [\"forms.example.edu\"]\n",
			wantErr: "base_url host \"unlisted.example.edu\" is not allowed by allowed_hosts",
		},
		{
			name: "listed host port is accepted",
			body: "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://forms.example.edu:8443/request\"\nallowed_hosts = [\"forms.example.edu:8443\"]\n",
		},
		{
			name:    "scheme in allowed host is rejected",
			body:    "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://forms.example.edu/request\"\nallowed_hosts = [\"https://forms.example.edu\"]\n",
			wantErr: "allowed_hosts entry \"https://forms.example.edu\" must be a bare hostname",
		},
		{
			name:    "path in allowed host is rejected",
			body:    "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://forms.example.edu/request\"\nallowed_hosts = [\"forms.example.edu/request\"]\n",
			wantErr: "allowed_hosts entry \"forms.example.edu/request\" must be a bare hostname",
		},
		{
			name: "allowed host matching is case insensitive",
			body: "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"https://FORMS.Example.EDU/request\"\nallowed_hosts = [\"forms.example.edu\"]\n",
		},
		{
			name:    "patron web host outside the allowlist is rejected",
			body:    "[browser.document_delivery]\nkind = \"illiad\"\nbase_url = \"https://illiad.example.edu/ILLiadWebPlatform\"\npatron_web_base_url = \"https://web.example.edu/illiad/illiad.dll\"\nallowed_hosts = [\"illiad.example.edu\"]\n",
			wantErr: "patron_web_base_url host \"web.example.edu\" is not allowed by allowed_hosts",
		},
		{
			name:    "oclc kind is not implemented",
			body:    "[browser.document_delivery]\nkind = \"oclc\"\n",
			wantErr: "not implemented",
		},
		{
			name:    "unknown kind",
			body:    "[browser.document_delivery]\nkind = \"tipasa\"\n",
			wantErr: "not a recognized document-delivery kind",
		},
		{
			name:    "missing kind",
			body:    "[browser.document_delivery]\nbase_url = \"https://ill.example.edu/request\"\n",
			wantErr: "kind is required",
		},
		{
			name:    "api_key on a form-kind profile is dead config",
			body:    "[browser.document_delivery]\nkind = \"openurl\"\napi_key = \"secret\"\n",
			wantErr: "api_key is set but kind is",
		},
		{
			name:    "auto_if_unconditional on libkey kind is not auto-capable",
			body:    "[browser.document_delivery]\nkind = \"libkey\"\nsubmit_policy = \"auto_if_unconditional\"\n",
			wantErr: "requires kind \"illiad\"",
		},
		{
			name:    "unknown request class",
			body:    "[browser.document_delivery]\nkind = \"illiad\"\nrequest_classes = [\"ill_book\"]\n",
			wantErr: "not modelled yet",
		},
		{
			name:    "http base_url is rejected",
			body:    "[browser.document_delivery]\nkind = \"custom\"\nbase_url = \"http://ill.example.edu/request\"\n",
			wantErr: "base_url must be an absolute https URL",
		},
		{
			name:    "named-profile errors carry the profile name",
			body:    "[browser.resolvers.campus]\nopenurl_base_url = \"https://campus.example.edu/openurl\"\n[browser.resolvers.campus.document_delivery]\nkind = \"oclc\"\n",
			wantErr: "browser.resolvers.campus.document_delivery.kind",
		},
		{
			name: "patron_web_base_url configured on a valid illiad profile is accepted (absent is also legal, per the base fixtures above)",
			body: validIlliadDefault + "patron_web_base_url = \"https://illiadweb.example.edu/illiad/illiad.dll\"\n",
		},
		{
			name:    "patron_web_base_url http is rejected",
			body:    "[browser.document_delivery]\nkind = \"illiad\"\npatron_web_base_url = \"http://illiadweb.example.edu/illiad/illiad.dll\"\n",
			wantErr: "patron_web_base_url must be an absolute https URL",
		},
		{
			name:    "patron_web_base_url junk is rejected",
			body:    "[browser.document_delivery]\nkind = \"illiad\"\npatron_web_base_url = \"not a url\"\n",
			wantErr: "patron_web_base_url must be an absolute https URL",
		},
		{
			name:    "patron_web_base_url on a form-kind profile is dead config",
			body:    "[browser.document_delivery]\nkind = \"openurl\"\npatron_web_base_url = \"https://illiadweb.example.edu/illiad/illiad.dll\"\n",
			wantErr: "patron_web_base_url is set but kind is",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("access_mode = \"conservative\"\n"+test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("valid document_delivery config rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestInstitutionForMirrorsDefaultProfileDocumentDelivery(t *testing.T) {
	dd := &DocumentDelivery{Kind: "illiad", BaseURL: "https://illiad.example.edu/ILLiadWebPlatform"}
	cfg := Config{Browser: Browser{
		OpenURLBase:      "https://resolver.example.edu/openurl",
		DocumentDelivery: dd,
	}}
	for _, name := range []string{"", "default"} {
		inst, ok := cfg.InstitutionFor(name)
		if !ok || inst.DocumentDelivery != dd {
			t.Fatalf("InstitutionFor(%q) = %+v, %t — the default profile must carry the top-level DocumentDelivery pointer", name, inst, ok)
		}
	}
}
func TestTimeoutFieldsRejectAboveCeiling(t *testing.T) {
	// saveWith validates via Save, reusing the same error strings Load would surface.
	mustAccept := func(t *testing.T, cfg Config) {
		t.Helper()
		if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
			t.Fatalf("expected valid config, got error: %v", err)
		}
	}
	mustReject := func(t *testing.T, cfg Config, wantErr string) {
		t.Helper()
		err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Save err = %v, want an error containing %q", err, wantErr)
		}
	}
	base := func() Config {
		cfg := Default()
		cfg.AccessMode = ModeConservative
		return cfg
	}

	t.Run("fetch.timeout_seconds", func(t *testing.T) {
		cfg := base()
		cfg.Fetch.TimeoutSeconds = 3600
		mustAccept(t, cfg)

		cfg = base()
		cfg.Fetch.TimeoutSeconds = 3601
		mustReject(t, cfg, "fetch.timeout_seconds must be in 5..3600")

		cfg = base()
		cfg.Fetch.TimeoutSeconds = math.MaxInt64
		mustReject(t, cfg, "fetch.timeout_seconds must be in 5..3600")
	})

	t.Run("browser.action_expiry_seconds", func(t *testing.T) {
		cfg := base()
		cfg.Browser.ActionExpirySeconds = 2592000
		mustAccept(t, cfg)

		cfg = base()
		cfg.Browser.ActionExpirySeconds = 2592001
		mustReject(t, cfg, "browser.action_expiry_seconds must be in 0..2592000")

		cfg = base()
		cfg.Browser.ActionExpirySeconds = math.MaxInt64
		mustReject(t, cfg, "browser.action_expiry_seconds must be in 0..2592000")
	})

	t.Run("actions.stale_after_seconds", func(t *testing.T) {
		cfg := base()
		cfg.Actions.StaleAfterSeconds = 31536000
		mustAccept(t, cfg)

		cfg = base()
		cfg.Actions.StaleAfterSeconds = 31536001
		mustReject(t, cfg, "actions.stale_after_seconds must be in 0..31536000")

		cfg = base()
		cfg.Actions.StaleAfterSeconds = math.MaxInt64
		mustReject(t, cfg, "actions.stale_after_seconds must be in 0..31536000")
	})
}
