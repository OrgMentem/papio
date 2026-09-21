// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/credential"
	"papio/internal/delivery"
	"papio/internal/pdf"
	"papio/internal/runtimecredential"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

type doctorCredentialReader func(context.Context, string) (credential.Record, error)

func (f doctorCredentialReader) Load(ctx context.Context, ref string) (credential.Record, error) {
	return f(ctx, ref)
}

const doctorCredentialRef = "keyring:00000000000000000000000000000001"

func credentialChecks(cfg config.Config, views ...CredentialView) Report {
	report := Report{OK: true}
	add := func(name, status, detail, remediation string) {
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
		if status == Fail {
			report.OK = false
		}
	}
	checkCredentials(cfg, add, views...)
	checkSourceCredentials(cfg, add, views...)
	checkNotifications(context.Background(), cfg, nil, add, views...)
	return report
}

func TestCredentialDiagnosticsUseResolvedOpenAIRETier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record credential.Record
		err    error
		state  string
		status string
		rate   float64
	}{
		{"client pair", credential.Record{Kind: credential.KindOpenAIREClient, ClientID: "PRIVATE_CLIENT_ID", ClientSecret: "PRIVATE_CLIENT_SECRET"}, nil, "ready", Pass, config.OpenAIREAuthenticatedRatePerSec},
		{"missing", credential.Record{}, credential.ErrNotFound, "missing", Warn, config.OpenAIREKeylessRatePerSec},
		{"unavailable", credential.Record{}, errors.New("raw backend contains PRIVATE_BACKEND_ERROR"), "unavailable", Warn, config.OpenAIREKeylessRatePerSec},
		{"wrong kind", credential.Record{Kind: credential.KindCORE, APIKey: "PRIVATE_WRONG_KIND_KEY"}, nil, "invalid", Warn, config.OpenAIREKeylessRatePerSec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Email = "reader@example.test"
			p := cfg.Sources[config.SourceOpenAIRE]
			p.CredentialRef = doctorCredentialRef
			cfg.Sources[config.SourceOpenAIRE] = p
			view := runtimecredential.Resolve(context.Background(), cfg, doctorCredentialReader(func(context.Context, string) (credential.Record, error) {
				return tc.record, tc.err
			}), nil, nil)
			report := credentialChecks(cfg, view)
			status, detail := checkStatus(t, report, "credential:sources.openaire")
			if status != tc.status || !strings.Contains(detail, tc.state+" (source: keyring)") {
				t.Fatalf("credential diagnostic = %s %s", status, detail)
			}
			_, tier := checkStatus(t, report, "source_openaire")
			wantTier := "60 requests/hour"
			if tc.state == "ready" {
				wantTier = "7,200 requests/hour"
			}
			if !strings.Contains(tier, wantTier) || view.SourcePolicy(config.SourceOpenAIRE).RatePerSec != tc.rate {
				t.Fatalf("diagnostic and resolved pacing disagree: %s", tier)
			}
			if cfg.Sources[config.SourceOpenAIRE].HasClientCredentials() || cfg.Sources[config.SourceOpenAIRE].RatePerSec != config.OpenAIREKeylessRatePerSec {
				t.Fatal("diagnostics changed the serializable config")
			}
			assertCredentialDiagnosticsPrivate(t, report, "PRIVATE_CLIENT_ID", "PRIVATE_CLIENT_SECRET", "PRIVATE_BACKEND_ERROR", "PRIVATE_WRONG_KIND_KEY")
		})
	}
}

