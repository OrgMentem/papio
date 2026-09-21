// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/credential"
	"papio/internal/runtimecredential"
)

type credentialConfigStore interface {
	credential.Reader
	Save(context.Context, string, credential.Record) error
	Delete(context.Context, string) error
}

type credentialConfigDependencies struct {
	store          credentialConfigStore
	legacyStore    agentCredentialStore
	lookupEnv      func(string) (string, bool)
	readSecret     func(io.Reader) ([]byte, error)
	readSnapshot   func(string) (config.Snapshot, error)
	saveConfig     func(config.Config, string, config.Snapshot) error
	loadDiskConfig func(string) (config.Config, error)
	newReference   func() (string, error)
	prepareEnv     func(config.Config, string) (runtimecredential.LookupEnv, error)
}

func defaultCredentialConfigDependencies() credentialConfigDependencies {
	return credentialConfigDependencies{
		store: credential.NewStore(), legacyStore: agentcredential.NewStore(), lookupEnv: os.LookupEnv,
		readSnapshot: config.ReadSnapshot, saveConfig: config.SaveIfUnchanged, loadDiskConfig: config.Load,
		newReference: credential.NewReference, prepareEnv: takeCredentialCommandEnvironment,
		readSecret: func(input io.Reader) ([]byte, error) {
			file, ok := input.(*os.File)
			if !ok || !term.IsTerminal(int(file.Fd())) {
				return nil, errors.New("use --key-stdin when supplying a key through a pipe")
			}
			return term.ReadPassword(int(file.Fd()))
		},
	}
}

type credentialConfigResult struct {
	Target              string `json:"target,omitempty"`
	Reference           string `json:"reference,omitempty"`
	PreviousReference   string `json:"previous_reference,omitempty"`
	Outcome             string `json:"outcome"`
	RestartRequired     bool   `json:"restart_required"`
	EnvironmentOverride string `json:"environment_override,omitempty"`
}

func credentialArgs(min, max int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return errors.New("incorrect arguments; credentials must be supplied at the hidden prompt or through --key-stdin, never as arguments")
		}
		return nil
	}
}

func newCredentialConfigCommand(opt *options) *cobra.Command {
	return newCredentialConfigCommandWithDependencies(opt, defaultCredentialConfigDependencies())
}

