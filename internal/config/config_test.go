// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"errors"
	"os"
	"path/filepath"
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
		got.Zotio.AttachmentMode != "linked-file" || got.Path != path {
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

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\nunknown_option=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field accepted")
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

	for _, accountID := range []string{"17227x", strings.Repeat("1", 65)} {
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

func TestBrowserResolverProfilesRejectInvalidNameAndURL(t *testing.T) {
	for _, test := range []struct {
		name, profile, base string
	}{
		{name: "uppercase name", profile: "INSTITUTE", base: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
		{name: "http URL", profile: "institute", base: "http://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
		{name: "relative URL", profile: "institute", base: "/discovery/openurl"},
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
	data := []byte("access_mode = \"conservative\"\n[browser.resolvers]\nune = 'https://example.alma.exlibrisgroup.com/view/uresolver/61EXL_INST/openurl?svc_dat=viewit'\n")
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
