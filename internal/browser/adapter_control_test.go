// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"papio/internal/config"
	"strings"
	"testing"
	"time"

	"papio/internal/adaptercontrol"
	"papio/internal/adaptercontrol/controltest"
	"papio/internal/job"
	"papio/internal/protocol"
)

func controlService(t *testing.T) *adaptercontrol.Service {
	t.Helper()
	public, _ := controltest.PlaceholderKey()
	return adaptercontrol.Open(t.TempDir(), public)
}

func publishControl(t *testing.T, service *adaptercontrol.Service, sequence int64, revisions, domains []string) {
	t.Helper()
	_, private := controltest.PlaceholderKey()
	envelope, err := adaptercontrol.Sign(adaptercontrol.Document{
		Sequence: sequence, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "papio-control-test",
		SuspendedRevisions: revisions, SuspendedSafetyDomains: domains,
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(envelope); err != nil {
		t.Fatal(err)
	}
}

// controlHolder installs a holder at extensionVersion that reported control.
func controlHolder(t *testing.T, b *Bridge, extensionVersion string, control sessionControl) {
	t.Helper()
	b.arbitration.setHolderForTest(&browserSession{
		ID: "control-test-holder", ExtensionVersion: extensionVersion,
		Features:   []string{providerDriveEpochV1Feature, effectPermitFeature},
		LastSyncAt: b.now(), control: control,
	}, 0)
}

func appliedControl(sequence int64, hosts ...string) sessionControl {
	return sessionControl{resolved: true, outcome: protocol.AdapterControlOutcomeApplied, sequence: sequence, suspendedHosts: hosts}
}

func TestHelloAckAdvertisesAdapterControlOnlyToExtensionsThatParseIt(t *testing.T) {
	ackFor := func(version string, wired bool) *protocol.HelloAckPayload {
		b, _, _, _ := newBridge(t)
		if wired {
			b.SetAdapterControl(controlService(t))
		}
		msgs, _ := runSync(t, b, helloWithFeatures(t, version))
		ack := firstOfType(msgs, protocol.MsgHelloAck)
		if ack == nil {
			t.Fatalf("no hello_ack for %s", version)
		}
		return ack.Payload.(*protocol.HelloAckPayload)
	}
	if !ackFor("0.16.0", true).AdapterControl {
		t.Fatal("0.16.0 was not told the daemon answers adapter control")
	}
	// Released 0.15.x rejects the whole ack when it carries the field, so the
	// field must never reach it. The feature list is unchanged and still capped.
	if ackFor("0.15.0", true).AdapterControl {
		t.Fatal("adapter_control was sent to 0.15.0, whose strict hello_ack parser rejects it")
	}
	if ackFor("0.16.0", false).AdapterControl {
		t.Fatal("a bridge without adapter control advertised it")
	}
	if got := len(ackFor("0.16.0", true).Features); got != 32 {
		t.Fatalf("feature list length = %d, want exactly 32 (the fail-closed cap)", got)
	}
}

func TestAdapterControlRequestReturnsCanonicalVerifiedControl(t *testing.T) {
	b, _, _, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	runSync(t, b, helloWithFeatures(t, "0.16.0"))
	ask := func() *protocol.AdapterControlResponsePayload {
		msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAdapterControlRequest, "", map[string]any{
			"request_id": "control-request-0001", "last_control_sequence": 0,
		}))
		response := firstOfType(msgs, protocol.MsgAdapterControlResponse)
		if response == nil {
			t.Fatalf("no adapter_control_response in %v", msgs)
		}
		return response.Payload.(*protocol.AdapterControlResponsePayload)
	}
	none := ask()
	if none.State != "none" || none.ControlSequence != 0 || none.SuspensionsDigest != protocol.SuspensionsDigest(nil, nil) ||
		none.SuspendedRevisions == nil || len(none.SuspendedRevisions) != 0 {
		t.Fatalf("response with nothing published = %+v", none)
	}
	publishControl(t, service, 4, []string{"sciencedirect@0.3.0"}, []string{"publisher:www.sciencedirect.com"})
	current := ask()
	if current.State != "current" || current.ControlSequence != 4 ||
		current.SuspensionsDigest != "d9ada45fb9029f57dacec82802fed8b4472457d678544901e1f283057552b028" ||
		len(current.SuspendedRevisions) != 1 || current.SuspendedRevisions[0] != "sciencedirect@0.3.0" ||
		len(current.SuspendedSafetyDomains) != 1 {
		t.Fatalf("response = %+v", current)
	}
	publishControl(t, service, 5, nil, nil)
	if lifted := ask(); lifted.ControlSequence != 5 || len(lifted.SuspendedRevisions) != 0 {
		t.Fatalf("a higher sequence did not lift the suspension: %+v", lifted)
	}
}

