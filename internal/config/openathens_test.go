// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAthensDomainLoadsPerProfileAndIsNotInherited(t *testing.T) {
	cfg, err := loadConfigText(t, `access_mode = "conservative"
[browser]
openurl_base_url = "https://example.primo.exlibrisgroup.com/nde/openurl"
openathens_domain = "example.edu"

[browser.resolvers.institute]
openurl_base_url = "https://onesearch.library.example-institute.edu/discovery/openurl"
openathens_domain = "institute.example.ac.uk"

[browser.resolvers.plain]
openurl_base_url = "https://plain.example.edu/openurl"
`)
	if err != nil {
		t.Fatal(err)
	}
	def, _ := cfg.InstitutionFor("")
	institute, _ := cfg.InstitutionFor("institute")
	plain, _ := cfg.InstitutionFor("plain")
	if def.OpenAthensDomain != "example.edu" || institute.OpenAthensDomain != "institute.example.ac.uk" {
		t.Fatalf("default=%q institute=%q", def.OpenAthensDomain, institute.OpenAthensDomain)
	}
	if plain.OpenAthensDomain != "" {
		t.Fatalf("a profile without the field inherited %q from the default institution", plain.OpenAthensDomain)
	}
}

func TestOpenAthensDomainValidationFailsClosed(t *testing.T) {
	good := []string{"example.edu", "example.edu.au", "a-b.example.ac.uk", "x1.example.org", "xn--bcher-kva.example"}
	bad := []string{
		"Example.edu",                     // uppercase
		"example",                         // one label is not a tenant domain
		"https://example.edu",             // scheme
		"example.edu/redirector",          // slash
		"example.edu:8443",                // port
		"example.edu?url=x",               // query
		"example.edu#frag",                // fragment
		"user@example.edu",                // userinfo
		"example.edu.",                    // trailing dot
		".example.edu",                    // leading dot
		"example..edu",                    // empty label
		"-example.edu",                    // label starts with a hyphen
		"example-.edu",                    // label ends with a hyphen
		"exa mple.edu",                    // whitespace
		"example.edu%2f",                  // percent escape
		"exämple.edu",                     // non-ASCII
		strings.Repeat("a", 64) + ".edu",  // label over 63 bytes
		strings.Repeat("a.", 130) + "edu", // name over 253 bytes
	}
	for _, domain := range good {
		for field, text := range map[string]string{
			"browser.openathens_domain":                   "[browser]\nopenathens_domain = \"" + domain + "\"\n",
			"browser.resolvers.example.openathens_domain": "[browser.resolvers.example]\nopenurl_base_url = \"https://example.edu/openurl\"\nopenathens_domain = \"" + domain + "\"\n",
		} {
			if _, err := loadConfigText(t, "access_mode = \"conservative\"\n"+text); err != nil {
				t.Errorf("%s %q rejected: %v", field, domain, err)
			}
		}
	}
	for _, domain := range bad {
		for field, text := range map[string]string{
			"browser.openathens_domain":                   "[browser]\nopenathens_domain = \"" + domain + "\"\n",
			"browser.resolvers.example.openathens_domain": "[browser.resolvers.example]\nopenurl_base_url = \"https://example.edu/openurl\"\nopenathens_domain = \"" + domain + "\"\n",
		} {
			_, err := loadConfigText(t, "access_mode = \"conservative\"\n"+text)
			if err == nil {
				t.Errorf("%s %q accepted", field, domain)
				continue
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("%s %q error %q does not name the field", field, domain, err)
			}
		}
		if ValidateOpenAthensDomain(domain) == nil {
			t.Errorf("ValidateOpenAthensDomain(%q) accepted", domain)
		}
	}
}

// Save validates the same rules, so `papio config` writers cannot persist a
// value that Load would refuse.
func TestSaveRejectsInvalidOpenAthensDomain(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Browser.OpenAthensDomain = "https://example.edu"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
		t.Fatal("Save accepted a URL as the OpenAthens domain")
	}
	cfg.Browser.OpenAthensDomain = "example.edu"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatalf("Save rejected a valid domain: %v", err)
	}
}
