// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/agentcredential"
	"papio/internal/credential"
)

// Retained only for reading pre-ADR-0030 profile-derived credentials. New writes
// use the common typed store through credentialConfigDependencies.
type agentCredentialStore interface {
	Load(context.Context, string) (string, error)
	Save(context.Context, string, string) error
	Delete(context.Context, string) error
}

type agentConfigDependencies = credentialConfigDependencies

type agentConfigResult struct {
	Backend             string `json:"backend"`
	Credential          string `json:"credential"`
	EnvironmentOverride string `json:"environment_override"`
	Reference           string `json:"reference,omitempty"`
	PreviousReference   string `json:"previous_reference,omitempty"`
	RestartRequired     *bool  `json:"restart_required,omitempty"`
}

func newAgentConfigCommand(opt *options) *cobra.Command {
	return newAgentConfigCommandWithDependencies(opt, defaultCredentialConfigDependencies())
}

// These compatibility commands share storage, binding and publication with
// config credentials. Removal detaches enrollment; it never deletes a record
// that another profile may have deliberately bound.
func newAgentConfigCommandWithDependencies(opt *options, deps agentConfigDependencies) *cobra.Command {
	command := &cobra.Command{Use: "agent", Short: "Configure optional article-agent decisions", Annotations: map[string]string{"mcp:hidden": "true"}}
	var stdin bool
	set := &cobra.Command{Use: "set", Short: "Enable TypeSafe/Jev with a key saved in the OS credential store", Args: credentialArgs(0, 0), Long: "Enable cloud article decisions for this configuration profile. This sends article DOIs, bounded titles and sanitized control descriptions to TypeSafe. The key is stored as a shared-service typed credential with an explicit reference. Input is hidden; use --key-stdin for a secret-manager pipe. Restart the daemon after changing this setting.", RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := setCredential(cmd, opt, deps, "agent.typesafe", stdin)
		if err != nil {
			return err
		}
		restart := true
		return opt.printResult(agentConfigResult{Backend: "typesafe", Credential: "stored", EnvironmentOverride: result.EnvironmentOverride, Reference: result.Reference, PreviousReference: result.PreviousReference, RestartRequired: &restart}, "TypeSafe key saved and bound to this profile (%s). Restart the daemon. This explicit reference takes precedence over PAPIO_TYPESAFE_API_KEY.%s", result.Reference, retainedCredentialNotice(result.PreviousReference))
	}}
	set.Flags().BoolVar(&stdin, "key-stdin", false, "read the API key from standard input instead of a hidden terminal prompt")
	status := &cobra.Command{Use: "status", Short: "Show agent configuration without revealing credentials", Args: credentialArgs(0, 0), Annotations: map[string]string{"mcp:read-only": "true"}, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := opt.loadConfig()
		if err != nil {
			return errors.New("configuration could not be loaded")
		}
		depsForCall, err := prepareCredentialEnvironment(deps, cfg, "")
		if err != nil {
			return err
		}
		result := agentConfigResult{Backend: "none", Credential: "not_configured", EnvironmentOverride: agentEnvironmentStatus(depsForCall, false)}
		binding, err := credentialBinding(cfg, "agent.typesafe")
		if err != nil {
			return err
		}
		rows := credentialStatuses(cmd.Context(), cfg, []credential.Binding{binding}, depsForCall)
		if cfg.Agent != nil && cfg.Agent.Backend == "typesafe" {
			result.Backend = "typesafe"
		}
		if len(rows) != 0 {
			result.Credential = rows[0].State
			if result.Credential == "ready" {
				result.Credential = "stored"
			}
			if rows[0].Source == "environment" && binding.Reference == "" && rows[0].State == "ready" {
				result.Credential = "environment"
			}
		}
		result.Reference = binding.Reference
		result.EnvironmentOverride = agentEnvironmentStatus(depsForCall, binding.Reference != "")
		return opt.printResult(result, "Agent backend: %s; credential: %s; environment override: %s. This reports configuration, not the running daemon; restart after changes.", result.Backend, result.Credential, result.EnvironmentOverride)
	}}
	remove := &cobra.Command{Use: "remove", Short: "Disable the configured agent while preserving stored credentials", Args: credentialArgs(0, 0), RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := detachCredential(opt, deps, "agent.typesafe")
		if err != nil {
			return err
		}
		restart := true
		return opt.printResult(agentConfigResult{Backend: "none", Credential: "detached", Reference: result.Reference, EnvironmentOverride: agentEnvironmentStatus(deps, false), RestartRequired: &restart}, "Agent disabled in config; stored credentials are unchanged. Restart the daemon. Remove PAPIO_TYPESAFE_API_KEY from its launch environment too, if set. Use config credentials delete REFERENCE to explicitly delete a shared record.%s", retainedCredentialNotice(result.Reference))
	}}
	command.AddCommand(set, status, remove)
	return command
}

func agentEnvironmentStatus(deps credentialConfigDependencies, hasReference bool) string {
	value, present := deps.lookupEnv("PAPIO_TYPESAFE_API_KEY")
	if !present {
		return "unset"
	}
	if hasReference {
		return "ignored"
	}
	if strings.TrimSpace(value) == "" {
		return "disabled"
	}
	if agentcredential.ValidateKey(strings.TrimSpace(value)) != nil {
		return "invalid"
	}
	return "enabled"
}
