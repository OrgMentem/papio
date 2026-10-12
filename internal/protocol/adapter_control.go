// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Signed adapter control (ADR-0021 Decision 4). The daemon verifies a signed
// document and holds canonical control; these three job-free frames carry the
// verified, restrictive-only result to one browser session and back. No frame
// here names a host, selector, path, method, text, or threshold of the
// document: only exact packaged adapter revision IDs and safety-domain IDs
// travel, plus the hostnames the extension itself derives from its own
// packaged adapters.
// Control state values of adapter_control_response.state.
const (
	AdapterControlStateNone        = "none"
	AdapterControlStateCurrent     = "current"
	AdapterControlStateUnavailable = "unavailable"
)

// Outcome values of adapter_control_apply_result.outcome.
const (
	AdapterControlOutcomeApplied        = "applied"
	AdapterControlOutcomeMissing        = "missing"
	AdapterControlOutcomeRolledBack     = "rolled_back"
	AdapterControlOutcomeDigestMismatch = "digest_mismatch"
	AdapterControlOutcomeUnavailable    = "unavailable"
)

// Caps shared by the signed document, the response frame, and the apply
// result. One constant per bound, mirrored by extension/src/protocol.ts and
// protocol/browser-v1.schema.json.
const (
	MaxAdapterControlRevisions = 64
	MaxAdapterControlDomains   = 64
	MaxAdapterControlHosts     = 64
)

