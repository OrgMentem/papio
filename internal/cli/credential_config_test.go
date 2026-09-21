// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/credential"
)

type fakeCredentialConfigStore struct {
	records                map[string]credential.Record
	saved, loaded, deleted int
	onSave                 func(string, credential.Record) error
	onLoad                 func(string) (credential.Record, error)
	err                    error
}

func (s *fakeCredentialConfigStore) Load(_ context.Context, ref string) (credential.Record, error) {
	s.loaded++
	if s.onLoad != nil {
		return s.onLoad(ref)
	}
	if s.err != nil {
		return credential.Record{}, s.err
	}
	r, ok := s.records[ref]
	if !ok {
		return credential.Record{}, credential.ErrNotFound
	}
	return r, nil
}
func (s *fakeCredentialConfigStore) Save(_ context.Context, ref string, r credential.Record) error {
	s.saved++
	if s.err != nil {
		return s.err
	}
	if s.onSave != nil {
		if err := s.onSave(ref, r); err != nil {
			return err
		}
	}
	s.records[ref] = r
	return nil
}
func (s *fakeCredentialConfigStore) Delete(_ context.Context, ref string) error {
	s.deleted++
	if s.err != nil {
		return s.err
	}
	delete(s.records, ref)
	return nil
}

type fakeAgentCredentialStore struct {
	key, profile           string
	err                    error
	loaded, saved, deleted int
}

func (s *fakeAgentCredentialStore) Load(_ context.Context, p string) (string, error) {
	s.loaded++
	s.profile = p
	return s.key, s.err
}
func (s *fakeAgentCredentialStore) Save(_ context.Context, p, k string) error {
	s.saved++
	s.profile = p
	s.key = k
	return s.err
}
func (s *fakeAgentCredentialStore) Delete(_ context.Context, p string) error {
	s.deleted++
	s.profile = p
	return s.err
}

type credentialCLIFixture struct {
	t               *testing.T
	path            string
	opt             *options
	deps            credentialConfigDependencies
	store           *fakeCredentialConfigStore
	legacy          *fakeAgentCredentialStore
	env             map[string]string
	out, diagnostic bytes.Buffer
}

func newCredentialCLIFixture(t *testing.T) *credentialCLIFixture {
	t.Helper()
	f := &credentialCLIFixture{t: t, path: filepath.Join(t.TempDir(), "config.toml"), store: &fakeCredentialConfigStore{records: map[string]credential.Record{}}, legacy: &fakeAgentCredentialStore{err: agentcredential.ErrNotFound}, env: map[string]string{}}
	cfg := config.Default()
	cfg.Path = f.path
	cfg.DataDir = t.TempDir()
	cfg.Browser.AdoptionRoot = filepath.Join(t.TempDir(), "papio")
	cfg.AccessMode = config.ModeDelegated
	f.write(cfg)
	f.opt = &options{configPath: f.path, out: &f.out, errOut: &f.diagnostic, jsonOutput: true}
	f.deps = credentialConfigDependencies{store: f.store, legacyStore: f.legacy, lookupEnv: func(n string) (string, bool) { v, ok := f.env[n]; return v, ok }, readSecret: func(io.Reader) ([]byte, error) { return []byte("synthetic-private-key"), nil }, readSnapshot: config.ReadSnapshot, saveConfig: config.SaveIfUnchanged, loadDiskConfig: config.Load, newReference: credential.NewReference}
	return f
}
func (f *credentialCLIFixture) write(cfg config.Config) {
	f.t.Helper()
	if err := config.Save(cfg, f.path); err != nil {
		f.t.Fatal(err)
	}
}
func (f *credentialCLIFixture) config() config.Config {
	f.t.Helper()
	cfg, err := config.Load(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}
func (f *credentialCLIFixture) bytes() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}
func (f *credentialCLIFixture) run(args []string, input string) error {
	f.out.Reset()
	f.diagnostic.Reset()
	cmd := newCredentialConfigCommandWithDependencies(f.opt, f.deps)
	cmd.SetOut(&f.out)
	cmd.SetErr(&f.diagnostic)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetIn(strings.NewReader(input))
	cmd.SetArgs(args)
	return cmd.ExecuteContext(context.Background())
}
func (f *credentialCLIFixture) runAgent(args []string, input string) error {
	f.out.Reset()
	f.diagnostic.Reset()
	cmd := newAgentConfigCommandWithDependencies(f.opt, f.deps)
	cmd.SetOut(&f.out)
	cmd.SetErr(&f.diagnostic)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetIn(strings.NewReader(input))
	cmd.SetArgs(args)
	return cmd.ExecuteContext(context.Background())
}
func (f *credentialCLIFixture) assertNoSecret(err error, secrets ...string) {
	f.t.Helper()
	output := f.out.String() + f.diagnostic.String()
	if err != nil {
		output += err.Error()
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(output, secret) {
			f.t.Fatal("credential appeared in command output")
		}
	}
}
func (f *credentialCLIFixture) reference(target string) string {
	f.t.Helper()
	b, err := credentialBinding(f.config(), target)
	if err != nil {
		f.t.Fatal(err)
	}
	return b.Reference
}