func TestAdapterControlRequestOnAnUnverifiableStoreAnswersUnavailable(t *testing.T) {
	b, _, _, _ := newBridge(t)
	b.SetAdapterControl(unavailableControl{})
	runSync(t, b, helloWithFeatures(t, "0.16.0"))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAdapterControlRequest, "", map[string]any{
		"request_id": "control-request-0002", "last_control_sequence": 3,
		"last_suspensions_digest": protocol.SuspensionsDigest(nil, nil),
	}))
	response := firstOfType(msgs, protocol.MsgAdapterControlResponse)
	if response == nil || response.Payload.(*protocol.AdapterControlResponsePayload).State != "unavailable" {
		t.Fatalf("response = %+v, want unavailable", response)
	}
}

type unavailableControl struct{}

func (unavailableControl) Snapshot() adaptercontrol.Snapshot {
	return adaptercontrol.Snapshot{State: adaptercontrol.StateUnavailable, Digest: protocol.SuspensionsDigest(nil, nil), Reason: "test"}
}

func TestApplyResultIsRecordedPerSessionAndNeverFatal(t *testing.T) {
	b, _, _, _ := newBridge(t)
	b.SetAdapterControl(controlService(t))
	runSync(t, b, helloWithFeatures(t, "0.16.0"))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAdapterControlApplyResult, "", map[string]any{
		"request_id": "control-request-0003", "control_sequence": 4, "outcome": "applied",
		"suspended_hosts": []string{"sciencedirect.com"},
	}))
	if len(msgs) != 0 {
		t.Fatalf("apply_result drew a reply: %v", msgs)
	}
	sessions := b.AdapterControlSessions()
	if len(sessions) != 1 || !sessions[0].Resolved || sessions[0].Outcome != "applied" || sessions[0].Sequence != 4 ||
		len(sessions[0].SuspendedHosts) != 1 || !sessions[0].Capable {
		t.Fatalf("sessions = %+v", sessions)
	}
	// A new hello is a new session: its control starts unresolved.
	runSync(t, b, helloWithFeatures(t, "0.16.0"))
	if sessions := b.AdapterControlSessions(); len(sessions) != 1 || sessions[0].Resolved {
		t.Fatalf("a reconnect inherited control state: %+v", sessions)
	}
}

func TestControlSuppressionBlocksOnlyWhatTheDocumentNames(t *testing.T) {
	b, _, _, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)

	// Nothing published: nothing is ever withheld, for any extension version.
	controlHolder(t, b, "0.15.0", sessionControl{})
	if reason := b.controlSuppression("publisher:www.sciencedirect.com", "sciencedirect@0.3.0", []string{"www.sciencedirect.com"}); reason != "" {
		t.Fatalf("with no control published, suppression = %q", reason)
	}

	publishControl(t, service, 3, []string{"sciencedirect@0.3.0"}, []string{"institution:library.example.edu"})
	controlHolder(t, b, "0.16.0", appliedControl(3, "sciencedirect.com"))
	cases := []struct {
		name, domain, revision string
		hosts                  []string
		want                   string
	}{
		{"the named revision", "", "sciencedirect@0.3.0", nil, controlReasonRevision},
		{"another revision of the same adapter", "", "sciencedirect@0.3.1", nil, ""},
		{"a sibling adapter", "", "wiley@0.1.0", nil, ""},
		{"the named domain", "institution:library.example.edu", "", nil, controlReasonDomain},
		{"another domain", "institution:other.example.edu", "", nil, ""},
		{"a host the suspended revision serves", "", "", []string{"www.sciencedirect.com"}, controlReasonHost},
		{"a host-keyed domain on that host", "publisher:www.sciencedirect.com", "", nil, controlReasonHost},
		{"a sibling provider host", "publisher:onlinelibrary.wiley.com", "", []string{"onlinelibrary.wiley.com"}, ""},
	}
	for _, tc := range cases {
		if got := b.controlSuppression(tc.domain, tc.revision, tc.hosts); got != tc.want {
			t.Errorf("%s: suppression = %q, want %q", tc.name, got, tc.want)
		}
	}

	// Once control is published, a holder that has not applied exactly it gets
	// nothing automatic: too old, unresolved, behind, refusing, or an
	// unverifiable daemon store.
	for name, holder := range map[string]struct {
		version string
		control sessionControl
	}{
		"an extension too old to speak control": {"0.15.0", sessionControl{}},
		"unresolved":                            {"0.16.0", sessionControl{}},
		"behind":                                {"0.16.0", appliedControl(2)},
		"rolled back":                           {"0.16.0", sessionControl{resolved: true, outcome: "rolled_back", sequence: 9}},
	} {
		controlHolder(t, b, holder.version, holder.control)
		if got := b.controlSuppression("", "", nil); got != controlReasonUnapplied {
			t.Errorf("%s: suppression = %q, want %q", name, got, controlReasonUnapplied)
		}
	}
	b.SetAdapterControl(unavailableControl{})
	controlHolder(t, b, "0.16.0", appliedControl(3))
	if got := b.controlSuppression("", "", nil); got != controlReasonUnapplied {
		t.Errorf("unavailable store: suppression = %q", got)
	}
}