func TestCredentialDiagnosticsRequiredAndOptionalFailures(t *testing.T) {
	cfg := config.Default()
	cfg.Email = "reader@example.test"
	for _, name := range []string{config.SourceCORE, config.SourceCrossrefTDM, config.SourceOpenAlex, config.SourceSemanticScholar} {
		p := cfg.Sources[name]
		p.Enabled, p.CredentialRef = true, doctorCredentialRef
		cfg.Sources[name] = p
	}
	cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: doctorCredentialRef}
	cfg.Notify.WebhookCredentialRef = doctorCredentialRef
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{Kind: "illiad", CredentialRef: doctorCredentialRef}
	cfg.Browser.Resolvers = map[string]config.Institution{"campus": {
		DocumentDelivery: &config.DocumentDelivery{Kind: "illiad", CredentialRef: doctorCredentialRef},
	}}
	view := runtimecredential.Resolve(context.Background(), cfg, doctorCredentialReader(func(context.Context, string) (credential.Record, error) {
		return credential.Record{}, credential.ErrNotFound
	}), nil, nil)
	report := credentialChecks(cfg, view)
	for _, target := range []string{"sources.core", "sources.crossref_tdm", "agent.typesafe", "notify.webhook", "browser.document_delivery", "browser.resolvers.campus.document_delivery"} {
		status, detail := checkStatus(t, report, "credential:"+target)
		if status != Fail || !strings.Contains(detail, "missing") {
			t.Errorf("%s = %s %s, want failed missing key", target, status, detail)
		}
	}
	for _, source := range []string{config.SourceCORE, config.SourceCrossrefTDM} {
		if view.SourcePolicy(source).Enabled {
			t.Fatal("fixture must disable the unresolved required source in runtime policy")
		}
	}
	for _, source := range []string{config.SourceOpenAlex, config.SourceSemanticScholar} {
		status, detail := checkStatus(t, report, "credential:sources."+source)
		if status != Warn || !strings.Contains(detail, "keyless") || !view.SourcePolicy(source).Enabled {
			t.Fatalf("optional key failure = %s %s", status, detail)
		}
	}
	if report.OK {
		t.Fatal("configured required integrations are unavailable, so doctor must fail")
	}
	_, notification := checkStatus(t, report, "notifications")
	if !strings.Contains(notification, "webhook unavailable") {
		t.Fatalf("missing webhook reference was described as configured: %s", notification)
	}
}

func TestCredentialDiagnosticsEnvironmentAndLegacySources(t *testing.T) {
	cfg := config.Default()
	cfg.Email = "reader@example.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true, CredentialRef: "env:PAPIO_DOCTOR_FIXTURE"}
	cfg.Sources[config.SourceCORE] = config.Source{Enabled: true, APIKey: "PRIVATE_LEGACY_KEY"}
	cfg.Notify.WebhookCredentialRef = doctorCredentialRef
	view := runtimecredential.Resolve(context.Background(), cfg, doctorCredentialReader(func(context.Context, string) (credential.Record, error) {
		return credential.Record{Kind: credential.KindWebhook, URL: "https://hooks.example.test/PRIVATE_URL_TOKEN?key=PRIVATE_QUERY_TOKEN", Bearer: "PRIVATE_WEBHOOK_BEARER"}, nil
	}), nil, func(name string) (string, bool) {
		return "PRIVATE_ENVIRONMENT_KEY", name == "PAPIO_DOCTOR_FIXTURE"
	})
	report := credentialChecks(cfg, view)
	for target, source := range map[string]string{"sources.openalex": "environment", "sources.core": "legacy", "notify.webhook": "keyring"} {
		status, detail := checkStatus(t, report, "credential:"+target)
		if status != Pass || !strings.Contains(detail, "ready (source: "+source+")") {
			t.Fatalf("%s = %s %s", target, status, detail)
		}
	}
	_, notification := checkStatus(t, report, "notifications")
	if !strings.Contains(notification, "webhook configured") {
		t.Fatalf("resolved webhook is missing: %s", notification)
	}
	status, detail := checkStatus(t, report, "credential:agent.typesafe")
	if status != Skip || !strings.Contains(detail, "not_configured (source: none)") {
		t.Fatalf("unused agent = %s %s", status, detail)
	}
	assertCredentialDiagnosticsPrivate(t, report, "PRIVATE_LEGACY_KEY", "PRIVATE_ENVIRONMENT_KEY", "PRIVATE_URL_TOKEN", "PRIVATE_QUERY_TOKEN", "PRIVATE_WEBHOOK_BEARER", "https://hooks.example.test")
}