func TestCredentialConfigSetTypedRecords(t *testing.T) {
	for _, tc := range []struct {
		target string
		record credential.Record
	}{
		{"agent.typesafe", credential.Record{Kind: credential.KindTypeSafe, APIKey: "typesafe-private"}},
		{"sources.openalex", credential.Record{Kind: credential.KindOpenAlex, APIKey: "openalex-private"}},
		{"sources.semanticscholar", credential.Record{Kind: credential.KindSemanticScholar, APIKey: "semantic-private"}},
		{"sources.core", credential.Record{Kind: credential.KindCORE, APIKey: "core-private"}},
		{"sources.crossref_tdm", credential.Record{Kind: credential.KindCrossrefTDM, APIKey: "tdm-private"}},
		{"sources.openaire", credential.Record{Kind: credential.KindOpenAIREClient, ClientID: "client-private", ClientSecret: "secret-private"}},
		{"sources.openaire", credential.Record{Kind: credential.KindOpenAIREToken, APIKey: "token-private"}},
		{"notify.webhook", credential.Record{Kind: credential.KindWebhook, URL: "https://example.org/hooks/path-private?token=query-private", Bearer: "bearer-private"}},
	} {
		t.Run(tc.target+string(tc.record.Kind), func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			input := tc.record.APIKey
			if tc.target == "sources.openaire" || tc.target == "notify.webhook" {
				var err error
				input, err = credential.Encode(tc.record)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.run([]string{"set", tc.target, "--key-stdin"}, input+"\r\n"); err != nil {
				t.Fatal(err)
			}
			ref := f.reference(tc.target)
			if credential.ValidateReference(ref) != nil || f.store.records[ref] != tc.record || f.store.saved != 1 || f.store.loaded != 2 {
				t.Fatal("typed credential was not published and verified")
			}
			f.assertNoSecret(nil, tc.record.APIKey, tc.record.ClientID, tc.record.ClientSecret, tc.record.URL, tc.record.Bearer)
			for _, secret := range []string{tc.record.APIKey, tc.record.ClientID, tc.record.ClientSecret, tc.record.URL, tc.record.Bearer} {
				if secret != "" && bytes.Contains(f.bytes(), []byte(secret)) {
					t.Fatal("credential copied into config")
				}
			}
		})
	}
}

