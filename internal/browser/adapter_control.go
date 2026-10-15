// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"papio/internal/adaptercontrol"
	"papio/internal/protocol"
)

// AdapterControlMinExtensionVersion is the first extension whose strict
// hello_ack parser accepts adapter_control and that speaks the
// adapter_control_* frames. It shipped while the hello_ack feature list was
// full at 32, so it is a capability flag gated by version, the same mechanism
// as SessionRolesMinExtensionVersion: released 0.15.x rejects the whole ack if
// the field is present.
const AdapterControlMinExtensionVersion = "0.16.0"

// AdapterControlSource is the daemon's canonical verified control.
type AdapterControlSource interface {
	Snapshot() adaptercontrol.Snapshot
}

// sessionControl is what one browser session reported it did with the
// daemon's control. It lives on the session, so a fresh hello (reconnect, MV3
// restart) starts unresolved and a promotion keeps it.
type sessionControl struct {
	resolved       bool
	outcome        string
	sequence       int64
	suspendedHosts []string
}

// Refusal reasons, shown in daemon logs and `papio doctor`.
const (
	controlReasonUnapplied = "the browser has not applied the current signed adapter control"
	controlReasonDomain    = "signed adapter control suspends this safety domain"
	controlReasonRevision  = "signed adapter control suspends this adapter revision"
	controlReasonHost      = "signed adapter control suspends the adapter that serves this host"
)

// SetAdapterControl wires the canonical control source. Nil disables the
// feature entirely (every query answers "none" and nothing is suspended).
func (b *Bridge) SetAdapterControl(source AdapterControlSource) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adapterControl = source
}

func (b *Bridge) controlSnapshot() adaptercontrol.Snapshot {
	if b.adapterControl == nil {
		return adaptercontrol.Snapshot{State: adaptercontrol.StateNone, Digest: protocol.SuspensionsDigest(nil, nil)}
	}
	return b.adapterControl.Snapshot()
}

// adapterControlCapable reports whether a session speaks the control frames.
func adapterControlCapable(session *browserSession) bool {
	return session != nil && compareVersion(session.ExtensionVersion, AdapterControlMinExtensionVersion) >= 0
}

// controlSuppression reports why signed control forbids an automatic effect
// for the current holder, or "" when it permits one. It is the single daemon
// gate behind every automatic offer and effect permit:
//
//   - Once the daemon holds control (or cannot verify what it holds), a holder
//     that has not applied that exact sequence gets no automatic effect, which
//     covers extensions too old to speak the frames at all.
//   - An exact suspended safety domain, adapter revision, or a host served by
//     a suspended revision the session reported suspends the effect.
//
// The caller holds b.mu. domain, revision, and hosts may each be empty.
func (b *Bridge) controlSuppression(domain, revision string, hosts []string) string {
	if b == nil || b.adapterControl == nil {
		return ""
	}
	snapshot := b.adapterControl.Snapshot()
	holder := b.arbitration.holderSession()
	if snapshot.Mandatory() {
		if holder == nil {
			return ""
		}
		if snapshot.State == adaptercontrol.StateUnavailable || !adapterControlCapable(holder) ||
			!holder.control.resolved || holder.control.outcome != protocol.AdapterControlOutcomeApplied ||
			holder.control.sequence < snapshot.Sequence {
			return controlReasonUnapplied
		}
	}
	if domain != "" && snapshot.DomainSuspended(domain) {
		return controlReasonDomain
	}
	if revision != "" && snapshot.RevisionSuspended(revision) {
		return controlReasonRevision
	}
	if holder == nil || len(holder.control.suspendedHosts) == 0 {
		return ""
	}
	if domainHost := safetyDomainHost(domain); domainHost != "" {
		hosts = append(hosts[:len(hosts):len(hosts)], domainHost)
	}
	for _, host := range hosts {
		if browserOfferHostMatches(holder.control.suspendedHosts, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))) {
			return controlReasonHost
		}
	}
	return ""
}

// safetyDomainHost extracts the host from the host-keyed safety domains
// (`institution:`, `publisher:`, `oa:`, `delivery:`, `pdf_grab:`). Route
// families carry no host.
func safetyDomainHost(domain string) string {
	kind, rest, ok := strings.Cut(domain, ":")
	if !ok {
		return ""
	}
	switch kind {
	case "institution", "publisher", "oa", "delivery", "pdf_grab":
		return rest
	}
	return ""
}

// adapterControlRequest answers adapter_control_request with the daemon's canonical
// verified control. It never errors for an ordinary condition: an unverifiable
// state is a structured "unavailable" the extension fails closed on.
func (b *Bridge) adapterControlRequest(sessionID string, p *protocol.AdapterControlRequestPayload) ([]json.RawMessage, error) {
	snapshot := b.controlSnapshot()
	payload := protocol.AdapterControlResponsePayload{
		RequestID:              p.RequestID,
		State:                  string(snapshot.State),
		ControlSequence:        snapshot.Sequence,
		SuspensionsDigest:      snapshot.Digest,
		SuspendedRevisions:     append([]string{}, snapshot.Revisions...),
		SuspendedSafetyDomains: append([]string{}, snapshot.Domains...),
	}
	if payload.SuspensionsDigest == "" {
		payload.SuspensionsDigest = protocol.SuspensionsDigest(nil, nil)
	}
	frame, err := b.frame(protocol.MsgAdapterControlResponse, "", payload)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// adapterControlApplied records what a session did. It sends no reply.
func (b *Bridge) adapterControlApplied(session *browserSession, p *protocol.AdapterControlApplyResultPayload) {
	if session == nil {
		return
	}
	session.control = sessionControl{
		resolved:       true,
		outcome:        p.Outcome,
		sequence:       p.ControlSequence,
		suspendedHosts: append([]string(nil), p.SuspendedHosts...),
	}
}

// AdapterControlSession is one browser session's control state for doctor.
type AdapterControlSession struct {
	ID               string
	ExtensionVersion string
	Holder           bool
	Capable          bool
	Resolved         bool
	Outcome          string
	Sequence         int64
	SuspendedHosts   []string
	LastSyncAt       time.Time
}

// AdapterControlSessions lists every connected session with what it reported,
// holder first.
func (b *Bridge) AdapterControlSessions() []AdapterControlSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []AdapterControlSession
	add := func(session *browserSession, holder bool) {
		out = append(out, AdapterControlSession{
			ID: session.ID, ExtensionVersion: session.ExtensionVersion, Holder: holder,
			Capable: adapterControlCapable(session), Resolved: session.control.resolved,
			Outcome: session.control.outcome, Sequence: session.control.sequence,
			SuspendedHosts: append([]string(nil), session.control.suspendedHosts...),
			LastSyncAt:     session.LastSyncAt,
		})
	}
	if holder := b.arbitration.holderSession(); holder != nil {
		add(holder, true)
	}
	ids := make([]string, 0, len(b.arbitration.pending))
	for id := range b.arbitration.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add(b.arbitration.pending[id], false)
	}
	return out
}

// controlRefusalDetail renders a refusal for a structured result detail.
func controlRefusalDetail(reason string) string {
	return fmt.Sprintf("automatic effects are paused: %s", reason)
}
