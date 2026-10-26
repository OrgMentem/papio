// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package runtimecredential resolves integration credentials without changing
// serializable configuration. One immutable view supplies clients and policies.
package runtimecredential

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"papio/internal/agentcredential"
	"papio/internal/config"
	"papio/internal/credential"
)

const legacyAgentEnv = "PAPIO_TYPESAFE_API_KEY"

var ErrEnvironmentAlreadyConsumed = errors.New("environment credential already consumed")

type EnvironmentAlreadyConsumedError struct {
	Name string
}

func (e *EnvironmentAlreadyConsumedError) Error() string {
	return "environment credential " + e.Name + " already consumed; reuse the EnvironmentSnapshot"
}

func (*EnvironmentAlreadyConsumedError) Unwrap() error { return ErrEnvironmentAlreadyConsumed }

// Keep only names here. Credential values belong exclusively to their snapshots.
var consumedEnvironment = struct {
	sync.Mutex
	names map[string]bool
}{names: make(map[string]bool)}

type LegacyLoader func(context.Context, string) (string, error)
type LookupEnv func(string) (string, bool)

type Runtime struct {
	cfg         config.Config
	resolved    *credential.Resolution
	agentKey    string
	agentStatus credential.Status
}

func (r *Runtime) String() string               { return "Papio runtime credentials (redacted)" }
func (r *Runtime) GoString() string             { return r.String() }
func (r *Runtime) MarshalJSON() ([]byte, error) { return json.Marshal(r.Statuses()) }

// EnvironmentSnapshot owns one profile's captured environment. Reuse it
// explicitly for repeated initialization; a fresh capture reads only the OS
// environment and never inherits an earlier capture.
type EnvironmentSnapshot struct {
	path     string
	dataDir  string
	bindings map[string]string
	values   map[string]string
}

func (*EnvironmentSnapshot) String() string     { return "Papio environment snapshot (redacted)" }
func (s *EnvironmentSnapshot) GoString() string { return s.String() }

func (*EnvironmentSnapshot) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

// Lookup returns the captured values only for the original profile and binding
// set. A changed profile or target must capture its own environment.
func (s *EnvironmentSnapshot) Lookup(cfg config.Config) LookupEnv {
	missing := func(string) (string, bool) { return "", false }
	if s == nil || s.path != cfg.Path || s.dataDir != cfg.DataDir {
		return missing
	}
	bindings := cfg.CredentialBindings()
	if len(bindings) != len(s.bindings) {
		return missing
	}
	for _, binding := range bindings {
		if ref, ok := s.bindings[binding.Target]; !ok || ref != binding.Reference {
			return missing
		}
	}
	return func(name string) (string, bool) { value, ok := s.values[name]; return value, ok }
}

// TakeEnvironment snapshots all explicitly selected variables before removing
// any, since several bindings may use the same variable. It runs before the
// first vault access so helpers cannot inherit these keys. Callers that need
// another resolution must explicitly reuse the returned snapshot.
// A missing variable consumed by this process is an error, not an absent key.
func TakeEnvironment(cfg config.Config) (*EnvironmentSnapshot, error) {
	consumedEnvironment.Lock()
	defer consumedEnvironment.Unlock()
	snapshot := &EnvironmentSnapshot{
		path: cfg.Path, dataDir: cfg.DataDir,
		bindings: make(map[string]string),
		values:   make(map[string]string),
	}
	names := map[string]bool{legacyAgentEnv: true}
	for _, binding := range cfg.CredentialBindings() {
		snapshot.bindings[binding.Target] = binding.Reference
		if name, ok := credential.EnvironmentName(binding.Reference); ok {
			names[name] = true
		}
	}
	for name := range names {
		if value, ok := os.LookupEnv(name); ok {
			snapshot.values[name] = value
			delete(consumedEnvironment.names, name)
		} else if consumedEnvironment.names[name] {
			return nil, &EnvironmentAlreadyConsumedError{Name: name}
		}
	}
	for name := range names {
		if err := os.Unsetenv(name); err != nil {
			return nil, errors.New("could not isolate integration credentials from child processes")
		}
		if value := snapshot.values[name]; value != "" {
			consumedEnvironment.names[name] = true
		}
	}
	return snapshot, nil
}

