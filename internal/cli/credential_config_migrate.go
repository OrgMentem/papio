// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/credential"
	"papio/internal/runtimecredential"
)

type credentialStatusRow struct {
	Target              string `json:"target"`
	Reference           string `json:"reference,omitempty"`
	Source              string `json:"source"`
	State               string `json:"state"`
	EnvironmentOverride string `json:"environment_override,omitempty"`
}

func credentialStatuses(ctx context.Context, cfg config.Config, bindings []credential.Binding, deps credentialConfigDependencies) []credentialStatusRow {
	resolution := credential.Resolve(ctx, bindings, deps.store, deps.lookupEnv)
	rows := make([]credentialStatusRow, 0, len(bindings))
	for _, status := range resolution.Statuses() {
		row := credentialStatusRow{Target: status.Target, Reference: status.Reference, Source: string(status.Source), State: string(status.State)}
		if row.Target == "agent.typesafe" && row.Reference == "" {
			row = legacyAgentCredentialStatus(ctx, cfg, deps)
		}
		rows = append(rows, row)
	}
	return rows
}

func legacyAgentCredentialStatus(ctx context.Context, cfg config.Config, deps credentialConfigDependencies) credentialStatusRow {
	// Only this consumer is requested: avoid reading unrelated vault entries.
	selected := config.Config{Path: cfg.Path, DataDir: cfg.DataDir, Agent: cfg.Agent}
	runtime := runtimecredential.Resolve(ctx, selected, deps.store, deps.legacyStore.Load, deps.lookupEnv)
	for _, status := range runtime.Statuses() {
		if status.Target == "agent.typesafe" {
			return credentialStatusRow{Target: status.Target, Reference: status.Reference, Source: status.Source, State: status.State, EnvironmentOverride: agentEnvironmentStatus(deps, false)}
		}
	}
	return credentialStatusRow{Target: "agent.typesafe", Source: "none", State: "not_configured", EnvironmentOverride: agentEnvironmentStatus(deps, false)}
}

type credentialMigrationResult struct {
	Outcome         string                   `json:"outcome"`
	Credentials     []credentialConfigResult `json:"credentials"`
	RestartRequired bool                     `json:"restart_required"`
}

type credentialMigrationEntry struct {
	binding credential.Binding
	record  credential.Record
}

