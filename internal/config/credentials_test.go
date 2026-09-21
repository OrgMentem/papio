// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"papio/internal/credential"
)

const credentialTestRef = "keyring:0123456789abcdef0123456789abcdef"

func credentialConfig() Config {
	cfg := Default()
	cfg.AccessMode = ModeAssisted
	cfg.Browser.OpenURLBase = "https://library.example.test/openurl"
	cfg.Browser.DocumentDelivery = &DocumentDelivery{Kind: "illiad", BaseURL: "https://library.example.test/illiad", APIKey: "default-test-secret"}
	cfg.Browser.Resolvers = map[string]Institution{
		"second": {OpenURLBase: "https://second.example.test/openurl", DocumentDelivery: &DocumentDelivery{Kind: "illiad", BaseURL: "https://second.example.test/illiad", APIKey: "second-test-secret"}},
	}
	return cfg
}

func TestCredentialReferencesRoundTripWithoutSecrets(t *testing.T) {
	cfg := credentialConfig()
	cfg.Sources[SourceOpenAlex] = Source{Enabled: true, APIKey: "openalex-test-secret"}
	cfg.Sources[SourceOpenAIRE] = Source{Enabled: true, ClientID: "client-test-id", ClientSecret: "client-test-secret", RatePerSec: OpenAIREKeylessRatePerSec, Burst: 1}
	cfg.Notify.WebhookURL = "https://hooks.example.test/private-test-token"
	cfg.Notify.WebhookSecret = "bearer-test-secret"
	original := cfg.clone()
	for _, binding := range cfg.CredentialBindings() {
		var err error
		cfg, err = cfg.WithCredentialReference(binding.Target, credentialTestRef)
		if err != nil {
			t.Fatalf("bind %s: %v", binding.Target, err)
		}
	}
	if cfg.Agent == nil || cfg.Agent.Backend != "typesafe" {
		t.Fatal("agent binding did not enroll profile")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"openalex-test-secret", "client-test-id", "client-test-secret", "default-test-secret", "second-test-secret", "private-test-token", "bearer-test-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("migrated config contains a legacy secret")
		}
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range loaded.CredentialBindings() {
		if b.Reference != credentialTestRef || b.Legacy != (credential.Record{}) {
			t.Errorf("binding %s did not round-trip without literals", b.Target)
		}
	}
	if loaded.SourcePolicy(SourceOpenAIRE).RatePerSec != OpenAIREKeylessRatePerSec {
		t.Fatal("unresolved reference promoted authenticated pacing")
	}
	resolved := loaded.Sources[SourceOpenAIRE]
	resolved.ClientID, resolved.ClientSecret = "id", "secret"
	if EffectiveSourcePolicy(SourceOpenAIRE, resolved).RatePerSec != OpenAIREAuthenticatedRatePerSec {
		t.Fatal("resolved pair failed to promote pacing")
	}
	if original.Sources[SourceOpenAlex].APIKey == "" || original.Browser.DocumentDelivery.APIKey == "" || original.Notify.WebhookURL == "" {
		t.Fatal("original profile mutated")
	}
}

func TestCredentialReferencesArePureAndExplicit(t *testing.T) {
	cfg := credentialConfig()
	cfg.Agent = &Agent{Backend: "typesafe", CredentialRef: "env:PAPIO_TEST_ABSENT_CREDENTIAL"}
	cfg.Notify.WebhookCredentialRef = credentialTestRef // nonexistent references are valid syntax
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Agent.CredentialRef != cfg.Agent.CredentialRef {
		t.Fatalf("pure config load: %v", err)
	}
	for _, b := range got.CredentialBindings() {
		if b.Target == "agent.typesafe" && b.Legacy != (credential.Record{}) {
			t.Fatal("catalog must not look up legacy TypeSafe secret")
		}
	}
}

func TestCredentialReferenceValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"bad source reference", func(c *Config) {
			s := c.Sources[SourceOpenAlex]
			s.CredentialRef = "keyring:bad-secret-must-not-echo"
			c.Sources[SourceOpenAlex] = s
		}},
		{"unsupported source", func(c *Config) {
			s := c.Sources[SourceUnpaywall]
			s.CredentialRef = credentialTestRef
			c.Sources[SourceUnpaywall] = s
		}},
		{"source mixed key", func(c *Config) {
			s := c.Sources[SourceOpenAlex]
			s.CredentialRef, s.APIKey = credentialTestRef, "bad-secret-must-not-echo"
			c.Sources[SourceOpenAlex] = s
		}},
		{"source mixed pair", func(c *Config) {
			s := c.Sources[SourceOpenAIRE]
			s.CredentialRef, s.ClientID = credentialTestRef, "bad-secret-must-not-echo"
			c.Sources[SourceOpenAIRE] = s
		}},
		{"agent not enrolled", func(c *Config) { c.Agent = &Agent{CredentialRef: credentialTestRef} }},
		{"agent invalid reference", func(c *Config) { c.Agent = &Agent{Backend: "typesafe", CredentialRef: "env:1BAD"} }},
		{"webhook mixed URL", func(c *Config) {
			c.Notify.WebhookCredentialRef, c.Notify.WebhookURL = credentialTestRef, "https://example.test/bad-secret-must-not-echo"
		}},
		{"webhook mixed bearer", func(c *Config) {
			c.Notify.WebhookCredentialRef, c.Notify.WebhookSecret = credentialTestRef, "bad-secret-must-not-echo"
		}},
		{"delivery mixed", func(c *Config) { c.Browser.DocumentDelivery.CredentialRef = credentialTestRef }},
		{"delivery non API", func(c *Config) {
			c.Browser.DocumentDelivery.Kind = "openurl"
			c.Browser.DocumentDelivery.APIKey = ""
			c.Browser.DocumentDelivery.CredentialRef = credentialTestRef
		}},
		{"named delivery mixed", func(c *Config) { c.Browser.Resolvers["second"].DocumentDelivery.CredentialRef = credentialTestRef }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := credentialConfig()
			tt.edit(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatal("invalid reference configuration accepted")
			}
			if strings.Contains(err.Error(), "bad-secret-must-not-echo") {
				t.Fatal("error revealed credential input")
			}
		})
	}
}

func TestCredentialBindingDeepCloneAndDetach(t *testing.T) {
	cfg := credentialConfig()
	cfg.Browser.ExtensionIDs = []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	cfg.Browser.DocumentDelivery.AllowedHosts = []string{"library.example.test"}
	cfg.Browser.Resolvers["second"].DocumentDelivery.RequestClasses = []string{"digital_journal_article"}
	before := cfg.clone()
	got, err := cfg.WithCredentialReference("browser.resolvers.second.document_delivery", credentialTestRef)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, before) || got.Browser.DocumentDelivery.APIKey != "default-test-secret" || got.Browser.Resolvers["second"].DocumentDelivery.APIKey != "" {
		t.Fatal("binding changed another integration or original configuration")
	}
	got.Browser.ExtensionIDs[0] = "changed"
	got.Browser.DocumentDelivery.AllowedHosts[0] = "changed"
	got.Browser.Resolvers["second"].DocumentDelivery.RequestClasses[0] = "changed"
	got.Sources[SourceOpenAlex] = Source{}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("nested clone aliases original")
	}
	for _, b := range cfg.CredentialBindings() {
		bound, err := cfg.WithCredentialReference(b.Target, credentialTestRef)
		if err != nil {
			t.Fatal(err)
		}
		detached, err := bound.WithCredentialReference(b.Target, "")
		if err != nil {
			t.Fatalf("detach %s: %v", b.Target, err)
		}
		for _, item := range detached.CredentialBindings() {
			if item.Target == b.Target && (item.Reference != "" || item.Legacy != (credential.Record{})) {
				t.Fatalf("detach %s kept credential", b.Target)
			}
		}
		if b.Target == "agent.typesafe" && detached.Agent != nil {
			t.Fatal("agent detach re-enabled legacy lookup")
		}
	}
	if _, err := cfg.WithCredentialReference("browser.resolvers.absent.document_delivery", credentialTestRef); err == nil {
		t.Fatal("unconfigured delivery accepted")
	}
}

func TestCredentialMigrationRefusesAmbiguousAndUnsupportedLiterals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source Source
	}{
		{SourceOpenAIRE, Source{APIKey: "token", ClientID: "id", ClientSecret: "secret"}},
		{SourceOpenAIRE, Source{ClientSecret: "secret"}},
		{SourceOpenAlex, Source{APIKey: "key", ClientID: "unexpected"}},
		{SourceUnpaywall, Source{APIKey: "unused"}},
	} {
		cfg := credentialConfig()
		cfg.Sources[tt.name] = tt.source
		if err := cfg.ValidateCredentialMigration(); err == nil {
			t.Errorf("migration accepted ambiguous/unsupported fields for %s", tt.name)
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("legacy load behavior changed: %v", err)
		}
	}
	cfg := credentialConfig()
	cfg.Sources[SourceOpenAIRE] = Source{APIKey: "token", ClientID: "id", ClientSecret: "secret"}
	for _, b := range cfg.CredentialBindings() {
		if b.Target == "sources.openaire" && (b.Legacy.APIKey != "token" || b.Legacy.ClientID != "id" || b.Legacy.ClientSecret != "secret") {
			t.Fatal("catalog silently discarded conflicting OpenAIRE fields")
		}
	}
}
