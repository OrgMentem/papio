// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package runtimecredential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/agentcredential"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/credential"
	"papio/internal/delivery"

	"github.com/pelletier/go-toml/v2"
)

type readerFunc func(context.Context, string) (credential.Record, error)

func (f readerFunc) Load(ctx context.Context, ref string) (credential.Record, error) {
	return f(ctx, ref)
}
func noEnvironment(string) (string, bool) { return "", false }
func testRef(n int) string                { return fmt.Sprintf("keyring:%032x", n) }
func bindSource(cfg *config.Config, name, ref string) {
	p := cfg.Sources[name]
	p.CredentialRef = ref
	p.Enabled = true
	cfg.Sources[name] = p
}
func statusFor(t *testing.T, r *Runtime, target string) credential.Status {
	t.Helper()
	for _, s := range r.Statuses() {
		if s.Target == target {
			return s
		}
	}
	t.Fatalf("status absent for %s", target)
	return credential.Status{}
}

func TestRuntimeCredentialPoliciesAndConfigRemainSeparate(t *testing.T) {
	cfg := config.Default()
	bindSource(&cfg, config.SourceOpenAlex, testRef(1))
	bindSource(&cfg, config.SourceOpenAIRE, testRef(2))
	cfg.Notify.WebhookCredentialRef = testRef(3)
	before, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]credential.Record{
		testRef(1): {Kind: credential.KindOpenAlex, APIKey: "sensitive-openalex-key"},
		testRef(2): {Kind: credential.KindOpenAIREClient, ClientID: "private-client-id", ClientSecret: "private-client-secret"},
		testRef(3): {Kind: credential.KindWebhook, URL: "https://example.org/private-url-token?secret=private-query", Bearer: "private-bearer"},
	}
	r := Resolve(context.Background(), cfg, readerFunc(func(_ context.Context, ref string) (credential.Record, error) { return records[ref], nil }), nil, noEnvironment)
	if r.SourcePolicy(config.SourceOpenAlex).APIKey != records[testRef(1)].APIKey {
		t.Fatal("resolved source key absent")
	}
	pair := r.SourcePolicy(config.SourceOpenAIRE)
	if !pair.HasClientCredentials() || pair.RatePerSec != 1.9 || pair.Burst != 5 {
		t.Fatal("resolved client pair did not select authenticated tier")
	}
	if cfg.SourcePolicy(config.SourceOpenAIRE).HasClientCredentials() || cfg.SourcePolicy(config.SourceOpenAIRE).RatePerSec == pair.RatePerSec {
		t.Fatal("runtime credentials or derived tier leaked into config")
	}
	endpoint, bearer := r.Webhook()
	if endpoint != records[testRef(3)].URL || bearer != records[testRef(3)].Bearer {
		t.Fatal("webhook lost endpoint or bearer")
	}
	after, _ := toml.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("resolution modified serializable config")
	}
	diagnostic, _ := json.Marshal(r)
	for _, secret := range []string{"sensitive-openalex-key", "private-client-id", "private-client-secret", "private-url-token", "private-query", "private-bearer"} {
		if strings.Contains(string(after), secret) || strings.Contains(string(diagnostic), secret) || strings.Contains(fmt.Sprintf("%#v", r), secret) {
			t.Fatal("secret escaped runtime view")
		}
	}
	// Another reference to the same account must not reset its quota identity.
	bindSource(&cfg, config.SourceOpenAlex, testRef(4))
	r2 := Resolve(context.Background(), cfg, readerFunc(func(_ context.Context, ref string) (credential.Record, error) {
		if ref == testRef(4) {
			return records[testRef(1)], nil
		}
		return records[ref], nil
	}), nil, noEnvironment)
	if budget.IdentityFor(r.SourcePolicy(config.SourceOpenAlex)) != budget.IdentityFor(r2.SourcePolicy(config.SourceOpenAlex)) {
		t.Fatal("credential reference changed quota identity")
	}
}