// Resolve reads only the records explicitly bound to this profile. Legacy
// literals keep their old client semantics until explicit migration. The old
// TypeSafe path-derived record and environment override remain compatibility
// inputs only when this profile has no new reference.
func Resolve(ctx context.Context, cfg config.Config, reader credential.Reader, legacy LegacyLoader, env LookupEnv) *Runtime {
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	r := &Runtime{cfg: cfg, resolved: credential.Resolve(ctx, cfg.CredentialBindings(), reader, env)}
	r.agentStatus = credential.Status{Target: "agent.typesafe", Source: "none", State: "not_configured"}
	if cfg.Agent != nil && cfg.Agent.CredentialRef != "" {
		// A reference never enrolls an otherwise disabled backend.
		if cfg.Agent.Backend == "typesafe" {
			if record, ok := r.resolved.Record("agent.typesafe"); ok {
				r.agentKey = record.APIKey
			}
		}
		return r
	}
	key, overridden := env(legacyAgentEnv)
	if overridden {
		r.agentStatus.Source = "environment"
		key = strings.TrimSpace(key)
		if key == "" {
			return r
		} // Legacy empty override explicitly disables.
	} else {
		if cfg.Agent == nil || cfg.Agent.Backend != "typesafe" {
			return r
		}
		r.agentStatus.Source = "legacy"
		profile, err := agentcredential.Profile(cfg.Path, cfg.DataDir)
		if err != nil || legacy == nil {
			r.agentStatus.State = "unavailable"
			return r
		}
		key, err = legacy(ctx, profile)
		if err != nil {
			r.agentStatus.State = "unavailable"
			if errors.Is(err, agentcredential.ErrNotFound) {
				r.agentStatus.State = "missing"
			}
			if errors.Is(err, agentcredential.ErrInvalidKey) {
				r.agentStatus.State = "invalid"
			}
			return r
		}
	}
	if agentcredential.ValidateKey(key) != nil {
		r.agentStatus.State = "invalid"
		return r
	}
	r.agentKey, r.agentStatus.State = key, "ready"
	return r
}

// SourcePolicy includes only this source's resolved credential. A missing
// optional key retains keyless operation; required-key acquisition is disabled.
func (r *Runtime) SourcePolicy(name string) config.Source {
	p := r.cfg.Sources[name]
	if p.CredentialRef == "" {
		return r.cfg.SourcePolicy(name)
	}
	p.APIKey, p.ClientID, p.ClientSecret = "", "", ""
	if record, ok := r.resolved.Record("sources." + name); ok {
		p.APIKey, p.ClientID, p.ClientSecret = record.APIKey, record.ClientID, record.ClientSecret
	} else if name == config.SourceCORE || name == config.SourceCrossrefTDM {
		p.Enabled = false
	}
	return config.EffectiveSourcePolicy(name, p)
}

func (r *Runtime) InstitutionFor(name string) (config.Institution, bool) {
	inst, ok := r.cfg.InstitutionFor(name)
	if inst.DocumentDelivery == nil || inst.DocumentDelivery.CredentialRef == "" {
		return inst, ok
	}
	dd := *inst.DocumentDelivery
	dd.APIKey = ""
	target := "browser.document_delivery"
	if name != "" && name != "default" {
		target = "browser.resolvers." + name + ".document_delivery"
	}
	if record, found := r.resolved.Record(target); found {
		dd.APIKey = record.APIKey
	}
	inst.DocumentDelivery = &dd
	return inst, ok
}

func (r *Runtime) Webhook() (endpoint, bearer string) {
	if r.cfg.Notify.WebhookCredentialRef == "" {
		return r.cfg.Notify.WebhookURL, r.cfg.Notify.WebhookSecret
	}
	if record, ok := r.resolved.Record("notify.webhook"); ok {
		return record.URL, record.Bearer
	}
	return "", ""
}

func (r *Runtime) WebhookConfigured() bool { endpoint, _ := r.Webhook(); return endpoint != "" }
func (r *Runtime) AgentKey() string        { return r.agentKey }

func (r *Runtime) Statuses() []credential.Status {
	statuses := r.resolved.Statuses()
	if r.cfg.Agent != nil && r.cfg.Agent.CredentialRef != "" {
		return statuses
	}
	for i := range statuses {
		if statuses[i].Target == "agent.typesafe" {
			statuses[i] = r.agentStatus
			return statuses
		}
	}
	return append(statuses, r.agentStatus)
}