func newCredentialConfigCommandWithDependencies(opt *options, deps credentialConfigDependencies) *cobra.Command {
	command := &cobra.Command{Use: "credentials", Short: "Manage integration credentials in the OS store or environment", Annotations: map[string]string{"mcp:hidden": "true"}}
	var stdin bool
	set := &cobra.Command{Use: "set TARGET", Short: "Save a new credential and bind this configuration to it", Args: credentialArgs(1, 1),
		Long: "Create a fresh OS credential record for one integration. Supply a single API key at the hidden prompt or with --key-stdin. For sources.openaire and notify.webhook, use --key-stdin with a version-1 typed JSON credential record. TypeSafe setup enrolls this profile in cloud article decisions. Restart the daemon after changes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := setCredential(cmd, opt, deps, args[0], stdin)
			if err != nil {
				return err
			}
			return opt.printResult(result, "Credential saved and bound to %s (%s). Restart the daemon to use it.%s", result.Target, result.Reference, retainedCredentialNotice(result.PreviousReference))
		},
	}
	set.Flags().BoolVar(&stdin, "key-stdin", false, "read a key or typed JSON record from standard input")
	bind := &cobra.Command{Use: "bind TARGET REFERENCE", Short: "Reuse an existing typed credential for this configuration", Args: credentialArgs(2, 2), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, snapshot, err := loadCredentialMutation(opt, deps)
		if err != nil {
			return err
		}
		binding, err := credentialBinding(cfg, args[0])
		if err != nil {
			return err
		}
		if err := credential.ValidateReference(args[1]); err != nil {
			return credential.ErrInvalidReference
		}
		deps, err = prepareCredentialEnvironment(deps, cfg, args[1])
		if err != nil {
			return err
		}
		previousReference := binding.Reference
		binding.Reference = args[1]
		binding.Legacy = credential.Record{}
		if _, err := requireCredential(cmd.Context(), deps, binding); err != nil {
			return err
		}
		next, err := cfg.WithCredentialReference(binding.Target, args[1])
		if err != nil {
			return errors.New("credential reference is not valid for this integration")
		}
		if err := deps.saveConfig(next, cfg.Path, snapshot); err != nil {
			return errors.New("configuration changed or could not be saved; binding was not published")
		}
		if _, err := requireCredential(cmd.Context(), deps, binding); err != nil {
			return fmt.Errorf("binding saved (%s), but credential resolution could not be verified; inspect credentials status before restarting.%s", args[1], retainedCredentialNotice(previousReference))
		}
		return opt.printResult(credentialConfigResult{Target: binding.Target, Reference: args[1], PreviousReference: previousReference, Outcome: "bound", RestartRequired: true}, "Credential bound to %s (%s). Restart the daemon to use it.%s", binding.Target, args[1], retainedCredentialNotice(previousReference))
	}}
	status := &cobra.Command{Use: "status [TARGET]", Short: "Show selected credential sources without revealing values", Args: credentialArgs(0, 1), Annotations: map[string]string{"mcp:read-only": "true"}, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := opt.loadConfig()
		if err != nil {
			return errors.New("configuration could not be loaded")
		}
		depsForCall, err := prepareCredentialEnvironment(deps, cfg, "")
		if err != nil {
			return err
		}
		bindings := cfg.CredentialBindings()
		if len(args) == 1 {
			b, err := credentialBinding(cfg, args[0])
			if err != nil {
				return err
			}
			bindings = []credential.Binding{b}
		}
		rows := credentialStatuses(cmd.Context(), cfg, bindings, depsForCall)
		if opt.jsonOutput {
			return printPage(opt, "credentials", rows, false)
		}
		for _, row := range rows {
			selection := row.Source
			if row.Reference != "" {
				selection += "; reference: " + row.Reference
			}
			if _, err := fmt.Fprintf(opt.out, "%s: %s (%s)\n", row.Target, row.State, selection); err != nil {
				return err
			}
		}
		return nil
	}}
	detach := &cobra.Command{Use: "detach TARGET", Short: "Remove a configuration binding while preserving the stored record", Args: credentialArgs(1, 1), RunE: func(cmd *cobra.Command, args []string) error {
		result, err := detachCredential(opt, deps, args[0])
		if err != nil {
			return err
		}
		return opt.printResult(result, "Credential detached from %s; stored records are unchanged. Restart the daemon.%s", result.Target, retainedCredentialNotice(result.Reference))
	}}
	del := &cobra.Command{Use: "delete REFERENCE", Short: "Delete a stored record without changing configurations that reference it", Args: credentialArgs(1, 1), RunE: func(cmd *cobra.Command, args []string) error {
		ref := args[0]
		if credential.ValidateReference(ref) != nil || !strings.HasPrefix(ref, "keyring:") {
			return errors.New("delete requires a valid keyring reference; environment credentials cannot be deleted by Papio")
		}
		cfg, err := opt.loadConfig()
		if err != nil {
			return errors.New("configuration could not be loaded before accessing the credential store")
		}
		depsForCall, err := prepareCredentialEnvironment(deps, cfg, "")
		if err != nil {
			return err
		}
		if err := depsForCall.store.Delete(cmd.Context(), ref); err != nil && !errors.Is(err, credential.ErrNotFound) {
			return sanitizedCredentialError(err)
		}
		return opt.printResult(credentialConfigResult{Reference: ref, Outcome: "deleted", RestartRequired: true}, "Stored credential deleted. Configurations that reference it are unchanged; restart affected daemons.")
	}}
	migrate := &cobra.Command{Use: "migrate", Short: "Move this configuration's legacy secrets into verified OS records", Args: credentialArgs(0, 0), Long: "Migrate supported literal credentials and this profile's legacy TypeSafe key. New records are verified before one guarded config update. Existing vault entries are retained. Interrupted operations report staged references for inspection or explicit deletion. No plaintext backup is created. An active legacy TypeSafe environment override must be removed before migration.", RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := migrateCredentials(cmd.Context(), opt, deps)
		if err != nil {
			return err
		}
		return opt.printResult(result, "Migrated %d credential binding(s). Legacy vault entries are unchanged. Restart the daemon.", len(result.Credentials))
	}}
	command.AddCommand(set, bind, status, detach, del, migrate)
	return command
}

