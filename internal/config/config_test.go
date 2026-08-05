// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Email = "researcher@example.test"
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Sources[SourceOpenAlex] = Source{Enabled: true, APIKey: "secret", RatePerSec: 2, Burst: 1}
	cfg.Zotio.Executable = filepath.Join(t.TempDir(), "zotio")
	cfg.Zotio.AttachmentMode = "linked-file"
	cfg.Hooks.OnReady = "true"
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
		got.Zotio.AttachmentMode != "linked-file" || got.Hooks.OnReady != "true" || got.Path != path {
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

func TestSaveValidatesShibbolethEntityID(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Browser.ShibbolethEntityID = "https://idp.example.edu/entity"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("valid Shibboleth entity ID rejected: %v", err)
	}

	for _, entityID := range []string{"http://idp.example.edu/entity", "https://"} {
		t.Run(entityID, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.Browser.ShibbolethEntityID = entityID
			if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
				t.Fatalf("invalid Shibboleth entity ID %q accepted", entityID)
			}
		})
	}
}

func TestSaveValidatesProquestAccountID(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Browser.ProquestAccountID = "12345"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("valid ProQuest account ID rejected: %v", err)
	}

	for _, accountID := range []string{"12345x", strings.Repeat("1", 65)} {
		t.Run(accountID, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.Browser.ProquestAccountID = accountID
			if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
				t.Fatalf("invalid ProQuest account ID %q accepted", accountID)
			}
		})
	}
}

func TestZotioAutoImportDefaultsOffAndLoadsTrue(t *testing.T) {
	if Default().Zotio.AutoImport {
		t.Fatal("default zotio.auto_import = true, want false")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[zotio]\nauto_import=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Zotio.AutoImport {
		t.Fatal("loaded zotio.auto_import = false, want true")
	}
}

func TestZotioAutoEnrichDefaultsOnAndLoadsFalse(t *testing.T) {
	if !Default().Zotio.AutoEnrich {
		t.Fatal("default zotio.auto_enrich = false, want true")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[zotio]\nauto_enrich=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Zotio.AutoEnrich {
		t.Fatal("loaded zotio.auto_enrich = true, want false")
	}
}

func TestNotifyDefaultsOnAndLoadsFalse(t *testing.T) {
	if !Default().Notify.Enabled {
		t.Fatal("default notify.enabled = false, want true")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[notify]\nenabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Enabled {
		t.Fatal("loaded notify.enabled = true, want false")
	}
}

func TestUpdatesCheckDefaultsOffAndLoadsTrue(t *testing.T) {
	if Default().Updates.Check {
		t.Fatal("default updates.check = true, want false")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[updates]\ncheck=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Updates.Check {
		t.Fatal("loaded updates.check = false, want true")
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
	if profile, ok := cfg.ResolverProfileForOrigin("https://onesearch.library.example-college.edu"); !ok || profile != "college" {
		t.Fatalf("origin profile = %q, %t, want college", profile, ok)
	}
	if profile, ok := cfg.ResolverProfileForOrigin("https://example.primo.exlibrisgroup.com"); !ok || profile != "default" {
		t.Fatalf("default origin profile = %q, %t, want default", profile, ok)
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
