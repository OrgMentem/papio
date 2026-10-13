// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"

	"papio/internal/protocol"
)

// A suspension published while a generic-drive permit is held must stop further
// work under that permit, not only new offers.
func TestHeldGenericDrivePermitRefusedOnceDomainSuspended(t *testing.T) {
	const domain = "publisher:example.com"
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, helloWithFeatures(t, "1.2.3", providerDriveEpochV1Feature, effectPermitFeature))
	id := park(t, jobs, "generic-control", handoffWork())
	effectPermitOffer(t, jobs, id, "ctl-drive", domain)
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgProviderDriveEpochStartRequest, id, protocol.ProviderDriveEpochStartRequestPayload{RequestID: "ctl-start", DriveAttemptID: "ctl-drive", Strategy: "generic", Revision: "1"}))
	if start := firstOfType(msgs, protocol.MsgProviderDriveEpochStartResult); start == nil || start.Payload.(*protocol.ProviderDriveEpochStartResultPayload).Outcome != "started" {
		t.Fatalf("start=%+v", msgs)
	}
	service := controlService(t)
	b.SetAdapterControl(service)
	ctx := context.Background()
	authority := func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		session := b.arbitration.holderSession()
		_, ok := b.currentGenericDriveAuthority(ctx, session.ID, id, "ctl-drive", 0, "generic", "1")
		return ok
	}
	if !authority() {
		t.Fatal("held permit must be usable before any suspension")
	}
	publishControl(t, service, 1, nil, []string{domain})
	b.mu.Lock()
	b.arbitration.holderSession().control = appliedControl(1)
	b.mu.Unlock()
	if authority() {
		t.Fatal("held permit still usable after its safety domain was suspended")
	}
}
