// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

func TestJobsDiagnosePrefersV2AndRendersCutover(t *testing.T) {
	var out, errOut bytes.Buffer
	var calls []string
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		calls = append(calls, method)
		if method != "jobs.diagnose_v2" {
			return errors.New("unexpected method: " + method)
		}
		*result.(*api.JobDiagnosisV2) = api.JobDiagnosisV2{
			Diagnosis:          api.JobDiagnosis{JobID: "job_test", State: job.StateAwaitingHuman, Reason: job.DiagnosisReasonHumanAuthRequired, Why: "sign in", Next: "complete sign-in"},
			InstitutionCutover: &job.InstitutionCutoverDecision{Blocker: job.InstitutionCutoverBlockerPolicyGate, CanaryReadyRouteExists: true},
		}
		return nil
	})
	root.SetArgs([]string{"jobs", "diagnose", "job_test"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "jobs.diagnose_v2" {
		t.Fatalf("calls = %v, want only v2", calls)
	}
	if !strings.Contains(out.String(), "cutover blocker\tpolicy_gate") || !strings.Contains(out.String(), "canary ready route\tyes") {
		t.Fatalf("text output = %q", out.String())
	}
}

func TestJobsDiagnoseFallsBackToV1OnlyForUnknownMethod(t *testing.T) {
	var out, errOut bytes.Buffer
	var calls []string
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		calls = append(calls, method)
		if method == "jobs.diagnose_v2" {
			return &ipc.RemoteError{Code: "unknown_method", Message: "not supported"}
		}
		if method != "jobs.diagnose_v1" {
			return errors.New("unexpected method: " + method)
		}
		*result.(*api.JobDiagnosis) = api.JobDiagnosis{JobID: "job_test", State: job.StateReady, Reason: job.DiagnosisReasonComplete, Why: "done", Next: "export"}
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "diagnose", "job_test"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "jobs.diagnose_v2,jobs.diagnose_v1" {
		t.Fatalf("calls = %v, want one bounded fallback", calls)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json output = %q: %v", out.String(), err)
	}
	if _, ok := decoded["institution_cutover"]; ok {
		t.Fatalf("v1 fallback unexpectedly widened output: %v", decoded)
	}
}

func TestJobsDiagnoseDoesNotFallbackOnArbitraryV2Failure(t *testing.T) {
	var out, errOut bytes.Buffer
	var calls []string
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
		calls = append(calls, method)
		return errors.New("daemon transport failed")
	})
	root.SetArgs([]string{"jobs", "diagnose", "job_test"})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("arbitrary v2 failure unexpectedly succeeded")
	}
	if strings.Join(calls, ",") != "jobs.diagnose_v2" {
		t.Fatalf("calls = %v, want no v1 fallback", calls)
	}
}