func TestCredentialDiagnosticsOfflineDoesNotResolveReferences(t *testing.T) {
	cfg := documentDeliveryTestConfig(t.TempDir())
	cfg.Browser.DocumentDelivery.CredentialRef = doctorCredentialRef
	cfg.Browser.DocumentDelivery.APIKey = "PRIVATE_IGNORED_DELIVERY_KEY"
	cfg.Sources[config.SourceOpenAIRE] = config.Source{
		Enabled: true, CredentialRef: doctorCredentialRef, ClientID: "PRIVATE_IGNORED_ID", ClientSecret: "PRIVATE_IGNORED_SECRET",
		RatePerSec: config.OpenAIREKeylessRatePerSec, Burst: 1,
	}
	cfg.Sources[config.SourceCORE] = config.Source{Enabled: true, CredentialRef: "env:PAPIO_DOCTOR_UNUSED"}
	cfg.Notify.WebhookCredentialRef = doctorCredentialRef
	cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: doctorCredentialRef}
	view := doctorCredentials(cfg, nil)
	if p := view.SourcePolicy(config.SourceOpenAIRE); p.HasClientCredentials() || p.RatePerSec != config.OpenAIREKeylessRatePerSec {
		t.Fatal("an unchecked reference must not establish an authenticated policy")
	}
	inst, _ := view.InstitutionFor("default")
	if inst.DocumentDelivery.APIKey != "" || cfg.Browser.DocumentDelivery.APIKey == "" {
		t.Fatal("offline policy must clear a copied credential without mutating config")
	}
	report := credentialChecks(cfg)
	for _, target := range []string{"sources.openaire", "sources.core", "agent.typesafe", "notify.webhook", "browser.document_delivery"} {
		status, detail := checkStatus(t, report, "credential:"+target)
		if status != Skip || !strings.Contains(detail, "not_checked") {
			t.Errorf("unchecked %s = %s %s", target, status, detail)
		}
	}
	for _, source := range []string{"source_openaire", "source_core"} {
		status, _ := checkStatus(t, report, source)
		if status != Skip {
			t.Fatalf("%s must not imply its reference was resolved", source)
		}
	}
	checkDocumentDelivery(context.Background(), cfg, nil, func(name, status, detail, remediation string) {
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	})
	status, _ := checkStatus(t, report, "document_delivery:default:credentials")
	if status != Skip {
		t.Fatal("offline ILLiad credential check must be skipped")
	}
	assertCredentialDiagnosticsPrivate(t, report, "PRIVATE_IGNORED_DELIVERY_KEY", "PRIVATE_IGNORED_ID", "PRIVATE_IGNORED_SECRET")
}

func TestCredentialDiagnosticsResolvedILLiadGateAndNamedIsolation(t *testing.T) {
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := documentDeliveryTestConfig(data)
	cfg.Browser.AdoptionRoot = filepath.Join(t.TempDir(), "papio")
	cfg.Browser.DocumentDelivery.APIKey = ""
	cfg.Browser.DocumentDelivery.CredentialRef = doctorCredentialRef
	named := *cfg.Browser.DocumentDelivery
	named.CredentialRef = "keyring:00000000000000000000000000000002"
	cfg.Browser.Resolvers = map[string]config.Institution{"campus": {DocumentDelivery: &named}}
	svc := delivery.New(db, &cfg, nil)
	for _, name := range []string{"default", "campus"} {
		if err := svc.RecordLiveAcceptance(ctx, name, "illiad"); err != nil {
			t.Fatal(err)
		}
	}
	view := runtimecredential.Resolve(ctx, cfg, doctorCredentialReader(func(_ context.Context, ref string) (credential.Record, error) {
		if ref == doctorCredentialRef {
			return credential.Record{Kind: credential.KindILLiad, APIKey: "PRIVATE_ILLIAD_KEY"}, nil
		}
		return credential.Record{}, credential.ErrNotFound
	}), nil, nil)
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil, view)
	status, detail := checkStatus(t, report, "document_delivery:default:result")
	if status != Pass || !strings.Contains(detail, "AUTO-CAPABLE") {
		t.Fatalf("resolved credential must compile the actual gate: %s %s", status, detail)
	}
	status, detail = checkStatus(t, report, "document_delivery:campus:result")
	if status != Warn || detail != "PREFILL ONLY" {
		t.Fatalf("named profile must not borrow the default key: %s %s", status, detail)
	}
	status, _ = checkStatus(t, report, "credential:browser.resolvers.campus.document_delivery")
	if status != Fail {
		t.Fatal("missing named ILLiad credential must remain visible")
	}
	if cfg.Browser.DocumentDelivery.APIKey != "" || cfg.Browser.Resolvers["campus"].DocumentDelivery.APIKey != "" {
		t.Fatal("doctor hydrated serializable configuration")
	}
	assertCredentialDiagnosticsPrivate(t, report, "PRIVATE_ILLIAD_KEY", "patron-ref-123")
}

