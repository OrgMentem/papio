// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/agentcredential"
	"papio/internal/config"
)

type fakeAgentCredentialStore struct {
	key, profile           string
	err                    error
	saved, deleted, loaded int
	onDelete               func()
}

func (s *fakeAgentCredentialStore) Load(_ context.Context, profile string) (string, error) {
	s.loaded++
	s.profile = profile
	return s.key, s.err
}
func (s *fakeAgentCredentialStore) Save(_ context.Context, profile, key string) error {
	s.saved++
	s.profile = profile
	if s.err == nil {
		s.key = key
	}
	return s.err
}
func (s *fakeAgentCredentialStore) Delete(_ context.Context, profile string) error {
	s.deleted++
	s.profile = profile
	if s.onDelete != nil {
		s.onDelete()
	}
	return s.err
}

func TestAgentConfigSetAndRemove(t *testing.T) {
	for _, useStdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "hidden prompt", true: "stdin"}[useStdin], func(t *testing.T) {
			cfg := config.Default()
			cfg.AccessMode = config.ModeDelegated
			cfg.Path, cfg.DataDir = filepath.Join(t.TempDir(), "config.toml"), t.TempDir()
			var out, diagnostic bytes.Buffer
			store := &fakeAgentCredentialStore{}
			opt := &options{out: &out, errOut: &diagnostic, jsonOutput: true, configLoader: func(string) (config.Config, error) { return cfg, nil }}
			prompted := false
			deps := agentConfigDependencies{store: store, saveConfig: func(next config.Config, path string) error {
				if path != cfg.Path {
					t.Fatal("wrong config")
				}
				cfg = next
				return nil
			}, lookupEnv: func(string) (string, bool) { return "", false }, readSecret: func(io.Reader) ([]byte, error) { prompted = true; return []byte("synthetic-key"), nil }}
			cmd := newAgentConfigCommandWithDependencies(opt, deps)
			args := []string{"set"}
			if useStdin {
				args = append(args, "--key-stdin")
			}
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader("synthetic-key\r\n"))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if prompted == useStdin || store.saved != 1 || store.key != "synthetic-key" || cfg.Agent == nil || cfg.Agent.Backend != "typesafe" {
				t.Fatal("key setup did not complete")
			}
			want, _ := agentcredential.Profile(cfg.Path, cfg.DataDir)
			if store.profile != want {
				t.Fatal("wrong profile")
			}
			var result agentConfigResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.RestartRequired == nil || !*result.RestartRequired || result.Credential != "stored" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(out.String()+diagnostic.String(), store.key) {
				t.Fatal("key leaked")
			}
			store.onDelete = func() {
				if cfg.Agent != nil {
					t.Fatal("delete before disabling configuration")
				}
			}
			out.Reset()
			cmd = newAgentConfigCommandWithDependencies(opt, deps)
			cmd.SetArgs([]string{"remove"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if store.deleted != 1 || cfg.Agent != nil {
				t.Fatal("remove failed")
			}
		})
	}
}

func TestAgentConfigSetRejectsSecretsWithoutEcho(t *testing.T) {
	for _, input := range []string{"", "private key", "private\nkey", strings.Repeat("x", 1025), strings.Repeat("x", 1025) + "\r\n"} {
		var out bytes.Buffer
		cfg := config.Default()
		cfg.Path = filepath.Join(t.TempDir(), "config.toml")
		cfg.AccessMode = config.ModeDelegated
		store := &fakeAgentCredentialStore{}
		cmd := newAgentConfigCommandWithDependencies(&options{out: &out, errOut: &out, configLoader: func(string) (config.Config, error) { return cfg, nil }}, agentConfigDependencies{
			store: store, saveConfig: func(config.Config, string) error { t.Fatal("invalid key reached config"); return nil }, lookupEnv: func(string) (string, bool) { return "", false },
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"set", "--key-stdin"})
		cmd.SetIn(strings.NewReader(input))
		err := cmd.Execute()
		if err == nil || store.saved != 0 {
			t.Fatal("invalid key accepted")
		}
		if strings.Contains(out.String()+err.Error(), "private") {
			t.Fatal("key echoed")
		}
	}
	var out bytes.Buffer
	cmd := newAgentConfigCommandWithDependencies(&options{out: &out, errOut: &out}, agentConfigDependencies{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"set", "accidental-private-key"})
	err := cmd.Execute()
	if err == nil || strings.Contains(err.Error()+out.String(), "accidental-private-key") {
		t.Fatal("positional key echoed")
	}
}