func migrateCredentials(ctx context.Context, opt *options, deps credentialConfigDependencies) (credentialMigrationResult, error) {
	result := credentialMigrationResult{Outcome: "unchanged", Credentials: []credentialConfigResult{}}
	cfg, snapshot, err := loadCredentialMutation(opt, deps)
	if err != nil {
		return result, err
	}
	deps, err = prepareCredentialEnvironment(deps, cfg, "")
	if err != nil {
		return result, err
	}
	if err := checkRawLegacyCredentialInventory(cfg); err != nil {
		return result, err
	}
	if err := checkLegacyCredentialInventory(cfg); err != nil {
		return result, err
	}
	agentBinding, err := credentialBinding(cfg, "agent.typesafe")
	if err != nil {
		return result, err
	}
	if agentBinding.Reference == "" {
		if _, present := deps.lookupEnv("PAPIO_TYPESAFE_API_KEY"); present {
			return result, errors.New("legacy PAPIO_TYPESAFE_API_KEY is present and controls the selected agent credential (an empty value disables it); remove that override before migration")
		}
	}
	inventory := make([]credentialMigrationEntry, 0)
	for _, binding := range cfg.CredentialBindings() {
		if binding.Reference != "" {
			continue
		}
		record := binding.Legacy
		if binding.Target == "agent.typesafe" && cfg.Agent != nil && cfg.Agent.Backend == "typesafe" {
			profile, err := agentcredential.Profile(cfg.Path, cfg.DataDir)
			if err != nil {
				return result, errors.New("legacy agent credential identity is invalid")
			}
			key, err := deps.legacyStore.Load(ctx, profile)
			if err != nil {
				return result, errors.New("legacy agent credential is missing or unavailable; migration has not changed configuration or stored records")
			}
			if agentcredential.ValidateKey(key) != nil {
				return result, errors.New("legacy agent credential is invalid; migration has not changed configuration or stored records")
			}
			record = credential.Record{Kind: credential.KindTypeSafe, APIKey: key}
		}
		if record == (credential.Record{}) {
			continue
		}
		if _, err := credential.Encode(record); err != nil || !credentialKindAllowed(record.Kind, binding.Kinds) {
			return result, errors.New("legacy credential inventory contains an invalid or unsupported record; no credentials were changed")
		}
		inventory = append(inventory, credentialMigrationEntry{binding, record})
	}
	if len(inventory) == 0 {
		return result, nil
	}
	// Inventory is complete before the first vault mutation. Never report values.
	for _, entry := range inventory {
		_, _ = fmt.Fprintf(opt.errOut, "Migrating credential for %s\n", entry.binding.Target)
	}
	next := cfg
	for _, entry := range inventory {
		ref, err := deps.newReference()
		if err != nil {
			return result, stagedCredentialError(errors.New("credential reference could not be created"), result.Credentials)
		}
		staged := credentialConfigResult{Target: entry.binding.Target, Reference: ref, Outcome: "staged", RestartRequired: true}
		result.Credentials = append(result.Credentials, staged)
		if err := stageCredential(ctx, deps, ref, entry.record); err != nil {
			return result, stagedCredentialError(err, result.Credentials)
		}
		next, err = next.WithCredentialReference(entry.binding.Target, ref)
		if err != nil {
			return result, stagedCredentialError(errors.New("migrated credential configuration is invalid"), result.Credentials)
		}
	}
	if err := deps.saveConfig(next, cfg.Path, snapshot); err != nil {
		return result, stagedCredentialError(errors.New("configuration changed or could not be saved; original configuration was not replaced"), result.Credentials)
	}
	for i, entry := range inventory {
		binding, err := credentialBinding(next, entry.binding.Target)
		if err != nil {
			return result, errors.New("configuration saved, but migrated bindings could not be verified")
		}
		actual, err := requireCredential(ctx, deps, binding)
		if err != nil || actual != entry.record {
			return result, errors.New("configuration saved, but migrated credential resolution could not be verified; old vault entries are unchanged; inspect credentials status before restarting")
		}
		result.Credentials[i].Outcome = "migrated"
	}
	result.Outcome = "migrated"
	result.RestartRequired = true
	return result, nil
}

// Migration has stricter inventory rules than backward-compatible config reads.
func checkLegacyCredentialInventory(cfg config.Config) error {
	return cfg.ValidateCredentialMigration()
}

// Load deliberately tolerates and removes retired sources. Migration must read
// the original source inventory too, or publishing the normalized Config could
// erase an unsupported secret without ever reporting it.
func checkRawLegacyCredentialInventory(cfg config.Config) error {
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("original credential inventory could not be read")
	}
	defer clear(raw)
	var document struct {
		Sources map[string]struct {
			APIKey        string `toml:"api_key"`
			ClientID      string `toml:"client_id"`
			ClientSecret  string `toml:"client_secret"`
			CredentialRef string `toml:"credential_ref"`
		} `toml:"sources"`
	}
	if toml.Unmarshal(raw, &document) != nil {
		return errors.New("original credential inventory could not be parsed")
	}
	supported := make(map[string]bool)
	for _, binding := range cfg.CredentialBindings() {
		if strings.HasPrefix(binding.Target, "sources.") {
			supported[strings.TrimPrefix(binding.Target, "sources.")] = true
		}
	}
	for name, source := range document.Sources {
		if !supported[name] && (source.APIKey != "" || source.ClientID != "" || source.ClientSecret != "" || source.CredentialRef != "") {
			return errors.New("the original config contains unsupported or retired source credentials; resolve those settings explicitly before migration")
		}
	}
	return nil
}