type doctorCredentialOverride struct {
	CredentialView
	source func(string) config.Source
	states []credential.Status
}

func (v doctorCredentialOverride) SourcePolicy(name string) config.Source {
	if v.source != nil {
		return v.source(name)
	}
	return v.CredentialView.SourcePolicy(name)
}

func (v doctorCredentialOverride) Statuses() []credential.Status { return v.states }

func TestCredentialDiagnosticsCreditSpendUsesResolvedPolicy(t *testing.T) {
	ctx, cfg, db := creditFixture(t, 0.5, 999)
	view := doctorCredentialOverride{CredentialView: offlineCredentials{cfg: cfg}, source: func(name string) config.Source {
		p := cfg.SourcePolicy(name)
		if name == config.SourceOpenAlex {
			p.DailyCreditLimit = 23
		}
		return p
	}}
	var report Report
	checkCreditSpend(ctx, cfg, db, func(name, status, detail, remediation string) {
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	}, view)
	_, detail := checkStatus(t, report, "credits_openalex")
	if !strings.Contains(detail, "0 of 23 credits") || strings.Contains(detail, "999") {
		t.Fatalf("credit report ignored runtime policy: %s", detail)
	}
}

func TestCredentialDiagnosticsUnknownStatusCannotLeakProviderText(t *testing.T) {
	cfg := config.Default()
	cfg.Email = "reader@example.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true}
	view := doctorCredentialOverride{CredentialView: offlineCredentials{cfg: cfg}, states: []credential.Status{
		{Target: "sources.openalex", State: "PRIVATE_PROVIDER_ERROR", Source: "PRIVATE_BACKEND_ERROR", Reference: "https://PRIVATE_URL_TOKEN"},
		{Target: "PRIVATE_UNKNOWN_TARGET", State: "ready", Source: "keyring"},
	}}
	report := credentialChecks(cfg, view)
	status, detail := checkStatus(t, report, "credential:sources.openalex")
	if status != Warn || !strings.Contains(detail, "invalid (source: none)") {
		t.Fatalf("unrecognized status = %s %s", status, detail)
	}
	assertCredentialDiagnosticsPrivate(t, report, "PRIVATE_PROVIDER_ERROR", "PRIVATE_BACKEND_ERROR", "PRIVATE_URL_TOKEN", "PRIVATE_UNKNOWN_TARGET")
}

func TestCredentialDiagnosticsLegacyEmptyAgentOverrideIsDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Agent = &config.Agent{Backend: "typesafe"}
	view := runtimecredential.Resolve(context.Background(), cfg, nil, nil, func(name string) (string, bool) {
		return "", name == "PAPIO_TYPESAFE_API_KEY"
	})
	status, detail := checkStatus(t, credentialChecks(cfg, view), "credential:agent.typesafe")
	if status != Skip || !strings.Contains(detail, "explicitly disables") {
		t.Fatalf("intentional legacy override was mistaken for credential failure: %s %s", status, detail)
	}
}