func TestAgentConfigPartialFailures(t *testing.T) {
	for _, scenario := range []string{"store save", "config after save", "config before delete", "delete"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := config.Default()
			cfg.Path = filepath.Join(t.TempDir(), "config.toml")
			cfg.AccessMode = config.ModeDelegated
			cfg.Agent = &config.Agent{Backend: "typesafe"}
			store := &fakeAgentCredentialStore{}
			if scenario == "store save" || scenario == "delete" {
				store.err = errors.New("credential store unavailable")
			}
			writes := 0
			deps := agentConfigDependencies{store: store, lookupEnv: func(string) (string, bool) { return "", false }, saveConfig: func(next config.Config, _ string) error {
				writes++
				if strings.HasPrefix(scenario, "config") {
					return errors.New("private path")
				}
				cfg = next
				return nil
			}}
			var out bytes.Buffer
			cmd := newAgentConfigCommandWithDependencies(&options{out: &out, errOut: &out, configLoader: func(string) (config.Config, error) { return cfg, nil }}, deps)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			if strings.Contains(scenario, "delete") {
				cmd.SetArgs([]string{"remove"})
			} else {
				cmd.SetArgs([]string{"set", "--key-stdin"})
				cmd.SetIn(strings.NewReader("synthetic-key"))
			}
			err := cmd.Execute()
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe partial failure")
			}
			switch scenario {
			case "store save":
				if writes != 0 {
					t.Fatal("enrolled without saved key")
				}
			case "config after save":
				if store.saved != 1 || !strings.Contains(err.Error(), "key saved") {
					t.Fatal("lost partial-save explanation")
				}
			case "config before delete":
				if store.deleted != 0 {
					t.Fatal("deleted while still enrolled")
				}
			case "delete":
				if cfg.Agent != nil || !strings.Contains(err.Error(), "disabled in config") {
					t.Fatal("failed to disable or explain")
				}
			}
		})
	}
}

func TestAgentConfigStatusNeverRevealsKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enrolled  bool
		err       error
		key, want string
	}{
		{"disabled", false, nil, "private-key", "not_configured"},
		{"stored", true, nil, "private-key", "stored"},
		{"missing", true, agentcredential.ErrNotFound, "", "missing"},
		{"locked", true, errors.New("private store"), "", "unavailable"},
		{"invalid", true, nil, "private\nkey", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Path = filepath.Join(t.TempDir(), "config.toml")
			if tc.enrolled {
				cfg.Agent = &config.Agent{Backend: "typesafe"}
			}
			store := &fakeAgentCredentialStore{key: tc.key, err: tc.err}
			var out bytes.Buffer
			cmd := newAgentConfigCommandWithDependencies(&options{out: &out, errOut: &out, jsonOutput: true, configLoader: func(string) (config.Config, error) { return cfg, nil }}, agentConfigDependencies{
				store: store, lookupEnv: func(string) (string, bool) { return "", true },
			})
			cmd.SetArgs([]string{"status"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var result agentConfigResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Credential != tc.want || result.EnvironmentOverride != "disabled" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(out.String(), "private") || store.saved+store.deleted != 0 || (!tc.enrolled && store.loaded != 0) {
				t.Fatal("status accessed or exposed unexpected credential")
			}
		})
	}
}

func TestAgentConfigStatusInvalidEnvironment(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cmd := newAgentConfigCommandWithDependencies(&options{out: &out, errOut: &out, jsonOutput: true, configLoader: func(string) (config.Config, error) { return cfg, nil }}, agentConfigDependencies{
		lookupEnv: func(string) (string, bool) { return "private\nkey", true },
	})
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result agentConfigResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.EnvironmentOverride != "invalid" || result.RestartRequired != nil {
		t.Fatalf("status=%+v error=%v", result, err)
	}
	if strings.Contains(out.String(), "private") {
		t.Fatal("environment key leaked")
	}
}