// Capture before loading the config or touching the vault. Check both bytes and
// the supplied Config, because an embedding may have loaded it before this call.
func loadCredentialMutation(opt *options, deps credentialConfigDependencies) (config.Config, config.Snapshot, error) {
	path := opt.configPath
	if path == "" {
		path = filepath.Join(config.Dir(), "config.toml")
	}
	snapshot, err := deps.readSnapshot(path)
	if err != nil {
		return config.Config{}, snapshot, errors.New("configuration snapshot could not be read")
	}
	cfg, err := opt.loadConfig()
	if err != nil {
		return cfg, snapshot, errors.New("configuration could not be loaded")
	}
	disk, err := deps.loadDiskConfig(path)
	if err != nil {
		return cfg, snapshot, errors.New("configuration could not be verified")
	}
	current, err := deps.readSnapshot(path)
	if err != nil || !reflect.DeepEqual(snapshot, current) || !reflect.DeepEqual(cfg, disk) {
		return cfg, snapshot, errors.New("configuration changed since it was loaded; reload it and retry")
	}
	if _, err := cfg.RequireAccessMode(); err != nil {
		return cfg, snapshot, err
	}
	return cfg, snapshot, nil
}

func credentialBinding(cfg config.Config, target string) (credential.Binding, error) {
	for _, binding := range cfg.CredentialBindings() {
		if binding.Target == target {
			return binding, nil
		}
	}
	return credential.Binding{}, errors.New("unknown credential target; use credentials status to list available targets")
}

func requireCredential(ctx context.Context, deps credentialConfigDependencies, binding credential.Binding) (credential.Record, error) {
	resolution := credential.Resolve(ctx, []credential.Binding{binding}, deps.store, deps.lookupEnv)
	if record, ok := resolution.Record(binding.Target); ok {
		return record, nil
	}
	for _, status := range resolution.Statuses() {
		switch string(status.State) {
		case "missing", "not_configured":
			return credential.Record{}, credential.ErrNotFound
		case "invalid":
			return credential.Record{}, credential.ErrInvalidRecord
		}
	}
	return credential.Record{}, credential.ErrUnavailable
}

func setCredential(cmd *cobra.Command, opt *options, deps credentialConfigDependencies, target string, stdin bool) (credentialConfigResult, error) {
	cfg, snapshot, err := loadCredentialMutation(opt, deps)
	if err != nil {
		return credentialConfigResult{}, err
	}
	deps, err = prepareCredentialEnvironment(deps, cfg, "")
	if err != nil {
		return credentialConfigResult{}, err
	}
	binding, err := credentialBinding(cfg, target)
	if err != nil {
		return credentialConfigResult{}, err
	}
	if target == "agent.typesafe" {
		_, _ = fmt.Fprintln(opt.errOut, "Enabling TypeSafe/Jev sends article DOIs, bounded titles and sanitized control descriptions to TypeSafe.")
	}
	structured := target == "sources.openaire" || target == "notify.webhook"
	if structured && !stdin {
		return credentialConfigResult{}, errors.New("this integration requires --key-stdin with a version-1 typed JSON credential record")
	}
	var raw []byte
	if stdin {
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 2051))
	} else {
		_, _ = fmt.Fprint(opt.errOut, "API key (hidden): ")
		raw, err = deps.readSecret(cmd.InOrStdin())
		_, _ = fmt.Fprintln(opt.errOut)
	}
	defer clear(raw)
	if err != nil {
		return credentialConfigResult{}, errors.New("credential input could not be read")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	var record credential.Record
	if structured {
		record, err = credential.Decode(value)
	} else {
		record, err = credential.DecodeEnvironment(value, binding.Kinds)
	}
	if err != nil || !credentialKindAllowed(record.Kind, binding.Kinds) {
		return credentialConfigResult{}, credential.ErrInvalidRecord
	}
	if _, err := credential.Encode(record); err != nil {
		return credentialConfigResult{}, credential.ErrInvalidRecord
	}
	ref, err := deps.newReference()
	if err != nil {
		return credentialConfigResult{}, errors.New("credential reference could not be created")
	}
	if err := stageCredential(cmd.Context(), deps, ref, record); err != nil {
		return credentialConfigResult{}, stagedCredentialError(err, []credentialConfigResult{{Target: target, Reference: ref, Outcome: "staged"}})
	}
	next, err := cfg.WithCredentialReference(target, ref)
	if err != nil {
		return credentialConfigResult{}, stagedCredentialError(errors.New("credential configuration is invalid"), []credentialConfigResult{{Target: target, Reference: ref, Outcome: "staged"}})
	}
	result := credentialConfigResult{Target: target, Reference: ref, PreviousReference: binding.Reference, Outcome: "stored", RestartRequired: true}
	if target == "agent.typesafe" {
		result.EnvironmentOverride = agentEnvironmentStatus(deps, true)
	}
	if err := deps.saveConfig(next, cfg.Path, snapshot); err != nil {
		return credentialConfigResult{}, stagedCredentialError(errors.New("configuration changed or could not be saved; original configuration was not replaced"), []credentialConfigResult{result})
	}
	binding.Reference = ref
	binding.Legacy = credential.Record{}
	if actual, err := requireCredential(cmd.Context(), deps, binding); err != nil || actual != record {
		return credentialConfigResult{}, fmt.Errorf("configuration saved (%s), but credential resolution could not be verified; inspect credentials status before restarting.%s", ref, retainedCredentialNotice(result.PreviousReference))
	}
	return result, nil
}