func TestRuntimeUnavailableCredentialsKeepOnlyKeylessPaths(t *testing.T) {
	for _, storeErr := range []error{credential.ErrNotFound, credential.ErrUnavailable} {
		cfg := config.Default()
		for i, name := range []string{config.SourceOpenAlex, config.SourceOpenAIRE, config.SourceSemanticScholar, config.SourceCORE, config.SourceCrossrefTDM} {
			bindSource(&cfg, name, testRef(i+1))
		}
		cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: testRef(6)}
		cfg.Notify.WebhookCredentialRef = testRef(7)
		r := Resolve(context.Background(), cfg, readerFunc(func(context.Context, string) (credential.Record, error) { return credential.Record{}, storeErr }), nil, noEnvironment)
		for _, name := range []string{config.SourceOpenAlex, config.SourceOpenAIRE, config.SourceSemanticScholar} {
			p := r.SourcePolicy(name)
			if !p.Enabled || p.APIKey != "" || p.HasClientCredentials() {
				t.Fatalf("keyless source %s misconfigured", name)
			}
		}
		if r.SourcePolicy(config.SourceOpenAIRE).RatePerSec != config.Default().Sources[config.SourceOpenAIRE].RatePerSec {
			t.Fatal("unresolved reference granted authenticated capacity")
		}
		for _, name := range []string{config.SourceCORE, config.SourceCrossrefTDM} {
			if r.SourcePolicy(name).Enabled {
				t.Fatal("required-key source remained enabled")
			}
		}
		if r.AgentKey() != "" || r.WebhookConfigured() {
			t.Fatal("unresolved required credential enabled integration")
		}
		if !r.SourcePolicy(config.SourceArXiv).Enabled {
			t.Fatal("unrelated acquisition disabled")
		}
	}
}

func TestRuntimeDeliveryBindingsRemainInstitutionSpecific(t *testing.T) {
	cfg := config.Default()
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{Kind: "illiad", CredentialRef: testRef(1), PatronRef: "default-patron"}
	cfg.Browser.Resolvers = make(map[string]config.Institution)
	cfg.Browser.Resolvers["institute"] = config.Institution{DocumentDelivery: &config.DocumentDelivery{Kind: "illiad", CredentialRef: testRef(2), PatronRef: "other-patron"}}
	r := Resolve(context.Background(), cfg, readerFunc(func(_ context.Context, ref string) (credential.Record, error) {
		return credential.Record{Kind: credential.KindILLiad, APIKey: "key-" + ref}, nil
	}), nil, noEnvironment)
	a, _ := r.InstitutionFor("default")
	b, _ := r.InstitutionFor("institute")
	if a.DocumentDelivery.APIKey == "" || a.DocumentDelivery.APIKey == b.DocumentDelivery.APIKey || b.DocumentDelivery.PatronRef != "other-patron" {
		t.Fatal("institution bindings mixed")
	}
	if delivery.CompileGateProfile(a, "default").CredentialFingerprint == delivery.CompileGateProfile(b, "institute").CredentialFingerprint {
		t.Fatal("delivery fingerprint ignores resolved key")
	}
	a.DocumentDelivery.APIKey = "modified"
	again, _ := r.InstitutionFor("")
	if again.DocumentDelivery.APIKey == "modified" || cfg.Browser.DocumentDelivery.APIKey != "" || cfg.Browser.Resolvers["institute"].DocumentDelivery.APIKey != "" {
		t.Fatal("shared configuration was hydrated or runtime result aliased")
	}
}

func TestRuntimeAgentLegacyAndReferencePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, ref, env, stored string
		envSet, enrolled       bool
		wantKey, wantState     string
		wantLegacy             bool
	}{
		{name: "legacy saved", enrolled: true, stored: "old-key", wantKey: "old-key", wantState: "ready", wantLegacy: true},
		{name: "legacy environment", enrolled: true, envSet: true, env: "env-key", wantKey: "env-key", wantState: "ready"},
		{name: "legacy empty disables", enrolled: true, envSet: true, wantState: "not_configured"},
		{name: "legacy invalid never falls back", enrolled: true, envSet: true, env: "bad\nkey", wantState: "invalid"},
		{name: "legacy missing", enrolled: true, wantState: "missing", wantLegacy: true},
		{name: "not enrolled", stored: "old-key", wantState: "not_configured"},
		{name: "explicit ref authoritative", ref: testRef(1), enrolled: true, envSet: true, env: "ignored-env", stored: "old-key", wantKey: "new-key", wantState: "ready"},
		{name: "missing ref never falls back", ref: testRef(2), enrolled: true, envSet: true, env: "ignored-env", stored: "old-key", wantState: "missing"},
		{name: "wrong kind never falls back", ref: testRef(3), enrolled: true, wantState: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Path = filepath.Join(t.TempDir(), "config.toml")
			cfg.DataDir = t.TempDir()
			if tc.enrolled {
				cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: tc.ref}
			}
			legacyCalls := 0
			r := Resolve(context.Background(), cfg, readerFunc(func(_ context.Context, ref string) (credential.Record, error) {
				if ref == testRef(1) {
					return credential.Record{Kind: credential.KindTypeSafe, APIKey: "new-key"}, nil
				}
				if ref == testRef(3) {
					return credential.Record{Kind: credential.KindCORE, APIKey: "wrong-kind"}, nil
				}
				return credential.Record{}, credential.ErrNotFound
			}), func(context.Context, string) (string, error) {
				legacyCalls++
				if tc.stored == "" {
					return "", agentcredential.ErrNotFound
				}
				return tc.stored, nil
			}, func(name string) (string, bool) { return tc.env, tc.envSet && name == legacyAgentEnv })
			if r.AgentKey() != tc.wantKey || statusFor(t, r, "agent.typesafe").State != tc.wantState || (legacyCalls == 1) != tc.wantLegacy {
				t.Fatalf("unexpected selection: state=%s legacy calls=%d", statusFor(t, r, "agent.typesafe").State, legacyCalls)
			}
		})
	}
}

func TestRuntimeConsumesAllSelectedEnvironmentBeforeVaultAccess(t *testing.T) {
	cfg := config.Default()
	bindSource(&cfg, config.SourceOpenAlex, "env:PAPIO_CREDENTIAL_TEST_SHARED")
	bindSource(&cfg, config.SourceSemanticScholar, "env:PAPIO_CREDENTIAL_TEST_SHARED")
	bindSource(&cfg, config.SourceCORE, testRef(1))
	t.Setenv("PAPIO_CREDENTIAL_TEST_SHARED", "synthetic-shared-key")
	t.Setenv(legacyAgentEnv, "")
	t.Setenv("PAPIO_CREDENTIAL_UNSELECTED", "unrelated")
	env, err := TakeEnvironment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := Resolve(context.Background(), cfg, readerFunc(func(context.Context, string) (credential.Record, error) {
		calls++
		for _, name := range []string{"PAPIO_CREDENTIAL_TEST_SHARED", legacyAgentEnv} {
			if _, ok := os.LookupEnv(name); ok {
				t.Fatal("selected credential inherited by vault helper")
			}
		}
		return credential.Record{}, errors.New("raw private error")
	}), nil, env)
	if calls != 1 {
		t.Fatal("vault binding not attempted")
	}
	for _, name := range []string{config.SourceOpenAlex, config.SourceSemanticScholar} {
		if r.SourcePolicy(name).APIKey != "synthetic-shared-key" {
			t.Fatal("shared environment consumed before all bindings could read it")
		}
	}
	if os.Getenv("PAPIO_CREDENTIAL_UNSELECTED") != "unrelated" {
		t.Fatal("unselected environment changed")
	}
}