func TestSuspendedSafetyDomainStopsTheOfferAndALiftRestoresIt(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	ctx := context.Background()
	id := park(t, jobs, "wr_control_offer", handoffWork())
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	domain := actionSafetyDomain(cfg, *row, actions[0])
	if domain == "" {
		t.Fatal("test job has no safety domain")
	}
	offered := func() bool {
		latched, err := b.browserOfferLatched(ctx, *row, actions[0])
		if err != nil {
			t.Fatal(err)
		}
		return !latched
	}
	controlHolder(t, b, "0.16.0", appliedControl(1))
	if !offered() {
		t.Fatal("the job was withheld with no control published")
	}
	publishControl(t, service, 1, nil, []string{domain})
	if offered() {
		t.Fatal("a suspended safety domain was still offered")
	}
	// A higher sequence lifts the suspension, but the holder must prove it
	// applied that sequence before anything automatic is offered again.
	publishControl(t, service, 2, nil, nil)
	if offered() {
		t.Fatal("offers resumed before the browser applied the lifting sequence")
	}
	controlHolder(t, b, "0.16.0", appliedControl(2))
	if !offered() {
		t.Fatal("a higher sequence did not lift the suspension")
	}
}

func TestSuspendedSafetyDomainRefusesTheEffectPermit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	ctx := context.Background()
	id := park(t, jobs, "wr_control_permit", handoffWork())
	attempt, domain := "control-drive-attempt-1", "institution:example.edu"
	effectPermitOffer(t, jobs, id, attempt, domain)
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}

	publishControl(t, service, 1, nil, []string{domain})
	controlHolder(t, b, "0.16.0", appliedControl(1))
	frames, err := b.providerDriveEpochStart(ctx, id, start)
	if err != nil || permitOutcome(t, frames) != "unsupported" {
		t.Fatalf("suspended domain: outcome err=%v frames=%v, want unsupported", err, frames)
	}
	if permit, err := jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil || permit != nil {
		t.Fatalf("a permit exists for a suspended domain: %+v, %v", permit, err)
	}

	publishControl(t, service, 2, nil, nil)
	controlHolder(t, b, "0.16.0", appliedControl(2))
	frames, err = b.providerDriveEpochStart(ctx, id, start)
	if err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("after the lift: err=%v frames=%v, want started", err, frames)
	}
}

func TestSuspendedRevisionRefusesTheTermsPermitOnlyForThatRevision(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	ctx := context.Background()
	id := park(t, jobs, "wr_control_terms", handoffWork())
	effectPermitOffer(t, jobs, id, "control-terms-attempt", "publisher:www.jstor.org")
	request := func(version string) *protocol.TermsEffectStartRequestPayload {
		return &protocol.TermsEffectStartRequestPayload{
			RequestID: "request-terms-" + strings.ReplaceAll(version, ".", "-"), AdapterID: "jstor", AdapterVersion: version,
			AuthorityDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}
	}
	publishControl(t, service, 1, []string{"jstor@1.0.0"}, nil)
	controlHolder(t, b, "0.16.0", appliedControl(1, "jstor.org"))
	frames, err := b.termsEffectStart(ctx, id, request("1.0.0"))
	if err != nil || permitOutcome(t, frames) != "unsupported" {
		t.Fatalf("suspended revision: err=%v frames=%v, want unsupported", err, frames)
	}
	// The host of the suspended revision is paused too, whatever the version.
	frames, err = b.termsEffectStart(ctx, id, request("1.0.1"))
	if err != nil || permitOutcome(t, frames) != "unsupported" {
		t.Fatalf("host of the suspended revision: err=%v frames=%v, want unsupported", err, frames)
	}
	// With no host reported, a different revision of the same adapter is unaffected.
	controlHolder(t, b, "0.16.0", appliedControl(1))
	frames, err = b.termsEffectStart(ctx, id, request("1.0.1"))
	if err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("unrelated revision: err=%v frames=%v, want started", err, frames)
	}
}