// TestJobsDiagnoseNamesTheSiblingAnOpenedHandoffIsQueuedBehind: measured live
// 2026-09-23, explicitly opened papers sat parked behind another paper's live
// institutional surface while `jobs diagnose` told the operator to "open the
// handoff" they had already opened. The diagnosis must name the job that holds
// the institution instead, for both gates that park a candidate: a sibling's
// live surface in the same safety domain, and the institution's sign-in slot.
func TestJobsDiagnoseNamesTheSiblingAnOpenedHandoffIsQueuedBehind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opened    bool
		seed      func(t *testing.T, jobs *job.Store, sibling string)
		wantPhase string
	}{
		{
			name:   "sibling live surface in the same safety domain",
			opened: true,
			seed: func(t *testing.T, jobs *job.Store, sibling string) {
				candidate := diagnoseQueueCandidate(t, jobs, sibling, "domain-shared")
				claim, err := jobs.ClaimMaterialization(context.Background(), job.MaterializationClaimInput{
					CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: candidate.JobAttemptRevision,
					InstitutionProfileRevision: candidate.InstitutionProfileRevision, RouteRevision: candidate.RouteRevision,
					MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Hour),
				})
				if err != nil {
					t.Fatalf("claim sibling: %v", err)
				}
				if err := jobs.BindMaterialization(context.Background(), claim.ID, claim.BindingID, 1, candidate.InstitutionProfileRevision, 7); err != nil {
					t.Fatalf("bind sibling: %v", err)
				}
			},
			wantPhase: "phase bound",
		},
		{
			name:   "sibling holds the institution sign-in slot",
			opened: true,
			seed: func(t *testing.T, jobs *job.Store, sibling string) {
				diagnoseQueueCandidate(t, jobs, sibling, "domain-sibling-only")
				if _, err := jobs.ReserveAuthenticationEntryLease(context.Background(), job.AuthenticationEntryLeaseInput{
					AuthenticationClaimID: "claim-diagnose-queue", LeaseID: "lease-diagnose-queue", OwnerID: sibling,
					BrowserHolderGeneration: 1, LeaseUntil: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("reserve sign-in slot: %v", err)
				}
			},
			wantPhase: "phase reserved",
		},
		{
			// Nothing was opened yet, so opening the handoff is still the step.
			name:   "handoff not opened",
			opened: false,
			seed: func(t *testing.T, jobs *job.Store, sibling string) {
				if _, err := jobs.ReserveAuthenticationEntryLease(context.Background(), job.AuthenticationEntryLeaseInput{
					AuthenticationClaimID: "claim-diagnose-queue", LeaseID: "lease-diagnose-queue", OwnerID: sibling,
					BrowserHolderGeneration: 1, LeaseUntil: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("reserve sign-in slot: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg := config.Default()
			cfg.AccessMode = config.ModeConservative
			cfg.DataDir = storetest.DataDir(t)
			cfg.Browser.AdoptionRoot = filepath.Join(cfg.DataDir, "adoptions")
			system, err := bootstrap.New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = system.Close() })
			jobs := system.Jobs
			if _, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
				ConfiguredName: "default", AuthorityDigest: "digest-diagnose-queue", AuthenticationClaimID: "claim-diagnose-queue",
			}}); err != nil {
				t.Fatal(err)
			}
			waiting := diagnoseQueueJob(t, jobs, "wr_diagnose_waiting", "10.1000/waiting")
			sibling := diagnoseQueueJob(t, jobs, "wr_diagnose_sibling", "10.1000/sibling")
			diagnoseQueueCandidate(t, jobs, waiting, "domain-shared")
			tc.seed(t, jobs, sibling)
			if tc.opened {
				if err := jobs.RecordEvent(ctx, waiting, "handoff.opened", map[string]any{"principal": "cli"}); err != nil {
					t.Fatal(err)
				}
			}

			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, cfg, api.InProcessCaller(system))
			root.SetArgs([]string{"jobs", "diagnose", waiting})
			if err := root.ExecuteContext(ctx); err != nil {
				t.Fatalf("jobs diagnose: %v (%s)", err, errOut.String())
			}
			got := out.String()
			if tc.wantPhase == "" {
				if strings.Contains(got, "waiting:") || !strings.Contains(got, "next\topen the handoff") {
					t.Fatalf("output = %q, want the unopened handoff to still be the next step", got)
				}
				return
			}
			want := "next\twaiting: institution sign-in slot / live claim held by " + sibling + " (" + tc.wantPhase + ", since "
			if !strings.Contains(got, want) {
				t.Fatalf("output = %q, want a line containing %q", got, want)
			}
			if strings.Contains(got, "open the handoff") {
				t.Fatalf("output = %q still tells the operator to open a handoff they already opened", got)
			}
		})
	}
}

func diagnoseQueueJob(t *testing.T, jobs *job.Store, requestID, doi string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: doi}, "", "", job.Policy{
		AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff available", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id
}

func diagnoseQueueCandidate(t *testing.T, jobs *job.Store, jobID, domain string) *job.BrowserCandidate {
	t.Helper()
	ctx := context.Background()
	profiles, err := jobs.ListInstitutionProfiles(ctx, false)
	if err != nil || len(profiles) == 0 {
		t.Fatalf("list institution profiles: %v (%d)", err, len(profiles))
	}
	attempt, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		JobID: jobID, JobAttemptRevision: attempt,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "pre-route-" + domain, SafetyDomainID: domain,
		AdapterRevision: "test-adapter", EffectContractID: "test-effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}
