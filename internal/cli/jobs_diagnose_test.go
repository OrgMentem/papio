// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
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