func TestCredentialConfigFreshReferencesSharedBindAndDetach(t *testing.T) {
	first := newCredentialCLIFixture(t)
	if err := first.run([]string{"set", "sources.openalex", "--key-stdin"}, "private-key-one"); err != nil {
		t.Fatal(err)
	}
	original := first.reference("sources.openalex")
	second := newCredentialCLIFixture(t)
	second.store = first.store
	second.deps.store = first.store
	if err := second.run([]string{"bind", "sources.openalex", original}, ""); err != nil {
		t.Fatal(err)
	}
	if err := first.run([]string{"set", "sources.openalex", "--key-stdin"}, "private-key-two"); err != nil {
		t.Fatal(err)
	}
	if first.reference("sources.openalex") == original || second.reference("sources.openalex") != original || first.store.records[original].APIKey != "private-key-one" {
		t.Fatal("rotation changed another profile's shared credential")
	}
	if err := second.run([]string{"detach", "sources.openalex"}, ""); err != nil {
		t.Fatal(err)
	}
	if second.reference("sources.openalex") != "" || first.store.deleted != 0 || first.store.records[original].APIKey != "private-key-one" {
		t.Fatal("detach deleted a shared record")
	}
	if err := second.run([]string{"delete", original}, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := first.store.records[original]; ok || first.store.deleted != 1 {
		t.Fatal("explicit deletion did not delete exactly the requested record")
	}
}

func TestCredentialConfigRejectsInvalidKindsAndUnavailableBind(t *testing.T) {
	for _, scenario := range []string{"wrong kind", "missing", "unavailable", "invalid reference", "environment missing", "environment invalid"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			before := f.bytes()
			ref, _ := credential.NewReference()
			switch scenario {
			case "wrong kind":
				f.store.records[ref] = credential.Record{Kind: credential.KindTypeSafe, APIKey: "private-typesafe"}
			case "unavailable":
				f.store.err = errors.New("backend-private-value")
			case "invalid reference":
				ref = "private-invalid-reference"
			case "environment missing":
				ref = "env:TEST_CREDENTIAL"
			case "environment invalid":
				ref = "env:TEST_CREDENTIAL"
				f.env["TEST_CREDENTIAL"] = "private\nvalue"
			}
			err := f.run([]string{"bind", "sources.openalex", ref}, "")
			if err == nil || !bytes.Equal(before, f.bytes()) || f.store.saved+f.store.deleted != 0 {
				t.Fatal("bad binding changed config or store")
			}
			f.assertNoSecret(err, "private-typesafe", "backend-private-value", "private-invalid-reference", "private\nvalue")
		})
	}
}