var (
	// AdapterRevisionIDRE is one exact packaged adapter revision:
	// `<adapter id>@<semver>`.
	AdapterRevisionIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}@[0-9]+\.[0-9]+\.[0-9]+$`)
	// AdapterSafetyDomainIDRE is one daemon-minted safety-domain ID,
	// `<kind>:<opaque>`, matched exactly and never interpreted by the document.
	AdapterSafetyDomainIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}:[a-z0-9._-]{1,253}$`)
	adapterControlHostRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?){0,8}$`)
	adapterControlDigestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// SuspensionsDigest is the canonical digest of one suspension set. The
// extension persists it beside the sequence, so two different suspension sets
// at one sequence are detectable. Both inputs MUST already be sorted and
// unique; validation of the frames and of the signed document enforces that.
func SuspensionsDigest(revisions, domains []string) string {
	hash := sha256.New()
	hash.Write([]byte("papio-suspensions/1\n"))
	for _, revision := range revisions {
		hash.Write([]byte("r " + revision + "\n"))
	}
	for _, domain := range domains {
		hash.Write([]byte("d " + domain + "\n"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ValidateSuspensionSets enforces the caps, ID shapes, sort order, and
// uniqueness every producer and consumer of a suspension set shares.
func ValidateSuspensionSets(what string, revisions, domains []string) error {
	if len(revisions) > MaxAdapterControlRevisions {
		return fmt.Errorf("%s revisions capped at %d", what, MaxAdapterControlRevisions)
	}
	if len(domains) > MaxAdapterControlDomains {
		return fmt.Errorf("%s safety domains capped at %d", what, MaxAdapterControlDomains)
	}
	if err := validateSortedUnique(what+" revision", revisions, AdapterRevisionIDRE); err != nil {
		return err
	}
	return validateSortedUnique(what+" safety domain", domains, AdapterSafetyDomainIDRE)
}

func validateSortedUnique(what string, values []string, pattern *regexp.Regexp) error {
	for i, value := range values {
		if !pattern.MatchString(value) {
			return fmt.Errorf("%s %q is not a valid ID", what, value)
		}
		if i > 0 && values[i-1] >= value {
			return fmt.Errorf("%s list must be sorted and unique", what)
		}
	}
	return nil
}

// AdapterControlRequestPayload asks the daemon for canonical control. It
// carries only what the extension persisted.
type AdapterControlRequestPayload struct {
	RequestID             string `json:"request_id"`
	LastControlSequence   int64  `json:"last_control_sequence"`
	LastSuspensionsDigest string `json:"last_suspensions_digest,omitempty"`
}

func (p *AdapterControlRequestPayload) validate() error {
	if !requestIDRE.MatchString(p.RequestID) {
		return fmt.Errorf("adapter_control_request.request_id must match the request_id charset (8..128 chars)")
	}
	if p.LastControlSequence < 0 || p.LastControlSequence > MaxBrowserInteger {
		return fmt.Errorf("adapter_control_request.last_control_sequence out of range")
	}
	if p.LastSuspensionsDigest != "" && !adapterControlDigestRE.MatchString(p.LastSuspensionsDigest) {
		return fmt.Errorf("adapter_control_request.last_suspensions_digest must be 64 lowercase hex characters")
	}
	if p.LastControlSequence == 0 && p.LastSuspensionsDigest != "" {
		return fmt.Errorf("adapter_control_request.last_suspensions_digest requires a non-zero sequence")
	}
	return nil
}

// AdapterControlResponsePayload is the daemon's canonical verified control for
// one session. State "none" means nothing is published; "unavailable" means
// the daemon holds control it cannot verify and the extension must fail closed.
type AdapterControlResponsePayload struct {
	RequestID              string   `json:"request_id"`
	State                  string   `json:"state"`
	ControlSequence        int64    `json:"control_sequence"`
	SuspensionsDigest      string   `json:"suspensions_digest"`
	SuspendedRevisions     []string `json:"suspended_revisions"`
	SuspendedSafetyDomains []string `json:"suspended_safety_domains"`
}

func (p *AdapterControlResponsePayload) validate() error {
	if !requestIDRE.MatchString(p.RequestID) {
		return fmt.Errorf("adapter_control_response.request_id must match the request_id charset (8..128 chars)")
	}
	if err := enumRequired("adapter_control_response.state", p.State,
		AdapterControlStateNone, AdapterControlStateCurrent, AdapterControlStateUnavailable); err != nil {
		return err
	}
	if p.ControlSequence < 0 || p.ControlSequence > MaxBrowserInteger {
		return fmt.Errorf("adapter_control_response.control_sequence out of range")
	}
	if !adapterControlDigestRE.MatchString(p.SuspensionsDigest) {
		return fmt.Errorf("adapter_control_response.suspensions_digest must be 64 lowercase hex characters")
	}
	if err := ValidateSuspensionSets("adapter_control_response", p.SuspendedRevisions, p.SuspendedSafetyDomains); err != nil {
		return err
	}
	if p.State == AdapterControlStateCurrent {
		if p.ControlSequence < 1 {
			return fmt.Errorf("adapter_control_response.current requires control_sequence >= 1")
		}
	} else if p.ControlSequence != 0 || len(p.SuspendedRevisions) != 0 || len(p.SuspendedSafetyDomains) != 0 {
		return fmt.Errorf("adapter_control_response.%s forbids a sequence or suspensions", p.State)
	}
	return nil
}

// AdapterControlApplyResultPayload reports what one session did with the
// response. SuspendedHosts is derived by the extension from its own packaged
// adapters (never from the document) so the daemon can withhold automation for
// a suspended revision without owning an adapter registry.
type AdapterControlApplyResultPayload struct {
	RequestID       string   `json:"request_id"`
	ControlSequence int64    `json:"control_sequence"`
	Outcome         string   `json:"outcome"`
	SuspendedHosts  []string `json:"suspended_hosts"`
}

func (p *AdapterControlApplyResultPayload) validate() error {
	if !requestIDRE.MatchString(p.RequestID) {
		return fmt.Errorf("adapter_control_apply_result.request_id must match the request_id charset (8..128 chars)")
	}
	if p.ControlSequence < 0 || p.ControlSequence > MaxBrowserInteger {
		return fmt.Errorf("adapter_control_apply_result.control_sequence out of range")
	}
	if err := enumRequired("adapter_control_apply_result.outcome", p.Outcome,
		AdapterControlOutcomeApplied, AdapterControlOutcomeMissing, AdapterControlOutcomeRolledBack,
		AdapterControlOutcomeDigestMismatch, AdapterControlOutcomeUnavailable); err != nil {
		return err
	}
	if len(p.SuspendedHosts) > MaxAdapterControlHosts {
		return fmt.Errorf("adapter_control_apply_result.suspended_hosts capped at %d", MaxAdapterControlHosts)
	}
	if p.Outcome != AdapterControlOutcomeApplied && len(p.SuspendedHosts) != 0 {
		return fmt.Errorf("adapter_control_apply_result.%s forbids suspended_hosts", p.Outcome)
	}
	for i, host := range p.SuspendedHosts {
		if host != strings.ToLower(host) || !adapterControlHostRE.MatchString(host) {
			return fmt.Errorf("adapter_control_apply_result.suspended_hosts entries must be lowercase hostnames")
		}
		if i > 0 && p.SuspendedHosts[i-1] >= host {
			return fmt.Errorf("adapter_control_apply_result.suspended_hosts must be sorted and unique")
		}
	}
	return nil
}