func TestCredentialDiagnosticsLegacyInvalidAgentOverrideIsFailure(t *testing.T) {
	cfg := config.Default()
	view := runtimecredential.Resolve(context.Background(), cfg, nil, nil, func(name string) (string, bool) {
		return "invalid embedded space", name == "PAPIO_TYPESAFE_API_KEY"
	})
	status, detail := checkStatus(t, credentialChecks(cfg, view), "credential:agent.typesafe")
	if status != Fail || !strings.Contains(detail, "invalid (source: environment)") {
		t.Fatalf("legacy environment enrollment was ignored: %s %s", status, detail)
	}
}

func TestCredentialDiagnosticsDiscoverySelectionCountsAsEnabled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		source    string
		discovery []string
		want      string
	}{
		{"default OpenAlex", config.SourceOpenAlex, nil, Warn},
		{"explicit OpenAlex", config.SourceOpenAlex, []string{config.SourceOpenAlex}, Warn},
		{"explicit Semantic Scholar", config.SourceSemanticScholar, []string{config.SourceSemanticScholar}, Warn},
		{"OpenAlex not selected", config.SourceOpenAlex, []string{config.SourceArXiv}, Skip},
		{"Semantic Scholar not selected", config.SourceSemanticScholar, nil, Skip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Email = "reader@example.test"
			cfg.Discovery.Sources = tc.discovery
			cfg.Sources[tc.source] = config.Source{Enabled: false, CredentialRef: doctorCredentialRef}
			view := runtimecredential.Resolve(context.Background(), cfg, doctorCredentialReader(func(context.Context, string) (credential.Record, error) {
				return credential.Record{}, credential.ErrNotFound
			}), nil, nil)
			status, detail := checkStatus(t, credentialChecks(cfg, view), "credential:sources."+tc.source)
			if status != tc.want {
				t.Fatalf("discovery credential = %s %s, want %s", status, detail, tc.want)
			}
			if tc.want == Warn && !strings.Contains(detail, "keyless") {
				t.Fatalf("active discovery fallback was not explained: %s", detail)
			}
		})
	}
}

func TestCredentialDiagnosticsManualILLIadDoesNotRequireUnboundKey(t *testing.T) {
	for _, profile := range []string{"default", "campus"} {
		for _, policy := range []string{"", "never", "prefill_only", "auto_if_unconditional"} {
			for _, bound := range []bool{false, true} {
				name := profile + "/" + policy + "/unbound"
				if bound {
					name = profile + "/" + policy + "/bound"
				}
				t.Run(name, func(t *testing.T) {
					cfg := documentDeliveryTestConfig(t.TempDir())
					dd := cfg.Browser.DocumentDelivery
					dd.APIKey, dd.SubmitPolicy = "", policy
					if bound {
						dd.CredentialRef = doctorCredentialRef
					}
					if profile != "default" {
						cfg.Browser.DocumentDelivery = nil
						cfg.Browser.Resolvers = map[string]config.Institution{profile: {DocumentDelivery: dd}}
					}
					view := runtimecredential.Resolve(context.Background(), cfg, doctorCredentialReader(func(context.Context, string) (credential.Record, error) {
						return credential.Record{}, credential.ErrNotFound
					}), nil, nil)
					report := credentialChecks(cfg, view)
					checkDocumentDelivery(context.Background(), cfg, nil, func(name, status, detail, remediation string) {
						report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
					}, view)
					want := Skip
					if bound || policy == "auto_if_unconditional" {
						want = Fail
					}
					status, detail := checkStatus(t, report, "credential:"+deliveryCredentialTarget(profile))
					if status != want {
						t.Fatalf("ILLIad credential = %s %s, want %s", status, detail, want)
					}
					if report.OK != (want == Skip) {
						t.Fatalf("manual prefill readiness = %v, want %v", report.OK, want == Skip)
					}
					status, detail = checkStatus(t, report, "document_delivery:"+profile+":result")
					if status != Warn || detail != "PREFILL ONLY" {
						t.Fatalf("missing key changed the delivery gate: %s %s", status, detail)
					}
				})
			}
		}
	}
}

func assertCredentialDiagnosticsPrivate(t *testing.T, report Report, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("credential or raw backend text appeared in diagnostic output")
		}
	}
}