// An explicit `papio actions open` is the operator's own request. Signed
// control withholds automatic authority only, so the open must still reach the
// browser: as an assisted offer with no drive epoch and then a focus frame,
// never silently dropped.
func TestExplicitOpenOfASuspendedJobIsAssistedNotDropped(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	ctx := context.Background()
	id := park(t, jobs, "wr_control_open", handoffWork())
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	domain := actionSafetyDomain(cfg, *row, actions[0])
	publishControl(t, service, 1, nil, []string{domain})

	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.16.0"))
	if got := countType(msgs, protocol.MsgJobOffer); got != 0 {
		t.Fatalf("a suspended domain was offered automatically: %d offers", got)
	}
	b.mu.Lock()
	b.arbitration.holderSession().control = appliedControl(1)
	b.mu.Unlock()
	if got := countType(msgsOf(t, runSyncAs, b, sessA), protocol.MsgJobOffer); got != 0 {
		t.Fatalf("a suspended domain was offered automatically on a later poll: %d offers", got)
	}

	queued, live, err := b.FocusHandoffs(ctx, []string{id})
	if err != nil || !live || queued != 1 {
		t.Fatalf("explicit open = queued %d live %t err %v; want 1,true", queued, live, err)
	}
	msgs, _ = runSyncAs(t, b, sessA)
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatalf("the explicit open of a suspended job produced no offer: %v", msgs)
	}
	payload := offer.Payload.(*protocol.JobOfferPayload)
	if payload.AccessMode != config.ModeAssisted || payload.DriveAttemptID != "" || payload.DriveOrdinal != nil {
		t.Fatalf("offer = %+v; want assisted with no drive epoch", payload)
	}
	if got := countType(msgs, protocol.MsgHandoffFocus); got != 1 {
		t.Fatalf("handoff_focus frames = %d, want 1", got)
	}
}

func msgsOf(t *testing.T, run func(*testing.T, *Bridge, string, ...json.RawMessage) ([]*protocol.BrowserMessage, []json.RawMessage), b *Bridge, session string) []*protocol.BrowserMessage {
	t.Helper()
	msgs, _ := run(t, b, session)
	return msgs
}

// An institutional claim already bound when control arrives must not obtain a
// navigation permit: the check keys on the public institution:<host> domain,
// not the candidate's keyed MAC.
func TestSuspendedInstitutionDomainRefusesTheRoutePermit(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	service := controlService(t)
	b.SetAdapterControl(service)
	runSync(t, b, materializationHello(t))
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "control-route-permit", handoffWork(), "")
	row, err := jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	domain := actionSafetyDomain(cfg, *row, actions[0])
	if !strings.HasPrefix(domain, "institution:") {
		t.Fatalf("public domain = %q, want institution:<host>", domain)
	}
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-c", AuthenticationClaimID: "auth-c",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-control-route", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "0123456789abcdef0123456789abcdef", // a keyed MAC, as in production
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.arbitration.generation(),
		JobAttemptRevision: candidate.JobAttemptRevision, InstitutionProfileRevision: candidate.InstitutionProfileRevision,
		RouteRevision: candidate.RouteRevision, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.arbitration.generation(), profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	route := func(request string) *protocol.InstitutionalRouteResponsePayload {
		frames, err := b.institutionalRoute(ctx, jobID, &protocol.InstitutionalRouteRequestPayload{
			RequestID: request, ClaimID: claim.ID, BindingID: claim.BindingID,
		})
		if err != nil || len(frames) != 1 {
			t.Fatalf("route frames=%v err=%v", frames, err)
		}
		msg, err := protocol.DecodeBrowserMessage(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		return msg.Payload.(*protocol.InstitutionalRouteResponsePayload)
	}
	publishControl(t, service, 1, nil, []string{domain})
	b.mu.Lock()
	holder := b.arbitration.holderSession()
	holder.ExtensionVersion, holder.control = "0.16.0", appliedControl(1)
	holder.Features = []string{institutionalMaterializationFeature, effectPermitFeature}
	b.mu.Unlock()
	if got := route("req_route_suspended"); got.Outcome != "not_eligible" || got.URL != "" {
		t.Fatalf("suspended institution route = %+v, want not_eligible with no URL", got)
	}
}
