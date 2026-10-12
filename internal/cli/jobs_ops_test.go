// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/work"
)

func jobDetailV3Fixture(id, state string) api.JobDetailV3 {
	return api.JobDetailV3{Job: &api.JobRow{Row: job.Row{ID: id, State: state, Work: work.Work{Title: "Waited"}}}}
}

// `jobs get --wait` returns the moment the job reaches a terminal state or
// needs a person (awaiting_human, needs_review), and keeps polling while the
// job is still being worked.
func TestJobsGetWaitStopsOnTerminalAndHumanStates(t *testing.T) {
	for _, state := range []string{
		job.StateReady, job.StateImported, job.StateUnavailable, job.StateFailed, job.StateCancelled,
		job.StateAwaitingHuman, job.StateNeedsReview,
	} {
		t.Run(state, func(t *testing.T) {
			var out, errOut bytes.Buffer
			polls := 0
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				if method != "jobs.get_v3" {
					t.Fatalf("method = %q, want jobs.get_v3", method)
				}
				polls++
				*result.(*api.JobDetailV3) = jobDetailV3Fixture("job_wait", state)
				return nil
			})
			// A state that wrongly keeps polling must fail the test, not hang it.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root.SetArgs([]string{"jobs", "get", "job_wait", "--wait"})
			if err := root.ExecuteContext(ctx); err != nil {
				t.Fatalf("jobs get --wait on %s: %v (stderr: %s)", state, err, errOut.String())
			}
			if polls != 1 {
				t.Fatalf("polled %d times on %s; want it to stop at once", polls, state)
			}
			if want := "job_wait\t" + state + "\ttitle:Waited\n"; out.String() != want {
				t.Fatalf("stdout = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestJobsGetWaitKeepsPollingWhileTheJobIsBeingWorked(t *testing.T) {
	var out, errOut bytes.Buffer
	var seen []string
	states := []string{job.StateQueued, job.StateReady}
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		state := states[len(seen)]
		seen = append(seen, state)
		*result.(*api.JobDetailV3) = jobDetailV3Fixture("job_wait", state)
		return nil
	})
	root.SetArgs([]string{"jobs", "get", "job_wait", "--wait"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs get --wait: %v (stderr: %s)", err, errOut.String())
	}
	if len(seen) != 2 {
		t.Fatalf("polled states %v; want queued then ready", seen)
	}
	if !strings.HasPrefix(out.String(), "job_wait\tready\t") {
		t.Fatalf("stdout = %q; want the ready job", out.String())
	}
}

// An older daemon without jobs.get_v3 still waits: each poll falls back to
// jobs.get_v2 and the same stop set applies.
func TestJobsGetWaitFallsBackToTheOlderDaemonMethod(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.get_v3":
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		case "jobs.get_v2":
			*result.(*api.JobDetailV2) = api.JobDetailV2{Job: jobDetailV3Fixture("job_wait", job.StateAwaitingHuman).Job}
			return nil
		}
		t.Fatalf("unexpected method %q", method)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root.SetArgs([]string{"jobs", "get", "job_wait", "--wait"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("jobs get --wait against an older daemon: %v (stderr: %s)", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "job_wait\tawaiting_human\t") {
		t.Fatalf("stdout = %q; want the awaiting_human job", out.String())
	}
}

// A script that only checks $? must not read "nothing was cancelled" as a stop.
func TestJobsCancelExitsNonzeroWhenTheJobWasAlreadyFinished(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		wantErr   bool
		wantPrint string
	}{
		{"ready is not cancellable", job.StateReady, true, "job_01 was already ready; nothing to cancel\n"},
		{"cancelled is success", job.StateCancelled, false, "Cancelled job_01\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				if method == "jobs.get" {
					*result.(*api.JobDetail) = api.JobDetail{Job: &job.Row{ID: "job_01", State: tc.state}}
				}
				return nil
			})
			root.SetArgs([]string{"jobs", "cancel", "job_01"})
			err := root.ExecuteContext(context.Background())
			if tc.wantErr != errors.Is(err, errJobNotCancelled) || (!tc.wantErr && err != nil) {
				t.Fatalf("jobs cancel on %s error = %v; want errJobNotCancelled = %t", tc.state, err, tc.wantErr)
			}
			if out.String() != tc.wantPrint {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.wantPrint)
			}
		})
	}
}

// An unknown --state matches no rows, so without a check a typo prints an empty
// list and exits 0, which reads as "nothing needs attention".
func TestJobsListStateFilterRejectsUnknownStatesAndForgivesHyphens(t *testing.T) {
	t.Run("unknown state fails before the daemon is called", func(t *testing.T) {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
			t.Fatal("an unknown state must never reach the daemon")
			return nil
		})
		root.SetArgs([]string{"jobs", "list", "--state", "readdy"})
		err := root.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), `"readdy"`) || !strings.Contains(err.Error(), "awaiting_human") {
			t.Fatalf("jobs list --state readdy = %v; want an error naming the value and the valid states", err)
		}
	})
	t.Run("hyphenated state is normalised", func(t *testing.T) {
		var out, errOut bytes.Buffer
		var sent any
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, _ any) error {
			if method == "jobs.list_v3" {
				sent = params.(map[string]any)["state"]
			}
			return nil
		})
		root.SetArgs([]string{"jobs", "list", "--state", "Awaiting-Human"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("jobs list --state Awaiting-Human: %v (stderr: %s)", err, errOut.String())
		}
		if sent != job.StateAwaitingHuman {
			t.Fatalf("state sent to the daemon = %v; want %s", sent, job.StateAwaitingHuman)
		}
	})
}