func detachCredential(opt *options, deps credentialConfigDependencies, target string) (credentialConfigResult, error) {
	cfg, snapshot, err := loadCredentialMutation(opt, deps)
	if err != nil {
		return credentialConfigResult{}, err
	}
	binding, err := credentialBinding(cfg, target)
	if err != nil {
		return credentialConfigResult{}, err
	}
	next, err := cfg.WithCredentialReference(target, "")
	if err != nil {
		return credentialConfigResult{}, errors.New("credential configuration could not be detached")
	}
	// Removing agent enrollment must also disable its legacy path-derived reader.
	if target == "agent.typesafe" {
		next.Agent = nil
	}
	if err := deps.saveConfig(next, cfg.Path, snapshot); err != nil {
		return credentialConfigResult{}, errors.New("configuration changed or could not be saved; credential was not detached")
	}
	return credentialConfigResult{Target: target, Reference: binding.Reference, Outcome: "detached", RestartRequired: true}, nil
}

func credentialKindAllowed(kind credential.Kind, allowed []credential.Kind) bool {
	for _, k := range allowed {
		if k == kind {
			return true
		}
	}
	return false
}

func sanitizedCredentialError(err error) error {
	for _, known := range []error{credential.ErrNotFound, credential.ErrInvalidRecord, credential.ErrInvalidReference, credential.ErrUnavailable, credential.ErrBusy, credential.ErrUncertain} {
		if errors.Is(err, known) {
			return known
		}
	}
	return credential.ErrUnavailable
}

func stageCredential(ctx context.Context, deps credentialConfigDependencies, ref string, record credential.Record) error {
	if err := deps.store.Save(ctx, ref, record); err != nil {
		return sanitizedCredentialError(err)
	}
	actual, err := deps.store.Load(ctx, ref)
	if err != nil {
		return sanitizedCredentialError(err)
	}
	if actual != record {
		return errors.New("saved credential readback did not match")
	}
	return nil
}

// Only references, target names and fixed error strings may cross this boundary.
func stagedCredentialError(cause error, staged []credentialConfigResult) error {
	refs := make([]string, 0, len(staged))
	for _, r := range staged {
		refs = append(refs, r.Reference)
	}
	return fmt.Errorf("%s; staged credential references (a dispatched write may still complete): %s; inspect status or explicitly delete unused records", cause.Error(), strings.Join(refs, ", "))
}

// Capture the prospective env binding as well as the profile's current inputs.
// Even the OS-store helper must not inherit either selected credential.
func takeCredentialCommandEnvironment(cfg config.Config, extraRef string) (runtimecredential.LookupEnv, error) {
	name, extra := credential.EnvironmentName(extraRef)
	var value string
	var present bool
	if extra {
		value, present = os.LookupEnv(name)
	}
	lookup, err := runtimecredential.TakeEnvironment(cfg)
	if err != nil {
		return nil, err
	}
	if extra {
		if err := os.Unsetenv(name); err != nil {
			return nil, errors.New("could not isolate integration credentials from child processes")
		}
		return func(n string) (string, bool) {
			if n == name {
				return value, present
			}
			return lookup(n)
		}, nil
	}
	return lookup, nil
}

func prepareCredentialEnvironment(deps credentialConfigDependencies, cfg config.Config, extraRef string) (credentialConfigDependencies, error) {
	if deps.prepareEnv == nil {
		return deps, nil
	}
	lookup, err := deps.prepareEnv(cfg, extraRef)
	if err != nil {
		return deps, errors.New("could not isolate integration credentials from child processes")
	}
	deps.lookupEnv = lookup
	return deps, nil
}

func retainedCredentialNotice(ref string) string {
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "env:") {
		return " Previous environment reference: " + ref + "; its value was not changed."
	}
	return " Retained credential: " + ref + ". Delete it explicitly only when no profile needs it."
}
