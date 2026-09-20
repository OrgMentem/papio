// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/acquisitionagent"
	"papio/internal/job"
	"papio/internal/protocol"
)

type agentBackendFunc func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error)

func (f agentBackendFunc) Decide(ctx context.Context, o acquisitionagent.Observation) (acquisitionagent.Decision, error) {
	return f(ctx, o)
}

func newAgentAttempt(t *testing.T, backend acquisitionagent.Backend) (*Bridge, *job.Store, string, protocol.AgentDecideRequestV1Payload) {
	t.Helper()
	b, jobs, _, _ := newBridge(t)
	b.SetAcquisitionBackend(backend)
	t.Cleanup(b.CloseAcquisitionBackend)
	effectPermitHolder(t, b)
	b.arbitration.holderSession().Features = append(b.arbitration.holderSession().Features, agentFallbackFeature)
	id := park(t, jobs, "agent-attempt", handoffWork())
	effectPermitOffer(t, jobs, id, "agent-drive", "agent-domain")
	request := protocol.AgentDecideRequestV1Payload{
		RequestID: "agent-request-1", DriveAttemptID: "agent-drive", Ordinal: 0, Strategy: "generic", Revision: "1",
		Observation: acquisitionagent.Observation{Revision: strings.Repeat("a", 64), DOI: handoffWork().DOI, Title: "Article title",
			Controls: []acquisitionagent.Control{{ID: "c1", Role: "button", Label: "Read PDF"}}},
	}
	frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: request.DriveAttemptID, Ordinal: request.Ordinal, Strategy: request.Strategy, Revision: request.Revision})
	if err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start: %v %s", err, frames)
	}
	return b, jobs, id, request
}

func submitAgent(t *testing.T, b *Bridge, id string, p protocol.AgentDecideRequestV1Payload) []json.RawMessage {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	frames, err := b.agentDecide(context.Background(), "permit-test-holder", id, &p)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func decodeAgent(t *testing.T, frames []json.RawMessage) protocol.AgentDecideResultV1Payload {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("expected one agent reply, got %d", len(frames))
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	p, ok := msg.Payload.(*protocol.AgentDecideResultV1Payload)
	if !ok {
		t.Fatalf("unexpected %T", msg.Payload)
	}
	return *p
}

func awaitAgent(t *testing.T, b *Bridge) protocol.AgentDecideResultV1Payload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		frames, err := b.drainAgentDecisions(context.Background(), "permit-test-holder")
		b.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) > 0 {
			return decodeAgent(t, frames)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("agent reply did not arrive")
	return protocol.AgentDecideResultV1Payload{}
}

// Use the wire entry point for hello and start: Sync's trailing poll can emit
// recovery traffic before the browser has even observed the provider page.
func startAgentViaSync(t *testing.T, backend acquisitionagent.Backend) (*Bridge, *job.Store, string, protocol.AgentDecideRequestV1Payload, []*protocol.BrowserMessage) {
	t.Helper()
	b, jobs, _, _ := newBridge(t)
	b.SetAcquisitionBackend(backend)
	t.Cleanup(b.CloseAcquisitionBackend)
	runSync(t, b, helloWithFeatures(t, "1.2.3", providerDriveEpochV1Feature, effectPermitFeature, agentFallbackFeature))
	if b.arbitration.generation() <= 0 {
		t.Fatal("hello did not allocate a durable holder generation")
	}
	id := park(t, jobs, "agent-sync-attempt", handoffWork())
	effectPermitOffer(t, jobs, id, "agent-sync-drive", "agent-domain")
	request := protocol.AgentDecideRequestV1Payload{
		RequestID: "agent-sync-request", DriveAttemptID: "agent-sync-drive", Ordinal: 0, Strategy: "generic", Revision: "1",
		Observation: acquisitionagent.Observation{Revision: strings.Repeat("a", 64), DOI: handoffWork().DOI,
			Controls: []acquisitionagent.Control{{ID: "c1", Role: "button", Label: "Read PDF"}}},
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgProviderDriveEpochStartRequest, id, protocol.ProviderDriveEpochStartRequestPayload{
		RequestID: "agent-sync-start", DriveAttemptID: request.DriveAttemptID, Ordinal: request.Ordinal, Strategy: request.Strategy, Revision: request.Revision,
	}))
	started := firstOfType(msgs, protocol.MsgProviderDriveEpochStartResult)
	if started == nil || started.Payload.(*protocol.ProviderDriveEpochStartResultPayload).Outcome != "started" {
		t.Fatalf("start did not succeed: %+v", msgs)
	}
	return b, jobs, id, request, msgs
}

