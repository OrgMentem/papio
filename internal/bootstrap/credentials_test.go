// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/doctor"
	"papio/internal/notify"
	"papio/internal/resolver"
	"papio/internal/runtimecredential"
	"papio/internal/store/storetest"
	"papio/internal/work"

	"github.com/pelletier/go-toml/v2"
)

func TestRuntimeCredentialsReachProductionClientsWithoutConfigHydration(t *testing.T) {
	cfg := allSourcesEnabled(t)
	t.Setenv("PAPIO_TEST_OPENALEX", "synthetic-openalex-account")
	t.Setenv("PAPIO_TEST_S2", "synthetic-s2-account")
	t.Setenv("PAPIO_TEST_OPENAIRE", `{"version":1,"kind":"openaire_client","client_id":"synthetic-client","client_secret":"synthetic-secret"}`)
	t.Setenv("PAPIO_TYPESAFE_API_KEY", "")
	for _, binding := range []struct{ source, ref string }{
		{config.SourceOpenAlex, "env:PAPIO_TEST_OPENALEX"},
		{config.SourceSemanticScholar, "env:PAPIO_TEST_S2"},
		{config.SourceOpenAIRE, "env:PAPIO_TEST_OPENAIRE"},
	} {
		p := cfg.Sources[binding.source]
		p.CredentialRef = binding.ref
		cfg.Sources[binding.source] = p
	}
	// A wiring failure hits the local rejection path, never a real provider.
	p := cfg.Sources[config.SourceOpenAlex]
	p.BaseURLForDev = "http://127.0.0.1:9/works"
	cfg.Sources[config.SourceOpenAlex] = p
	system := newSystemForRoles(t, cfg)
	for _, name := range []string{"PAPIO_TEST_OPENALEX", "PAPIO_TEST_S2", "PAPIO_TEST_OPENAIRE", "PAPIO_TYPESAFE_API_KEY"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatal("secret remains in child environment")
		}
	}
	for _, raw := range []config.Config{cfg, system.Config, system.App.Config} {
		encoded, _ := toml.Marshal(raw)
		for _, secret := range []string{"synthetic-openalex-account", "synthetic-s2-account", "synthetic-client", "synthetic-secret"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("runtime key copied into saved config")
			}
		}
	}
	var alex resolver.Resolver
	for _, entry := range system.App.Resolvers {
		switch entry.Adapter.Name() {
		case config.SourceOpenAlex:
			alex = entry.Adapter
			if entry.Policy.APIKey != "synthetic-openalex-account" {
				t.Fatal("resolver policy lacks resolved key")
			}
		case config.SourceSemanticScholar:
			if entry.Policy.APIKey != "synthetic-s2-account" {
				t.Fatal("second resolver policy lacks its own key")
			}
		case config.SourceOpenAIRE:
			if !entry.Policy.HasClientCredentials() || entry.Policy.RatePerSec != 1.9 {
				t.Fatal("OpenAIRE runtime client pair/tier absent")
			}
		}
	}
	// Pause the actual account identity. The resolver must carry that key
	// onto its HTTP request, or the quota observer cannot reject it.
	policy := system.Credentials.SourcePolicy(config.SourceOpenAlex)
	if err := system.Budgets.Defer(context.Background(), "openalex_quota", policy, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := alex.Resolve(context.Background(), work.Work{DOI: "10.1234/credential-test"})
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("resolver did not use resolved wire identity: %v", err)
	}

}

type credentialHTTP struct {
	key   string
	calls int
}

func (c *credentialHTTP) Do(req *http.Request) (*http.Response, error) {
	c.key = req.Header.Get("x-api-key")
	c.calls++
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
}

func TestRuntimeDiscoveryUsesResolvedKeyAndItsBudgetIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.Discovery.Sources = []string{config.SourceSemanticScholar}
	p := cfg.Sources[config.SourceSemanticScholar]
	p.CredentialRef = "env:PAPIO_S2_DISCOVERY"
	cfg.Sources[config.SourceSemanticScholar] = p
	r := runtimecredential.Resolve(context.Background(), cfg, nil, nil, func(name string) (string, bool) { return "synthetic-discovery-key", name == "PAPIO_S2_DISCOVERY" })
	budgets := floorTestBudgets(t)
	inner := &credentialHTTP{}
	sources, err := discoverySources(cfg, budgets, inner, r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sources[0].Search(context.Background(), discovery.SearchParams{Query: "credential wiring"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || inner.key != "synthetic-discovery-key" {
		t.Fatal("discovery did not send resolved key")
	}
	if err := budgets.Defer(context.Background(), config.SourceSemanticScholar, r.SourcePolicy(config.SourceSemanticScholar), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = sources[0].Search(context.Background(), discovery.SearchParams{Query: "credential wiring"})
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) || inner.calls != 1 {
		t.Fatal("discovery budget identity diverges from actual credential")
	}
}

func TestRuntimeMissingRequiredCredentialDoesNotPreventBootstrap(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = storetest.DataDir(t)
	cfg.AccessMode = config.ModeConservative
	for _, name := range []string{config.SourceCORE, config.SourceCrossrefTDM} {
		p := cfg.Sources[name]
		p.Enabled = true
		p.CredentialRef = "env:PAPIO_MISSING_TEST_CREDENTIAL"
		cfg.Sources[name] = p
	}
	t.Setenv("PAPIO_MISSING_TEST_CREDENTIAL", "")
	t.Setenv("PAPIO_TYPESAFE_API_KEY", "")
	system := newSystemForRoles(t, cfg)
	for _, entry := range system.App.Resolvers {
		if entry.Adapter.Name() == config.SourceCORE || entry.Adapter.Name() == config.SourceCrossrefTDM {
			if entry.Policy.Enabled {
				t.Fatal("required source enabled without credential")
			}
		}
	}
	if system.App == nil || system.Discovery == nil {
		t.Fatal("unrelated acquisition unavailable")
	}
	// Diagnostics are tested with only local capability checks. They report
	// the missing selected secret, not a raw upstream or environment value.
	report := doctor.Run(context.Background(), cfg, nil, system.PDFCapability, "", nil, system.Credentials)
	found := false
	for _, check := range report.Checks {
		if check.Name == "credential:sources.core" {
			found = true
			if check.Status == doctor.Pass {
				t.Fatal("doctor reported missing credential ready")
			}
		}
	}
	if !found {
		t.Fatal("doctor omitted unresolved binding")
	}
}

func TestRuntimeWebhookRecordReachesSender(t *testing.T) {
	// The sender receives the secret only after the runtime has resolved the
	// entire endpoint record; URL tokens never need to return to configuration.
	cfg := config.Default()
	cfg.Notify.WebhookCredentialRef = "env:PAPIO_WEBHOOK_TEST"
	r := runtimecredential.Resolve(context.Background(), cfg, nil, nil, func(string) (string, bool) {
		return `{"version":1,"kind":"webhook","url":"https://example.org/private-token","bearer":"private-bearer"}`, true
	})
	endpoint, bearer := r.Webhook()
	sender := notify.NewWebhook(endpoint, bearer)
	if sender.URL != "https://example.org/private-token" || sender.Secret != "private-bearer" || cfg.Notify.WebhookURL != "" || cfg.Notify.WebhookSecret != "" {
		t.Fatal("webhook record was not resolved separately")
	}
}