func TestCredentialConfigEnvironmentBindingAndReferenceAuthority(t *testing.T) {
	f := newCredentialCLIFixture(t)
	f.env["EXPLICIT_KEY"] = "explicit-private"
	f.env["PAPIO_TYPESAFE_API_KEY"] = "legacy-private"
	f.legacy.key = "old-profile-private"
	f.legacy.err = nil
	if err := f.run([]string{"bind", "agent.typesafe", "env:EXPLICIT_KEY"}, ""); err != nil {
		t.Fatal(err)
	}
	if f.config().Agent.Backend != "typesafe" || f.legacy.loaded != 0 || f.store.loaded != 0 {
		t.Fatal("explicit environment binding used an implicit store")
	}
	delete(f.env, "EXPLICIT_KEY")
	if err := f.run([]string{"status", "agent.typesafe"}, ""); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Credentials []credentialStatusRow `json:"credentials"`
		Truncated   bool                  `json:"truncated"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Credentials) != 1 || envelope.Credentials[0].State != "missing" || envelope.Credentials[0].Source != "environment" || f.legacy.loaded != 0 {
		t.Fatal("missing explicit ref fell back to legacy credential")
	}
	f.assertNoSecret(nil, "explicit-private", "legacy-private", "old-profile-private")
}

func TestCredentialConfigSetFailurePreservesConfigAndReportsStagedReference(t *testing.T) {
	for _, scenario := range []string{"store unavailable", "uncertain", "readback mismatch", "publish failure", "concurrent edit", "post-publish missing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			before := f.bytes()
			switch scenario {
			case "store unavailable":
				f.store.err = errors.New("backend-private")
			case "uncertain":
				f.store.err = credential.ErrUncertain
			case "readback mismatch":
				f.store.onLoad = func(string) (credential.Record, error) {
					return credential.Record{Kind: credential.KindOpenAlex, APIKey: "different-private"}, nil
				}
			case "publish failure":
				f.deps.saveConfig = func(config.Config, string, config.Snapshot) error { return errors.New("private-file-path") }
			case "concurrent edit":
				f.store.onSave = func(string, credential.Record) error {
					cfg := f.config()
					cfg.Email = "edited@example.org"
					f.write(cfg)
					before = f.bytes()
					return nil
				}
			case "post-publish missing":
				f.store.onLoad = func(ref string) (credential.Record, error) {
					if f.store.loaded > 1 {
						return credential.Record{}, credential.ErrNotFound
					}
					return f.store.records[ref], nil
				}
			}
			err := f.run([]string{"set", "sources.openalex", "--key-stdin"}, "original-private")
			if err == nil {
				t.Fatal("expected partial failure")
			}
			if scenario != "post-publish missing" {
				if !bytes.Equal(before, f.bytes()) || !strings.Contains(err.Error(), "keyring:") {
					t.Fatal("original config changed or staged reference was lost")
				}
			} else if f.reference("sources.openalex") == "" || !strings.Contains(err.Error(), "configuration saved") {
				t.Fatal("published-but-unverified state not disclosed")
			}
			f.assertNoSecret(err, "backend-private", "different-private", "private-file-path", "original-private")
		})
	}
}

func TestCredentialConfigMutationRejectsStaleSuppliedConfigBeforeStore(t *testing.T) {
	f := newCredentialCLIFixture(t)
	stale := f.config()
	f.opt.configLoader = func(string) (config.Config, error) { return stale, nil }
	changed := f.config()
	changed.Email = "changed@example.org"
	f.write(changed)
	before := f.bytes()
	err := f.run([]string{"set", "sources.openalex", "--key-stdin"}, "private-key")
	if err == nil || f.store.saved+f.store.loaded != 0 || !bytes.Equal(before, f.bytes()) {
		t.Fatal("stale supplied config was published or touched store")
	}
}

func TestCredentialConfigMigrationAtomicAndLegacyPreserved(t *testing.T) {
	f := newCredentialCLIFixture(t)
	cfg := f.config()
	s := cfg.Sources[config.SourceOpenAlex]
	s.APIKey = "alex-private"
	cfg.Sources[config.SourceOpenAlex] = s
	s = cfg.Sources[config.SourceOpenAIRE]
	s.ClientID = "client-private"
	s.ClientSecret = "pair-private"
	cfg.Sources[config.SourceOpenAIRE] = s
	cfg.Notify.WebhookURL = "https://example.org/hook/url-private"
	cfg.Notify.WebhookSecret = "bearer-private"
	cfg.Agent = &config.Agent{Backend: "typesafe"}
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{Kind: "illiad", APIKey: "default-illiad-private"}
	cfg.Browser.Resolvers = map[string]config.Institution{"campus": {OpenURLBase: "https://resolver.example.org/", DocumentDelivery: &config.DocumentDelivery{Kind: "illiad", APIKey: "named-illiad-private"}}}
	f.write(cfg)
	f.legacy.key = "legacy-typesafe-private"
	f.legacy.err = nil
	if err := f.run([]string{"migrate"}, ""); err != nil {
		t.Fatal(err)
	}
	if f.store.saved != 6 || f.legacy.deleted+f.legacy.saved != 0 || f.legacy.loaded != 1 {
		t.Fatal("migration lost a consumer or modified legacy store")
	}
	var result credentialMigrationResult
	if err := json.Unmarshal(f.out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "migrated" || !result.RestartRequired || len(result.Credentials) != 6 {
		t.Fatal("incomplete migration receipt")
	}
	next := f.config()
	if next.Sources[config.SourceOpenAlex].APIKey != "" || next.Sources[config.SourceOpenAIRE].ClientID != "" || next.Sources[config.SourceOpenAIRE].ClientSecret != "" || next.Notify.WebhookURL != "" || next.Notify.WebhookSecret != "" || next.Browser.DocumentDelivery.APIKey != "" || next.Browser.Resolvers["campus"].DocumentDelivery.APIKey != "" {
		t.Fatal("legacy literals survived migration")
	}
	defaultRef := f.reference("browser.document_delivery")
	namedRef := f.reference("browser.resolvers.campus.document_delivery")
	if defaultRef == namedRef || f.store.records[defaultRef].APIKey != "default-illiad-private" || f.store.records[namedRef].APIKey != "named-illiad-private" {
		t.Fatal("institution credentials were merged")
	}
	for _, secret := range []string{"alex-private", "client-private", "pair-private", "url-private", "bearer-private", "default-illiad-private", "named-illiad-private", "legacy-typesafe-private"} {
		f.assertNoSecret(nil, secret)
		if bytes.Contains(f.bytes(), []byte(secret)) {
			t.Fatal("secret serialized after migration")
		}
	}
	before := f.bytes()
	if err := f.run([]string{"migrate"}, ""); err != nil {
		t.Fatal(err)
	}
	if f.store.saved != 6 || !bytes.Equal(before, f.bytes()) {
		t.Fatal("repeat migration rewrote credentials")
	}
}

func TestCredentialConfigMigrationInterruptedAndConcurrentEdit(t *testing.T) {
	for _, scenario := range []string{"second store write", "config write", "concurrent edit", "readback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			cfg := f.config()
			for _, name := range []string{config.SourceOpenAlex, config.SourceCORE} {
				s := cfg.Sources[name]
				s.APIKey = name + "-private"
				cfg.Sources[name] = s
			}
			f.write(cfg)
			before := f.bytes()
			switch scenario {
			case "second store write":
				f.store.onSave = func(string, credential.Record) error {
					if f.store.saved == 2 {
						return errors.New("secret-backend-error")
					}
					return nil
				}
			case "config write":
				f.deps.saveConfig = func(config.Config, string, config.Snapshot) error { return errors.New("secret-file-error") }
			case "concurrent edit":
				f.store.onSave = func(string, credential.Record) error {
					if f.store.saved == 1 {
						cfg := f.config()
						cfg.Email = "external@example.org"
						f.write(cfg)
						before = f.bytes()
					}
					return nil
				}
			case "readback":
				f.store.onLoad = func(string) (credential.Record, error) { return credential.Record{}, credential.ErrNotFound }
			}
			err := f.run([]string{"migrate"}, "")
			if err == nil || !bytes.Equal(before, f.bytes()) || f.store.deleted != 0 || f.legacy.deleted != 0 {
				t.Fatal("interrupted migration changed original config or deleted credentials")
			}
			for ref := range f.store.records {
				if !strings.Contains(err.Error(), ref) {
					t.Fatal("staged reference omitted from interruption receipt")
				}
			}
			if !strings.Contains(err.Error(), "keyring:") {
				t.Fatal("dispatched reference omitted")
			}
			f.assertNoSecret(err, "openalex-private", "core-private", "secret-backend-error", "secret-file-error")
		})
	}
}

func TestCredentialConfigMigrationDeclinesAmbiguityAndEnvironment(t *testing.T) {
	for _, scenario := range []string{"unsupported", "partial", "competing", "legacy environment", "empty environment", "legacy missing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			cfg := f.config()
			switch scenario {
			case "unsupported":
				s := cfg.Sources[config.SourceCrossrefMetadata]
				s.APIKey = "private-key"
				cfg.Sources[config.SourceCrossrefMetadata] = s
			case "partial":
				s := cfg.Sources[config.SourceOpenAIRE]
				s.ClientID = "private-id"
				cfg.Sources[config.SourceOpenAIRE] = s
			case "competing":
				s := cfg.Sources[config.SourceOpenAIRE]
				s.ClientID = "private-id"
				s.ClientSecret = "private-secret"
				s.APIKey = "private-token"
				cfg.Sources[config.SourceOpenAIRE] = s
			case "legacy environment":
				f.env["PAPIO_TYPESAFE_API_KEY"] = "private-env"
			case "empty environment":
				f.env["PAPIO_TYPESAFE_API_KEY"] = ""
			case "legacy missing":
				cfg.Agent = &config.Agent{Backend: "typesafe"}
			}
			f.write(cfg)
			before := f.bytes()
			err := f.run([]string{"migrate"}, "")
			if err == nil || f.store.saved+f.store.deleted != 0 || !bytes.Equal(before, f.bytes()) {
				t.Fatal("ambiguous migration changed state")
			}
			f.assertNoSecret(err, "private-key", "private-id", "private-secret", "private-token", "private-env")
		})
	}
}

func TestCredentialConfigStatusEnvelopeAndNoSecretOutput(t *testing.T) {
	f := newCredentialCLIFixture(t)
	cfg := f.config()
	s := cfg.Sources[config.SourceOpenAlex]
	s.APIKey = "legacy-private"
	cfg.Sources[config.SourceOpenAlex] = s
	f.write(cfg)
	if err := f.run([]string{"status"}, ""); err != nil {
		t.Fatal(err)
	}
	assertEnvelope(t, "credentials status", "credentials", f.out.Bytes())
	f.assertNoSecret(nil, "legacy-private")
	if f.store.loaded+f.store.saved+f.store.deleted+f.legacy.loaded != 0 {
		t.Fatal("literal status unexpectedly touched vault")
	}
}

func TestCredentialConfigInputErrorsNeverEchoValues(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		input string
	}{
		{[]string{"set", "agent.typesafe", "--key-stdin"}, "private key"},
		{[]string{"set", "agent.typesafe", "--key-stdin"}, strings.Repeat("x", 1025)},
		{[]string{"set", "sources.openaire", "--key-stdin"}, `{"version":1,"kind":"openaire_client","client_id":"private","client_secret":"private","extra":"private"}`},
		{[]string{"set", "notify.webhook"}, ""},
		{[]string{"set", "sources.openalex", "private-key"}, ""},
		{[]string{"bind", "sources.openalex", "private-key"}, ""},
		{[]string{"delete", "private-key"}, ""},
		{[]string{"set", "private-target", "--key-stdin"}, "private-value"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			before := f.bytes()
			err := f.run(tc.args, tc.input)
			if err == nil || f.store.saved+f.store.deleted != 0 || !bytes.Equal(before, f.bytes()) {
				t.Fatal("invalid input accepted")
			}
			f.assertNoSecret(err, "private")
		})
	}
}

func TestCredentialConfigRetainsRecordHandles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agent    bool
		args     []string
		previous bool
	}{
		{"rotate", false, []string{"set", "agent.typesafe", "--key-stdin"}, true},
		{"detach", false, []string{"detach", "agent.typesafe"}, false},
		{"agent rotate", true, []string{"set", "--key-stdin"}, true},
		{"agent remove", true, []string{"remove"}, false},
		{"rebind", false, []string{"bind", "agent.typesafe", "env:REPLACEMENT"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, jsonOutput := range []bool{true, false} {
				f := newCredentialCLIFixture(t)
				if err := f.run([]string{"set", "agent.typesafe", "--key-stdin"}, "old-private"); err != nil {
					t.Fatal(err)
				}
				old := f.reference("agent.typesafe")
				f.opt.jsonOutput = jsonOutput
				f.env["REPLACEMENT"] = "new-private"
				var err error
				if tc.agent {
					err = f.runAgent(tc.args, "new-private")
				} else {
					err = f.run(tc.args, "new-private")
				}
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(f.out.String(), old) || f.store.deleted != 0 || f.store.records[old].APIKey != "old-private" {
					t.Fatal("old credential handle was lost or record deleted")
				}
				if jsonOutput {
					var receipt map[string]any
					if err := json.Unmarshal(f.out.Bytes(), &receipt); err != nil {
						t.Fatal(err)
					}
					field := "reference"
					if tc.previous {
						field = "previous_reference"
					}
					if receipt[field] != old {
						t.Fatal("retained reference missing from structured receipt")
					}
				}
				f.assertNoSecret(nil, "old-private", "new-private")
			}
		})
	}
}

func TestCredentialConfigScrubsEnvironmentBeforeStoreHelpers(t *testing.T) {
	for _, scenario := range []string{"status", "set", "agent set", "agent status", "migrate", "delete"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			const selected = "PAPIO_CLI_TEST_EXPLICIT_SECRET"
			t.Setenv(selected, "selected-private")
			t.Setenv("PAPIO_TYPESAFE_API_KEY", "legacy-private")
			f.deps.lookupEnv = os.LookupEnv
			f.deps.prepareEnv = takeCredentialCommandEnvironment
			cfg := f.config()
			source := cfg.Sources[config.SourceOpenAlex]
			source.CredentialRef = "env:" + selected
			cfg.Sources[config.SourceOpenAlex] = source
			ref, _ := credential.NewReference()
			f.store.records[ref] = credential.Record{Kind: credential.KindTypeSafe, APIKey: "saved-private"}
			cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: ref}
			if scenario == "migrate" {
				s := cfg.Sources[config.SourceCORE]
				s.APIKey = "core-private"
				cfg.Sources[config.SourceCORE] = s
			}
			f.write(cfg)
			calls := 0
			check := func() {
				calls++
				for _, name := range []string{selected, "PAPIO_TYPESAFE_API_KEY"} {
					if _, ok := os.LookupEnv(name); ok {
						t.Fatal("selected credential inherited by a store helper")
					}
				}
			}
			f.store.onSave = func(string, credential.Record) error { check(); return nil }
			f.store.onLoad = func(ref string) (credential.Record, error) {
				check()
				r, ok := f.store.records[ref]
				if !ok {
					return credential.Record{}, credential.ErrNotFound
				}
				return r, nil
			}
			var err error
			switch scenario {
			case "status":
				err = f.run([]string{"status"}, "")
			case "set":
				err = f.run([]string{"set", "sources.core", "--key-stdin"}, "new-private")
			case "agent set":
				err = f.runAgent([]string{"set", "--key-stdin"}, "new-private")
			case "agent status":
				err = f.runAgent([]string{"status"}, "")
			case "migrate":
				err = f.run([]string{"migrate"}, "")
			case "delete":
				err = f.run([]string{"delete", ref}, "")
				check()
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls == 0 {
				t.Fatal("no store oracle was reached")
			}
			if scenario == "status" {
				var out struct {
					Credentials []credentialStatusRow `json:"credentials"`
				}
				if err := json.Unmarshal(f.out.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				ready := false
				for _, row := range out.Credentials {
					if row.Target == "sources.openalex" && row.State == "ready" {
						ready = true
					}
				}
				if !ready {
					t.Fatal("captured environment value was lost after scrubbing")
				}
			}
			if scenario == "agent set" || scenario == "agent status" {
				var out agentConfigResult
				if err := json.Unmarshal(f.out.Bytes(), &out); err != nil || out.EnvironmentOverride != "ignored" {
					t.Fatal("environment receipt lost captured override")
				}
			}
			f.assertNoSecret(nil, "selected-private", "legacy-private", "saved-private", "new-private", "core-private")
		})
	}
}

func TestCredentialConfigBindScrubsNewAndReplacedEnvironmentInputs(t *testing.T) {
	f := newCredentialCLIFixture(t)
	const oldName = "PAPIO_CLI_TEST_OLD_SECRET"
	const newName = "PAPIO_CLI_TEST_NEW_SECRET"
	t.Setenv(oldName, "old-private")
	t.Setenv(newName, "new-private")
	t.Setenv("PAPIO_TYPESAFE_API_KEY", "legacy-private")
	cfg := f.config()
	source := cfg.Sources[config.SourceOpenAlex]
	source.CredentialRef = "env:" + oldName
	cfg.Sources[config.SourceOpenAlex] = source
	f.write(cfg)
	f.deps.lookupEnv = os.LookupEnv
	f.deps.prepareEnv = takeCredentialCommandEnvironment
	if err := f.run([]string{"bind", "sources.openalex", "env:" + newName}, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{oldName, newName, "PAPIO_TYPESAFE_API_KEY"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatal("prospective or replaced credential remained in process environment")
		}
	}
	if f.reference("sources.openalex") != "env:"+newName {
		t.Fatal("captured new environment credential was not usable after scrub")
	}
	f.assertNoSecret(nil, "old-private", "new-private", "legacy-private")
}

func TestCredentialConfigMigrationPreservesRetiredSourceSecrets(t *testing.T) {
	f := newCredentialCLIFixture(t)
	cfg := f.config()
	source := cfg.Sources[config.SourceOpenAlex]
	source.APIKey = "supported-private"
	cfg.Sources[config.SourceOpenAlex] = source
	f.write(cfg)
	original := append(f.bytes(), []byte("\n[sources.openalex_content]\napi_key = 'retired-private'\n")...)
	if err := os.WriteFile(f.path, original, 0600); err != nil {
		t.Fatal(err)
	}
	normalized := f.config()
	if _, ok := normalized.Sources["openalex_content"]; ok {
		t.Fatal("fixture no longer exercises the normal retired-source normalization")
	}
	err := f.run([]string{"migrate"}, "")
	if err == nil || !bytes.Equal(original, f.bytes()) || f.store.saved+f.store.loaded+f.store.deleted+f.legacy.loaded != 0 {
		t.Fatal("retired source secret was lost or migration touched vault before completing inventory")
	}
	f.assertNoSecret(err, "supported-private", "retired-private")
}

func TestCredentialConfigHumanStatusShowsReferenceWithoutSecrets(t *testing.T) {
	for _, scenario := range []string{"keyring ready", "keyring missing", "environment", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			f.opt.jsonOutput = false
			cfg := f.config()
			source := cfg.Sources[config.SourceOpenAlex]
			state, origin := "ready", "keyring"
			switch scenario {
			case "keyring ready", "keyring missing":
				source.CredentialRef, _ = credential.NewReference()
				if scenario == "keyring ready" {
					f.store.records[source.CredentialRef] = credential.Record{Kind: credential.KindOpenAlex, APIKey: "private-store-key"}
				} else {
					state = "missing"
				}
			case "environment":
				source.CredentialRef = "env:STATUS_TEST_KEY"
				f.env["STATUS_TEST_KEY"] = "private-env-key"
				origin = "environment"
			case "legacy":
				source.APIKey = "private-literal-key"
				origin = "legacy"
			}
			cfg.Sources[config.SourceOpenAlex] = source
			f.write(cfg)
			if err := f.run([]string{"status", "sources.openalex"}, ""); err != nil {
				t.Fatal(err)
			}
			selection := origin
			if source.CredentialRef != "" {
				selection += "; reference: " + source.CredentialRef
			}
			want := "sources.openalex: " + state + " (" + selection + ")\n"
			if f.out.String() != want {
				t.Fatalf("status = %q, want %q", f.out.String(), want)
			}
			f.assertNoSecret(nil, "private-store-key", "private-env-key", "private-literal-key")
		})
	}
}