func noDownloadReconcileFrame(t *testing.T, msg *protocol.BrowserMessage) json.RawMessage {
	t.Helper()
	p := msg.Payload.(*protocol.EffectPermitReconcileRequestPayload)
	return inFrame(t, protocol.MsgEffectPermitReconcileResponse, msg.JobID, protocol.EffectPermitReconcileResponsePayload{
		RequestID: p.RequestID, PermitID: p.PermitID, Outcome: "recorded",
		Dispatched: false, DownloadPresent: false, Acknowledged: false, TabPresent: false,
	})
}

func TestAgentDecisionSyncPreservesLivePermitBeforeDownload(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b, jobs, id, request, msgs := startAgentViaSync(t, agentBackendFunc(func(ctx context.Context, _ acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return acquisitionagent.Decision{Choice: "c1"}, nil
		case <-ctx.Done():
			return acquisitionagent.Decision{}, ctx.Err()
		}
	}))
	// Reproduce the live browser faithfully: if start solicits recovery, reply
	// that no download exists yet before submitting the first observation.
	if reconcile := firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest); reconcile != nil {
		runSync(t, b, noDownloadReconcileFrame(t, reconcile))
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgAgentDecideRequestV1, id, request))
	if result := firstOfType(msgs, protocol.MsgAgentDecideResultV1); result != nil {
		t.Fatalf("first observation refused before inference: %+v", result.Payload)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend did not start")
	}
	// The same no-download window lasts throughout observation/inference,
	// not only the start response. Ordinary polls must preserve the lease.
	for range 3 {
		msgs, _ = runSync(t, b)
		if reconcile := firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest); reconcile != nil {
			runSync(t, b, noDownloadReconcileFrame(t, reconcile))
			t.Fatal("live inference solicited recovery before a download existed")
		}
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, _ = runSync(t, b)
		if msg := firstOfType(msgs, protocol.MsgAgentDecideResultV1); msg != nil {
			result := msg.Payload.(*protocol.AgentDecideResultV1Payload)
			if result.Outcome != "decision" || result.Choice != "c1" || result.RequestID != request.RequestID || result.ObservationRevision != request.Observation.Revision {
				t.Fatalf("decision=%+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("decision did not arrive through Sync")
		}
		time.Sleep(5 * time.Millisecond)
	}
	permit, err := jobs.LiveEffectPermit(context.Background())
	if err != nil || permit == nil || permit.Status != job.EffectPermitHeld || calls.Load() != 1 {
		t.Fatalf("permit=%+v err=%v calls=%d", permit, err, calls.Load())
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("decision changed handoff state: row=%+v err=%v", row, err)
	}
}

