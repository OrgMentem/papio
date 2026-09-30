// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
)

func TestGrabIdentifierFlagsRequiresExactlyOne(t *testing.T) {
	for name, tc := range map[string]struct {
		doi, pmid, arxiv string
		wantErr          bool
	}{
		"none":     {wantErr: true},
		"multiple": {doi: "10.1000/test", pmid: "12345", wantErr: true},
		"doi":      {doi: "10.1000/test"},
		"pmid":     {pmid: "12345"},
		"arxiv":    {arxiv: "2401.00001"},
	} {
		t.Run(name, func(t *testing.T) {
			kind, value, err := grabIdentifierFlags(tc.doi, tc.pmid, tc.arxiv)
			if tc.wantErr {
				if err == nil {
					t.Fatal("grabIdentifierFlags succeeded; want error")
				}
				return
			}
			if err != nil || kind == "" || value == "" {
				t.Fatalf("grabIdentifierFlags = %q, %q, %v", kind, value, err)
			}
		})
	}
}

func TestGrabsConfirmExitStatusFollowsTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		result  api.GrabConfirmResult
		wantErr bool
		wantOut string
	}{
		{result: api.GrabConfirmResult{GrabID: "grab-1", JobID: "job-1", Outcome: "job_created"}, wantOut: "grab-1\tjob_created\tjob-1\n"},
		{result: api.GrabConfirmResult{GrabID: "grab-1", JobID: "job-1", Outcome: "refused_identity", Detail: "front matter names 10.1000/other"}, wantErr: true, wantOut: "refused — front matter names 10.1000/other"},
		// The daemon echoes the picked job on a failure too; the partial bind
		// must print its detail, not look like a filed capture.
		{result: api.GrabConfirmResult{GrabID: "grab-1", JobID: "job-1", Outcome: "failed", Detail: "pdf grab bound but bytes could not be adopted"}, wantErr: true, wantOut: "grab-1\tfailed\tpdf grab bound but bytes could not be adopted\n"},
		{result: api.GrabConfirmResult{GrabID: "grab-1", JobID: "job-1", Outcome: "unknown_job", Detail: "job is not in the candidate-eligible pool"}, wantErr: true, wantOut: "unknown_job"},
	} {
		for _, jsonOutput := range []bool{false, true} {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				if method != "grabs.confirm" {
					t.Fatalf("method = %q", method)
				}
				*result.(*api.GrabConfirmResult) = tc.result
				return nil
			})
			args := []string{"grabs", "confirm", "grab-1", "--job", "job-1"}
			if jsonOutput {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			err := root.ExecuteContext(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("%s (json=%v): err = %v, want error %v", tc.result.Outcome, jsonOutput, err, tc.wantErr)
			}
			if jsonOutput {
				var decoded api.GrabConfirmResult
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded != tc.result {
					t.Fatalf("%s --json = %+v, %v; want the structured result preserved", tc.result.Outcome, decoded, err)
				}
			} else if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("%s: stdout = %q, want %q", tc.result.Outcome, out.String(), tc.wantOut)
			}
		}
	}
}

func TestGrabsBindsNamesTheNextPageAndFallsBackOnOlderDaemons(t *testing.T) {
	page := grabsBindsResult{Binds: []api.GrabBindRow{
		{GrabID: "grab_0000000000000000000000newest", JobID: "job-1"},
		{GrabID: "grab_0000000000000000000000oldest", JobID: "job-2"},
	}, Truncated: true}
	var calls []string
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		calls = append(calls, method)
		if got := params.(map[string]any)["before"]; got != "grab_cursor" {
			t.Fatalf("before = %v, want the --before cursor", got)
		}
		*result.(*grabsBindsResult) = page
		return nil
	})
	root.SetArgs([]string{"grabs", "binds", "--before", "grab_cursor"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("grabs binds --before: %v", err)
	}
	if want := "next page: papio grabs binds --before grab_0000000000000000000000oldest\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("stdout = %q, want it to end with %q", out.String(), want)
	}
	if len(calls) != 1 || calls[0] != "grabs.binds_v2" {
		t.Fatalf("calls = %v, want grabs.binds_v2", calls)
	}

	// An older daemon has no cursor: the first page still comes from
	// grabs.binds, but a --before request must not silently restart there.
	oldDaemon := func(_ context.Context, method string, _ any, result any) error {
		if method == "grabs.binds_v2" {
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		}
		*result.(*grabsBindsResult) = grabsBindsResult{Binds: page.Binds}
		return nil
	}
	out.Reset()
	root = NewInProcessRoot(&out, &errOut, config.Config{}, oldDaemon)
	root.SetArgs([]string{"grabs", "binds"})
	if err := root.ExecuteContext(context.Background()); err != nil || !strings.Contains(out.String(), "grab_0000000") || strings.Contains(out.String(), "next page") {
		t.Fatalf("first page on an older daemon = %q, %v", out.String(), err)
	}
	root = NewInProcessRoot(&out, &errOut, config.Config{}, oldDaemon)
	root.SetArgs([]string{"grabs", "binds", "--before", "grab_cursor"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "grabs.binds_v2") {
		t.Fatalf("--before on an older daemon = %v, want the upgrade-required error", err)
	}
}
