// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/credential"
)

func TestAgentConfigSetAndRemoveUseUnifiedStore(t *testing.T) {
	for _, stdin := range []bool{true, false} {
		t.Run(map[bool]string{true: "stdin", false: "hidden"}[stdin], func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			f.env["PAPIO_TYPESAFE_API_KEY"] = "legacy-private"
			prompted := false
			f.deps.readSecret = func(io.Reader) ([]byte, error) { prompted = true; return []byte("new-private"), nil }
			args := []string{"set"}
			if stdin {
				args = append(args, "--key-stdin")
			}
			if err := f.runAgent(args, "new-private\r\n"); err != nil {
				t.Fatal(err)
			}
			ref := f.reference("agent.typesafe")
			if prompted == stdin || f.config().Agent.Backend != "typesafe" || f.store.records[ref] != (credential.Record{Kind: credential.KindTypeSafe, APIKey: "new-private"}) || f.legacy.saved+f.legacy.loaded+f.legacy.deleted != 0 {
				t.Fatal("agent facade did not use unified credential path")
			}
			var result agentConfigResult
			if err := json.Unmarshal(f.out.Bytes(), &result); err != nil || result.RestartRequired == nil || !*result.RestartRequired || result.Credential != "stored" || result.EnvironmentOverride != "ignored" || result.Reference != ref {
				t.Fatal("agent setup receipt is incomplete")
			}
			f.assertNoSecret(nil, "new-private", "legacy-private")
			if err := f.runAgent([]string{"remove"}, ""); err != nil {
				t.Fatal(err)
			}
			if f.config().Agent != nil || f.store.deleted+f.legacy.deleted != 0 || f.store.records[ref].APIKey != "new-private" {
				t.Fatal("agent removal deleted shared credential or left enrollment")
			}
		})
	}
}

func TestAgentConfigStatusLegacyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enrolled       bool
		env            *string
		key            string
		err            error
		want, override string
		loads          int
	}{
		{name: "disabled", want: "not_configured", override: "unset"},
		{name: "stored", enrolled: true, key: "private-key", want: "stored", override: "unset", loads: 1},
		{name: "missing", enrolled: true, err: agentcredential.ErrNotFound, want: "missing", override: "unset", loads: 1},
		{name: "unavailable", enrolled: true, err: errors.New("private-store"), want: "unavailable", override: "unset", loads: 1},
		{name: "invalid saved", enrolled: true, key: "private\nkey", want: "invalid", override: "unset", loads: 1},
		{name: "environment wins", enrolled: true, env: stringPointer("private-env"), key: "private-key", want: "environment", override: "enabled"},
		{name: "empty environment disables", enrolled: true, env: stringPointer(""), key: "private-key", want: "not_configured", override: "disabled"},
		{name: "invalid environment", enrolled: true, env: stringPointer("private\nkey"), key: "private-key", want: "invalid", override: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCredentialCLIFixture(t)
			if tc.enrolled {
				cfg := f.config()
				cfg.Agent = &config.Agent{Backend: "typesafe"}
				f.write(cfg)
			}
			if tc.env != nil {
				f.env["PAPIO_TYPESAFE_API_KEY"] = *tc.env
			}
			f.legacy.key = tc.key
			f.legacy.err = tc.err
			if err := f.runAgent([]string{"status"}, ""); err != nil {
				t.Fatal(err)
			}
			var result agentConfigResult
			if err := json.Unmarshal(f.out.Bytes(), &result); err != nil || result.Credential != tc.want || result.EnvironmentOverride != tc.override || result.RestartRequired != nil || f.legacy.loaded != tc.loads {
				t.Fatalf("unexpected status: %s", f.out.String())
			}
			f.assertNoSecret(nil, "private-key", "private-store", "private-env", "private\nkey")
		})
	}
}
func stringPointer(s string) *string { return &s }

func TestAgentConfigExplicitReferenceNeverFallsBack(t *testing.T) {
	f := newCredentialCLIFixture(t)
	ref, _ := credential.NewReference()
	cfg := f.config()
	cfg.Agent = &config.Agent{Backend: "typesafe", CredentialRef: ref}
	f.write(cfg)
	f.legacy.key = "legacy-private"
	f.legacy.err = nil
	f.env["PAPIO_TYPESAFE_API_KEY"] = "env-private"
	if err := f.runAgent([]string{"status"}, ""); err != nil {
		t.Fatal(err)
	}
	var result agentConfigResult
	if err := json.Unmarshal(f.out.Bytes(), &result); err != nil || result.Credential != "missing" || result.EnvironmentOverride != "ignored" || f.legacy.loaded != 0 {
		t.Fatal("explicit ref fell back or misreported precedence")
	}
	f.assertNoSecret(nil, "legacy-private", "env-private")
}

func TestAgentConfigSecretArgumentsAndFailedRemove(t *testing.T) {
	f := newCredentialCLIFixture(t)
	err := f.runAgent([]string{"set", "accidental-private-key"}, "")
	if err == nil || strings.Contains(err.Error()+f.out.String(), "accidental-private-key") {
		t.Fatal("positional key echoed")
	}
	cfg := f.config()
	cfg.Agent = &config.Agent{Backend: "typesafe"}
	f.write(cfg)
	f.deps.saveConfig = func(config.Config, string, config.Snapshot) error { return errors.New("private-file-error") }
	err = f.runAgent([]string{"remove"}, "")
	if err == nil || f.config().Agent == nil || f.legacy.deleted+f.store.deleted != 0 {
		t.Fatal("failed removal changed config or credential")
	}
	f.assertNoSecret(err, "private-file-error")
}
