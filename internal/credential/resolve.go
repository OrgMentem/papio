// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"context"
	"encoding/json"
	"errors"
)

// Binding names a consumer, its explicit reference and accepted record kinds.
// Legacy is considered only when Reference is empty; it is never a fallback.
type Binding struct {
	Target    string
	Reference string
	Kinds     []Kind
	Legacy    Record
}

// Status contains no resolved credential, environment value or backend error.
type Status struct {
	Target    string `json:"target"`
	Reference string `json:"reference"`
	Source    string `json:"source"`
	State     string `json:"state"`
}

// Resolution owns a startup snapshot separately from serializable config.
// Its records are immutable and accessible only by an explicit consumer lookup.
type Resolution struct {
	records  map[string]Record
	statuses []Status
}

func (Resolution) String() string   { return "credential.Resolution{redacted}" }
func (Resolution) GoString() string { return "credential.Resolution{redacted}" }

// MarshalJSON exposes diagnostics only, never the private records.
func (r Resolution) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Statuses []Status `json:"statuses"`
	}{r.Statuses()})
}

// Record returns a copy of the selected credential for target, if ready.
func (r *Resolution) Record(target string) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	record, ok := r.records[target]
	return record, ok
}

// Statuses returns a copy in binding order so callers cannot alter resolution.
func (r *Resolution) Statuses() []Status {
	if r == nil {
		return []Status{}
	}
	return append([]Status{}, r.statuses...)
}

// Resolve selects each binding independently. An explicit reference is always
// authoritative, including missing, unavailable and wrong-kind entries. Shared
// references are read once per resolution so their consumers see one snapshot.
// lookupEnv should read a previously captured environment; the bootstrap caller
// owns scrubbing those variables before it launches any worker or store helper.
func Resolve(ctx context.Context, bindings []Binding, reader Reader, lookupEnv func(string) (string, bool)) *Resolution {
	r := &Resolution{records: make(map[string]Record), statuses: make([]Status, 0, len(bindings))}
	targets := make(map[string]int)
	for _, binding := range bindings {
		targets[binding.Target]++
	}
	type loaded struct {
		record Record
		err    error
	}
	storeCache := make(map[string]loaded)
	type environment struct {
		value   string
		present bool
	}
	envCache := make(map[string]environment)
	for _, binding := range bindings {
		status := Status{Target: binding.Target, Reference: binding.Reference, Source: "none", State: "not_configured"}
		var record Record
		var err error
		switch {
		case binding.Reference != "":
			if ValidateReference(binding.Reference) != nil {
				// Invalid references may themselves contain a pasted secret. Do
				// not echo them into otherwise safe status or CLI JSON output.
				status.Reference = ""
				err = ErrInvalidReference
				break
			}
			if name, ok := EnvironmentName(binding.Reference); ok {
				status.Source = "environment"
				value, cached := envCache[name]
				if !cached {
					if lookupEnv != nil {
						value.value, value.present = lookupEnv(name)
					}
					envCache[name] = value
				}
				if !value.present {
					err = ErrNotFound
				} else {
					record, err = DecodeEnvironment(value.value, binding.Kinds)
				}
			} else {
				status.Source = "keyring"
				value, cached := storeCache[binding.Reference]
				if !cached {
					if reader == nil {
						value.err = ErrUnavailable
					} else {
						value.record, value.err = reader.Load(ctx, binding.Reference)
					}
					storeCache[binding.Reference] = value
				}
				record, err = value.record, value.err
			}
		case binding.Legacy != (Record{}):
			status.Source = "legacy"
			record = binding.Legacy
		default:
			// There is no credential to validate for an unconfigured consumer.
			r.statuses = append(r.statuses, status)
			continue
		}
		if err == nil {
			err = record.Validate()
			if err == nil && !allows(binding.Kinds, record.Kind) {
				err = ErrInvalidRecord
			}
		}
		if binding.Target == "" || targets[binding.Target] != 1 {
			err = ErrInvalidRecord
		}
		switch {
		case err == nil:
			status.State = "ready"
			r.records[binding.Target] = record
		case errors.Is(err, ErrNotFound):
			status.State = "missing"
		case errors.Is(err, ErrInvalidRecord), errors.Is(err, ErrInvalidReference):
			status.State = "invalid"
		default:
			status.State = "unavailable"
		}
		r.statuses = append(r.statuses, status)
	}
	return r
}
