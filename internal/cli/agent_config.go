// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"papio/internal/agentcredential"
	"papio/internal/config"
)

type agentCredentialStore interface {
	Load(context.Context, string) (string, error)
	Save(context.Context, string, string) error
	Delete(context.Context, string) error
}

type agentConfigDependencies struct {
	store      agentCredentialStore
	saveConfig func(config.Config, string) error
	lookupEnv  func(string) (string, bool)
	readSecret func(io.Reader) ([]byte, error)
}

type agentConfigResult struct {
	Backend             string `json:"backend"`
	Credential          string `json:"credential"`
	EnvironmentOverride string `json:"environment_override"`
	RestartRequired     *bool  `json:"restart_required,omitempty"`
}

func newAgentConfigCommand(opt *options) *cobra.Command {
	return newAgentConfigCommandWithDependencies(opt, agentConfigDependencies{
		store: agentcredential.NewStore(), saveConfig: config.Save, lookupEnv: os.LookupEnv,
		readSecret: func(input io.Reader) ([]byte, error) {
			file, ok := input.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return nil, errors.New("use --key-stdin when supplying a key through a pipe")
			}
			return term.ReadPassword(int(file.Fd()))
		},
	})
}

func newAgentConfigCommandWithDependencies(opt *options, deps agentConfigDependencies) *cobra.Command {
	command := &cobra.Command{Use: "agent", Short: "Configure optional article-agent decisions", Annotations: map[string]string{"mcp:hidden": "true"}}
	noArgs := func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("this command accepts no positional arguments; supply the key at the hidden prompt or with --key-stdin")
		}
		return nil
	}
	environment := func() string {
		value, present := deps.lookupEnv("PAPIO_TYPESAFE_API_KEY")
		if !present {
			return "unset"
		}
		if strings.TrimSpace(value) == "" {
			return "disabled"
		}
		if agentcredential.ValidateKey(strings.TrimSpace(value)) != nil {
			return "invalid"
		}
		return "enabled"
	}
	load := func() (config.Config, string, error) {
		cfg, err := opt.loadConfig()
		if err != nil {
			return cfg, "", err
		}
		profile, err := agentcredential.Profile(cfg.Path, cfg.DataDir)
		return cfg, profile, err
	}
	var stdin bool
	set := &cobra.Command{Use: "set", Short: "Enable TypeSafe/Jev with a key saved in the OS credential store", Args: noArgs,
		Long: "Enable cloud article decisions for this configuration profile. This sends article DOIs, bounded titles and sanitized control descriptions to TypeSafe. The API key stays in the OS credential store. Input is hidden; use --key-stdin for a secret-manager pipe. Restart the daemon after changing this setting.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, profile, err := load()
			if err != nil {
				return err
			}
			if _, err := cfg.RequireAccessMode(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(opt.errOut, "Enabling TypeSafe/Jev sends article DOIs, bounded titles and sanitized control descriptions to TypeSafe.")
			var raw []byte
			if stdin {
				raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1027))
			} else {
				_, _ = fmt.Fprint(opt.errOut, "TypeSafe API key (hidden): ")
				raw, err = deps.readSecret(cmd.InOrStdin())
				_, _ = fmt.Fprintln(opt.errOut)
			}
			defer clear(raw)
			if err != nil {
				return errors.New("could not read the TypeSafe key; use the hidden terminal prompt or --key-stdin")
			}
			key := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
			if err := agentcredential.ValidateKey(key); err != nil {
				return err
			}
			if err := deps.store.Save(cmd.Context(), profile, key); err != nil {
				return err
			}
			cfg.Agent = &config.Agent{Backend: "typesafe"}
			if err := deps.saveConfig(cfg, cfg.Path); err != nil {
				return errors.New("key saved, but agent configuration could not be saved; run set again after fixing config access")
			}
			restart := true
			return opt.printResult(agentConfigResult{"typesafe", "stored", environment(), &restart}, "TypeSafe key saved for this profile. Restart the daemon to use it. PAPIO_TYPESAFE_API_KEY, when set, overrides this setting.")
		},
	}
	set.Flags().BoolVar(&stdin, "key-stdin", false, "read the API key from standard input instead of a hidden terminal prompt")
	status := &cobra.Command{Use: "status", Short: "Show agent configuration without revealing credentials", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, profile, err := load()
			if err != nil {
				return err
			}
			result := agentConfigResult{Backend: "none", Credential: "not_configured", EnvironmentOverride: environment()}
			if cfg.Agent != nil && cfg.Agent.Backend == "typesafe" {
				result.Backend = "typesafe"
				key, err := deps.store.Load(cmd.Context(), profile)
				switch {
				case errors.Is(err, agentcredential.ErrNotFound):
					result.Credential = "missing"
				case errors.Is(err, agentcredential.ErrInvalidKey):
					result.Credential = "invalid"
				case err != nil:
					result.Credential = "unavailable"
				case agentcredential.ValidateKey(key) != nil:
					result.Credential = "invalid"
				default:
					result.Credential = "stored"
				}
			}
			return opt.printResult(result, "Agent backend: %s; credential: %s; environment override: %s. This reports configuration, not the running daemon; restart after changes.", result.Backend, result.Credential, result.EnvironmentOverride)
		},
	}
	remove := &cobra.Command{Use: "remove", Short: "Disable the configured agent and remove its saved key", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, profile, err := load()
			if err != nil {
				return err
			}
			// Disable enrollment first. A locked store must not leave the next
			// daemon automatically using a key the operator asked to remove.
			cfg.Agent = nil
			if err := deps.saveConfig(cfg, cfg.Path); err != nil {
				return errors.New("agent configuration could not be disabled; saved key was not changed")
			}
			if err := deps.store.Delete(cmd.Context(), profile); err != nil && !errors.Is(err, agentcredential.ErrNotFound) {
				return errors.New("agent disabled in config, but saved key removal could not be confirmed; restart the daemon and retry remove after unlocking the credential store; PAPIO_TYPESAFE_API_KEY still overrides config")
			}
			restart := true
			return opt.printResult(agentConfigResult{"none", "removed", environment(), &restart}, "Saved agent key removed and agent disabled in config. Restart the daemon. Remove PAPIO_TYPESAFE_API_KEY from its launch environment too, if set.")
		},
	}
	command.AddCommand(set, status, remove)
	return command
}