// dismiss, resolve and redrive refuse without --revision, so the plain-text
// listing has to show it: an operator who never passes --json can then copy it.
func TestActionsListTextShowsTheRevisionColumn(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "actions.list_v3" {
			t.Fatalf("unexpected method %q", method)
		}
		*result.(*api.ActionsPageV3) = api.ActionsPageV3{Actions: []api.ActionRow{{
			HumanAction: job.HumanAction{ID: 3, JobID: "job_x", Kind: "openurl_handoff", Status: "open", Revision: 7, CreatedAt: "2026-08-03T00:00:00Z"},
		}}}
		return nil
	})
	root.SetArgs([]string{"actions", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions list: %v (stderr: %s)", err, errOut.String())
	}
	columns := strings.Split(strings.TrimRight(out.String(), "\n"), "\t")
	if len(columns) < 5 || columns[0] != "3" || columns[3] != "open" || columns[4] != "7" {
		t.Fatalf("columns = %q; want id, job, kind, status, then the revision 7", columns)
	}
}

// Only job_created filed the capture; every other outcome leaves it parked or
// half-bound, so the exit status has to say the command did not do its job.
func TestGrabsIdentifyExitsNonzeroUnlessTheCaptureWasFiled(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		wantErr bool
	}{
		{"job_created", false},
		{"invalid_identifier", true},
		{"unknown_grab", true},
		{"already_owned", true},
		{"failed", true},
	} {
		for _, asJSON := range []bool{false, true} {
			name := tc.outcome
			args := []string{"grabs", "identify", "grab_01", "--doi", "10.1000/x"}
			if asJSON {
				name += " json"
				args = append([]string{"--json"}, args...)
			}
			t.Run(name, func(t *testing.T) {
				var out, errOut bytes.Buffer
				root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
					*result.(*api.GrabIdentifyResult) = api.GrabIdentifyResult{GrabID: "grab_01", Outcome: tc.outcome}
					return nil
				})
				root.SetArgs(args)
				err := root.ExecuteContext(context.Background())
				if (err != nil) != tc.wantErr {
					t.Fatalf("grabs identify outcome %s: error = %v; want error = %t", tc.outcome, err, tc.wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), tc.outcome) {
					t.Fatalf("error %q does not name the outcome %s", err, tc.outcome)
				}
				if !strings.Contains(out.String(), tc.outcome) {
					t.Fatalf("stdout %q must still carry the outcome %s", out.String(), tc.outcome)
				}
			})
		}
	}
}

func TestRepairAwaitingHumanExitsNonzeroWhenNothingWasRepaired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result api.RepairResult
		want   bool
	}{
		{"repaired", api.RepairResult{JobID: "job_01", Repaired: true, Outcome: "repaired", State: job.StateResolving}, false},
		{"not parked", api.RepairResult{JobID: "job_01", Outcome: "not_parked", State: job.StateReady}, true},
		{"open actions", api.RepairResult{JobID: "job_01", Outcome: "has_open_actions", State: job.StateAwaitingHuman}, true},
		{"conflict", api.RepairResult{JobID: "job_01", Outcome: "conflict", State: job.StateAwaitingHuman}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
				*result.(*api.RepairResult) = tc.result
				return nil
			})
			root.SetArgs([]string{"jobs", "repair-awaiting-human", "job_01"})
			err := root.ExecuteContext(context.Background())
			if tc.want != errors.Is(err, errNotRepaired) || (!tc.want && err != nil) {
				t.Fatalf("repair outcome %s: error = %v; want errNotRepaired = %t", tc.result.Outcome, err, tc.want)
			}
			if !strings.Contains(out.String(), tc.result.Outcome) {
				t.Fatalf("stdout %q must still carry the outcome", out.String())
			}
		})
	}
}

// With several ids, the operator must be able to tell which one was unknown,
// even when the daemon's message does not say.
func TestExportJobNamesTheUnknownID(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		id := params.(map[string]string)["job_id"]
		if id == "job_bad" {
			return &ipc.RemoteError{Code: "not_found", Message: "record not found"}
		}
		switch method {
		case "jobs.get_v2":
			*result.(*api.JobDetailV2) = api.JobDetailV2{Job: &api.JobRow{Row: job.Row{ID: id, State: job.StateReady, Work: work.Work{Title: "T"}}}}
		}
		return nil
	})
	root.SetArgs([]string{"export", "job", "job_good", "job_bad", "-o", filepath.Join(t.TempDir(), "out.json")})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"job_bad"`) || strings.Contains(err.Error(), "job_good") {
		t.Fatalf("export job error = %v; want it to name job_bad only", err)
	}
}