func TestAgentDecisionSyncReconcilesUncertainPermit(t *testing.T) {
	for _, condition := range []string{"unknown", "expired", "missing_lease", "worker_restart", "daemon_restart", "generation_unavailable"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int32
			backend := agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
				calls.Add(1)
				return acquisitionagent.Decision{Choice: "c1"}, nil
			})
			b, jobs, id, request, _ := startAgentViaSync(t, backend)
			permit, err := jobs.LiveEffectPermit(context.Background())
			if err != nil || permit == nil {
				t.Fatalf("permit=%+v err=%v", permit, err)
			}
			var msgs []*protocol.BrowserMessage
			switch condition {
			case "unknown":
				_, err = jobs.S.DB().Exec(`UPDATE effect_permits SET status='unknown_completion' WHERE id=?`, permit.ID)
			case "expired":
				_, err = jobs.S.DB().Exec(`UPDATE effect_permits SET lease_until=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), permit.ID)
			case "missing_lease":
				_, err = jobs.S.DB().Exec(`UPDATE effect_permits SET lease_until=NULL WHERE id=?`, permit.ID)
			case "worker_restart":
				_, err = b.Sync(context.Background(), testSessionID, true, nil)
				if err == nil {
					msgs, _ = runSync(t, b, helloWithFeatures(t, "1.2.3", providerDriveEpochV1Feature, effectPermitFeature, agentFallbackFeature))
				}
			case "daemon_restart":
				b.CloseAcquisitionBackend()
				b = NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, b.cfg, b.Version)
				b.SetAcquisitionBackend(backend)
				t.Cleanup(b.CloseAcquisitionBackend)
				msgs, _ = runSync(t, b, helloWithFeatures(t, "1.2.3", providerDriveEpochV1Feature, effectPermitFeature, agentFallbackFeature))
			case "generation_unavailable":
				b.materializationGenerationUnavailable = true
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(condition, "restart") && b.arbitration.generation() <= permit.BrowserHolderGeneration {
				t.Fatal("restart reused the permit's holder generation")
			}
			if msgs == nil {
				msgs, _ = runSync(t, b)
			}
			reconcile := firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest)
			if reconcile == nil || reconcile.Payload.(*protocol.EffectPermitReconcileRequestPayload).PermitID != permit.ID {
				t.Fatalf("uncertain permit did not solicit exact recovery: %+v", msgs)
			}
			runSync(t, b, noDownloadReconcileFrame(t, reconcile))
			got, err := jobs.GetEffectPermit(context.Background(), permit.ID)
			if err != nil || got == nil || got.Status != job.EffectPermitUnknownCompletion {
				t.Fatalf("no-download recovery permit=%+v err=%v", got, err)
			}
			msgs, _ = runSync(t, b, inFrame(t, protocol.MsgAgentDecideRequestV1, id, request))
			result := firstOfType(msgs, protocol.MsgAgentDecideResultV1)
			if result == nil || result.Payload.(*protocol.AgentDecideResultV1Payload).Outcome != "stale" || calls.Load() != 0 {
				t.Fatalf("uncertain permit admitted inference: result=%+v calls=%d", result, calls.Load())
			}
		})
	}
}

func TestAgentDecisionDoesNotBlockBridgeAndReservesBeforeCalling(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b, jobs, id, request := newAgentAttempt(t, agentBackendFunc(func(ctx context.Context, o acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return acquisitionagent.Decision{Choice: "c1", InputTokens: 10, OutputTokens: 2}, nil
		case <-ctx.Done():
			return acquisitionagent.Decision{}, ctx.Err()
		}
	}))
	if got := submitAgent(t, b, id, request); len(got) != 0 {
		t.Fatalf("premature reply: %s", got)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend did not start")
	}
	// A real Sync remains responsive while the network call is blocked.
	poll := make(chan error, 1)
	go func() { _, err := b.Sync(context.Background(), "permit-test-holder", false, nil); poll <- err }()
	select {
	case err := <-poll:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("inference held the bridge lock")
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	reserved := 0
	for _, e := range events {
		if e["kind"] == "browser.agent_decision_requested" {
			reserved++
			raw, _ := json.Marshal(e)
			if strings.Contains(string(raw), "Article title") || strings.Contains(string(raw), "Read PDF") {
				t.Fatal("projection leaked into receipt")
			}
		}
	}
	if reserved != 1 {
		t.Fatalf("budget reservations=%d", reserved)
	}
	if got := submitAgent(t, b, id, request); len(got) != 0 {
		t.Fatalf("duplicate did not coalesce: %s", got)
	}
	concurrent := request
	concurrent.RequestID = "agent-concurrent"
	if got := decodeAgent(t, submitAgent(t, b, id, concurrent)); got.Outcome != "stale" {
		t.Fatalf("concurrent=%+v", got)
	}
	close(release)
	result := awaitAgent(t, b)
	if result.Outcome != "decision" || result.Choice != "c1" || result.ObservationRevision != request.Observation.Revision {
		t.Fatalf("result=%+v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if got := decodeAgent(t, submitAgent(t, b, id, request)); got.Outcome != "stale" {
		t.Fatalf("consumed replay=%+v", got)
	}
	row, _ := jobs.Get(context.Background(), id)
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("model decision changed job to %s", row.State)
	}
}

func TestAgentDecisionCancellationRevokesPendingAnswer(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	b, jobs, id, request := newAgentAttempt(t, agentBackendFunc(func(ctx context.Context, _ acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		// A misbehaving backend returning an answer after cancellation still
		// cannot revive the attempt.
		return acquisitionagent.Decision{Choice: "c1"}, nil
	}))
	submitAgent(t, b, id, request)
	<-entered
	if err := jobs.Cancel(context.Background(), id, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("job cancellation did not cancel inference")
	}
	if result := awaitAgent(t, b); result.Outcome != "stale" || result.Choice != "" {
		t.Fatalf("cancelled result=%+v", result)
	}
}

func TestAgentDecisionRechecksAuthorityAtDelivery(t *testing.T) {
	for _, change := range []string{"holder", "permit", "work", "deadline", "backend_closed"} {
		t.Run(change, func(t *testing.T) {
			b, jobs, id, request := newAgentAttempt(t, agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
				return acquisitionagent.Decision{Choice: "c1"}, nil
			}))
			submitAgent(t, b, id, request)
			deadline := time.Now().Add(5 * time.Second)
			for {
				b.mu.Lock()
				ready := b.agentDecisions[id] != nil && b.agentDecisions[id].result != nil
				b.mu.Unlock()
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no completed decision")
				}
				time.Sleep(5 * time.Millisecond)
			}
			b.mu.Lock()
			switch change {
			case "deadline":
				b.agentDecisions[id].deadline = time.Now().Add(-time.Second)
			case "holder":
				b.arbitration.setHolderForTest(&browserSession{ID: "replacement", ExtensionVersion: "0.15.0", Features: []string{agentFallbackFeature, effectPermitFeature}, LastSyncAt: b.now()}, 1)
			case "permit":
				_, err := b.providerDriveEpochResult(context.Background(), id, &protocol.ProviderDriveEpochResultRequestPayload{DriveAttemptID: request.DriveAttemptID, Ordinal: request.Ordinal, Strategy: request.Strategy, Revision: request.Revision, Outcome: "unknown"})
				if err != nil {
					b.mu.Unlock()
					t.Fatal(err)
				}
			case "work":
				_, err := jobs.S.DB().Exec(`UPDATE identifiers SET value = ? WHERE kind = 'doi' AND work_request_id = (SELECT work_request_id FROM jobs WHERE id = ?)`, "10.1002/different", id)
				if err != nil {
					b.mu.Unlock()
					t.Fatal(err)
				}
			}
			b.mu.Unlock()
			if change == "backend_closed" {
				b.CloseAcquisitionBackend()
				b.mu.Lock()
				frames, err := b.drainAgentDecisions(context.Background(), "permit-test-holder")
				b.mu.Unlock()
				if err != nil || len(frames) != 0 {
					t.Fatalf("closed backend emitted %s %v", frames, err)
				}
				return
			}
			want := "stale"
			if change == "deadline" {
				want = "exhausted"
			}
			if result := awaitAgent(t, b); result.Outcome != want || result.Choice != "" {
				t.Fatalf("late reply=%+v", result)
			}
		})
	}
}

func TestAgentDecisionBudgetSurvivesCoordinatorReset(t *testing.T) {
	for _, condition := range []string{"count", "deadline", "duplicate"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int32
			backend := agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
				calls.Add(1)
				return acquisitionagent.Decision{Choice: "WAIT"}, nil
			})
			b, jobs, id, request := newAgentAttempt(t, backend)
			permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: request.DriveAttemptID, Ordinal: request.Ordinal, Strategy: request.Strategy, Revision: request.Revision})
			if err != nil {
				t.Fatal(err)
			}
			count, deadline := 1, time.Now().Add(time.Minute)
			if condition == "count" {
				count = agentDecisionLimit
			}
			if condition == "deadline" {
				deadline = time.Now().Add(-time.Minute)
			}
			for i := 0; i < count; i++ {
				rid := fmt.Sprintf("previous-%d", i)
				if condition == "duplicate" {
					rid = request.RequestID
				}
				if err := jobs.S.AppendEvent(context.Background(), id, "browser.agent_decision_requested", map[string]any{"permit_id": permit.ID, "request_id": rid, "deadline": deadline.Format(time.RFC3339Nano)}); err != nil {
					t.Fatal(err)
				}
			}
			b.SetAcquisitionBackend(backend)
			result := decodeAgent(t, submitAgent(t, b, id, request))
			want := "exhausted"
			if condition == "duplicate" {
				want = "stale"
			}
			if result.Outcome != want || calls.Load() != 0 {
				t.Fatalf("result=%+v calls=%d", result, calls.Load())
			}
		})
	}
}

func TestAgentDecisionFailsBeforeNetworkWithoutAuthority(t *testing.T) {
	for _, condition := range []string{"feature", "doi", "epoch", "backend", "unobserved_choice", "disabled_choice"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int32
			b, _, id, request := newAgentAttempt(t, agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
				calls.Add(1)
				choice := "invented"
				if condition == "disabled_choice" {
					choice = "c1"
				}
				return acquisitionagent.Decision{Choice: choice}, nil
			}))
			switch condition {
			case "feature":
				b.arbitration.holderSession().Features = slices.DeleteFunc(b.arbitration.holderSession().Features, func(s string) bool { return s == agentFallbackFeature })
			case "doi":
				request.Observation.DOI = "10.1002/different"
			case "epoch":
				request.Ordinal++
			case "backend":
				b.SetAcquisitionBackend(nil)
			case "disabled_choice":
				request.Observation.Controls[0].Disabled = true
			}
			frames := submitAgent(t, b, id, request)
			var result protocol.AgentDecideResultV1Payload
			if condition == "unobserved_choice" || condition == "disabled_choice" {
				result = awaitAgent(t, b)
			} else {
				result = decodeAgent(t, frames)
				if calls.Load() != 0 {
					t.Fatal("unauthorized backend call")
				}
			}
			if result.Outcome == "decision" || result.Choice != "" {
				t.Fatalf("invalid result=%+v", result)
			}
		})
	}
}

func TestAgentHelloPreservesLegacyCapAndNativeViewer(t *testing.T) {
	b, _, _, _ := newBridge(t)
	backend := agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		return acquisitionagent.Decision{}, nil
	})
	for _, configured := range []bool{false, true} {
		if configured {
			b.SetAcquisitionBackend(backend)
		}
		for _, agentPeer := range []bool{false, true} {
			peer := []string{nativeViewerDownloadFeature}
			if agentPeer {
				peer = append(peer, agentFallbackFeature)
			}
			raw, err := b.helloAck(sessionRolePending, peer)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			features := msg.Payload.(*protocol.HelloAckPayload).Features
			if len(features) > 32 || !slices.Contains(features, nativeViewerDownloadFeature) || !slices.Contains(features, triageSnapshotSchema5Feature) {
				t.Fatalf("invalid capabilities=%v", features)
			}
			if slices.Contains(features, agentFallbackFeature) != (configured && agentPeer) {
				t.Fatalf("agent capability configured=%v peer=%v features=%v", configured, agentPeer, features)
			}
		}
	}
}

func TestAgentDecisionStopsWhenHolderGoesSilent(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	b, _, id, request := newAgentAttempt(t, agentBackendFunc(func(ctx context.Context, _ acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return acquisitionagent.Decision{}, ctx.Err()
	}))
	submitAgent(t, b, id, request)
	<-entered
	b.mu.Lock()
	b.arbitration.holderSession().LastSyncAt = b.now().Add(-sessionStaleAfter - time.Second)
	b.mu.Unlock()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("silent holder left inference running")
	}
	if result := awaitAgent(t, b); result.Outcome != "stale" {
		t.Fatalf("silent holder result=%+v", result)
	}
}

func TestAgentDecisionCompletedMailboxRetiredOnRelease(t *testing.T) {
	b, _, id, request := newAgentAttempt(t, agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		return acquisitionagent.Decision{Choice: "c1"}, nil
	}))
	submitAgent(t, b, id, request)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		ready := b.agentDecisions[id] != nil && b.agentDecisions[id].result != nil
		if ready {
			b.release("permit-test-holder")
			remaining := len(b.agentDecisions)
			b.mu.Unlock()
			if remaining != 0 {
				t.Fatal("departed session retained completed decision")
			}
			return
		}
		b.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("decision did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
