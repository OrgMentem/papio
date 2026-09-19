// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"papio/internal/bootstrap"
	"papio/internal/store/storetest"
	"path/filepath"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

func TestPublisherRetryAPIUsesRevisionAndRejectsCallerURL(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.DataDir = storetest.DataDir(t)
	cfg.Browser.AdoptionRoot = filepath.Join(cfg.DataDir, "adoptions")
	system, err := bootstrap.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	id, err := system.Jobs.CreateRequest(ctx, "publisher-api", work.Work{DOI: "10.1000/publisher"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any"}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.ParkWithHumanAction(ctx, id, job.StateResolving, job.StateAwaitingHuman, "manual_download", "different work", nil, job.Access(true, "landing_page"), job.WithHumanActionDiagnosis(job.DiagnosisReasonWrongWork)); err != nil {
		t.Fatal(err)
	}
	actions, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	a := actions[0]
	router := Router(system)
	for _, params := range []map[string]any{
		{"action_id": a.ID, "expected_revision": a.Revision, "url": "https://evil.example"},
		{"action_id": a.ID}, {"action_id": -1, "expected_revision": 1},
	} {
		if rpcErr := callMethod(t, router, "actions.retry_publisher", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("params=%v error=%+v", params, rpcErr)
		}
	}
	hello, err := json.Marshal(map[string]any{"protocol": protocol.BrowserProtocolVersion, "type": "hello", "msg_id": "publisher-hello", "seq": 0, "payload": map[string]any{"extension_version": "0.21.0", "features": []string{protocol.InstitutionalMaterializationFeature, protocol.EffectPermitFeature}}})
	if err != nil {
		t.Fatal(err)
	}
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{"session_id": "aaaabbbbccccddddeeeeffff00001111", "messages": []json.RawMessage{hello}}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if rpcErr := callMethod(t, router, "actions.retry_publisher", map[string]any{"action_id": a.ID, "expected_revision": a.Revision + 1}, nil); rpcErr == nil {
		t.Fatal("stale revision accepted")
	}
	var result SubmitResult
	if rpcErr := callMethod(t, router, "actions.retry_publisher", map[string]any{"action_id": a.ID, "expected_revision": a.Revision}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.JobID != id {
		t.Fatalf("result=%+v", result)
	}
	open, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].Detail != job.PublisherHandoffDetail {
		t.Fatalf("open=%v err=%v", open, err)
	}
}
