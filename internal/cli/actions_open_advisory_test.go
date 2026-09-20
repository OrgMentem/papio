// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/work"
)

// Conservative acquisition leaves an informational action on unavailable. The
// inbox can open its canonical link without reviving the job; the CLI must not
// lose that row in its awaiting_human-only join and claim a truncated empty page.
func TestActionsOpenConservativeAdvisoryOnUnavailableJob(t *testing.T) {
	for _, selector := range [][]string{nil, {"--job", "job_conservative_open"}, {"--action", "42"}} {
		t.Run("selector="+fmtSelector(selector), func(t *testing.T) {
			row := job.Row{ID: "job_conservative_open", State: job.StateUnavailable,
				Work: work.Work{DOI: "10.1000/conservative"}, Policy: job.Policy{AccessMode: config.ModeConservative}}
			action := job.HumanAction{ID: 42, JobID: row.ID, Kind: "openurl_available", Status: "open",
				Detail: "no direct candidates; institutional OpenURL available but not opened in conservative mode"}
			beforeRow, beforeAction := row, action
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative,
				Browser: config.Browser{OpenURLBase: "https://resolver.example.test/openurl"}},
				func(_ context.Context, method string, params, result any) error {
					switch method {
					case "actions.list":
						*result.(*[]job.HumanAction) = []job.HumanAction{action}
					case "jobs.list_v2":
						if len(selector) > 0 {
							t.Fatal("selector must skip bulk jobs read")
						}
						if !reflect.DeepEqual(params, map[string]any{"state": job.StateAwaitingHuman, "limit": job.ListLimitMax}) {
							t.Fatalf("bulk params: %#v", params)
						}
						*result.(*api.JobsPage) = api.JobsPage{}
					case "jobs.get":
						if !reflect.DeepEqual(params, map[string]string{"job_id": row.ID}) {
							t.Fatalf("unrelated job lookup: %#v", params)
						}
						*result.(*api.JobDetail) = api.JobDetail{Job: &row, Actions: []job.HumanAction{action}}
					default:
						t.Fatalf("manual Open must not mutate a terminal job or request browser authority: %s", method)
					}
					return nil
				})
			root.SetArgs(append([]string{"--json", "actions", "open", "--dry-run"}, selector...))
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			var page struct {
				URLs      []string `json:"urls"`
				Truncated bool     `json:"truncated"`
			}
			if err := json.Unmarshal(out.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(page.URLs, []string{"https://doi.org/10.1000/conservative"}) || page.Truncated {
				t.Fatalf("conservative manual Open = %s, want the canonical URL without truncation", out.String())
			}
			if !reflect.DeepEqual(row, beforeRow) || !reflect.DeepEqual(action, beforeAction) {
				t.Fatal("manual Open changed job or action")
			}
		})
	}
}

func fmtSelector(selector []string) string {
	if len(selector) == 0 {
		return "queue"
	}
	return selector[0]
}

func TestActionsOpenJoinsBeyondJobPageLimit(t *testing.T) {
	// An older action is still actionable after more than a page of newer jobs.
	actions := make([]job.HumanAction, job.ListLimitMax+2)
	rows := make(map[string]job.Row, len(actions))
	for i := range actions {
		id := fmt.Sprintf("job_%03d", i)
		actions[i] = job.HumanAction{ID: int64(len(actions) - i), JobID: id, Kind: "openurl_available", Status: "open"}
		rows[id] = job.Row{ID: id, State: job.StateUnavailable, Work: work.Work{DOI: "10.1000/" + id}}
		if i < len(actions)-1 {
			row := rows[id]
			row.State = job.StateAwaitingHuman
			rows[id] = row
			actions[i].Kind = "openurl_handoff"
			actions[i].Detail = "https://doi.org/10.1000/" + id
		}
	}
	oldest := actions[len(actions)-1].JobID
	for _, selector := range [][]string{nil, {"--job", oldest}, {"--action", "1"}, {"--limit", "2"}} {
		t.Run(fmtSelector(selector), func(t *testing.T) {
			var out, errOut bytes.Buffer
			lookups := make(map[string]int)
			bulkCalls := 0
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
				switch method {
				case "actions.list":
					*result.(*[]job.HumanAction) = actions
				case "jobs.list_v2":
					bulkCalls++
					if len(selector) > 0 && selector[0] != "--limit" {
						t.Fatal("selector must skip bulk jobs read")
					}
					if !reflect.DeepEqual(params, map[string]any{"state": job.StateAwaitingHuman, "limit": job.ListLimitMax}) {
						t.Fatalf("bulk params: %#v", params)
					}
					page := api.JobsPage{Truncated: true}
					for _, action := range actions[:job.ListLimitMax] {
						page.Jobs = append(page.Jobs, rows[action.JobID])
					}
					*result.(*api.JobsPage) = page
				case "jobs.get":
					id := params.(map[string]string)["job_id"]
					row, ok := rows[id]
					if !ok {
						t.Fatalf("unrelated lookup %q", id)
					}
					lookups[id]++
					*result.(*api.JobDetail) = api.JobDetail{Job: &row}
				default:
					t.Fatalf("unexpected RPC %q", method)
				}
				return nil
			})
			root.SetArgs(append([]string{"--json", "actions", "open", "--dry-run"}, selector...))
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			var page struct {
				URLs      []string `json:"urls"`
				Truncated bool     `json:"truncated"`
			}
			if err := json.Unmarshal(out.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			want := make([]string, len(actions))
			for i, action := range actions {
				want[i] = "https://doi.org/10.1000/" + action.JobID
			}
			truncated := false
			if len(selector) > 0 {
				if selector[0] == "--limit" {
					want, truncated = want[:2], true
				} else {
					want = want[len(want)-1:]
					if !reflect.DeepEqual(lookups, map[string]int{oldest: 1}) {
						t.Fatalf("selector fetched unrelated jobs: %v", lookups)
					}
				}
			}
			if len(selector) == 0 || selector[0] == "--limit" {
				if bulkCalls != 1 || !reflect.DeepEqual(lookups, map[string]int{oldest: 1, actions[job.ListLimitMax].JobID: 1}) {
					t.Fatalf("queue fetched joined jobs: bulk=%d exact=%v", bulkCalls, lookups)
				}
			}
			if !reflect.DeepEqual(page.URLs, want) || page.Truncated != truncated {
				t.Fatalf("got %d URLs, truncated %v; want %d in action order, truncated %v", len(page.URLs), page.Truncated, len(want), truncated)
			}
		})
	}
}

func TestActionsOpenJobLookupDeduplicatesAndPropagatesErrors(t *testing.T) {
	for _, rpcErr := range []error{nil, &ipc.RemoteError{Code: "not_found"}, errors.New("connection lost")} {
		t.Run(fmt.Sprint(rpcErr), func(t *testing.T) {
			calls := 0
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
				switch method {
				case "actions.list":
					*result.(*[]job.HumanAction) = []job.HumanAction{
						{JobID: "closed", Status: "resolved"}, {JobID: "same", Status: "open"}, {JobID: "same", Status: "open"},
					}
				case "jobs.list_v2":
					*result.(*api.JobsPage) = api.JobsPage{}
				case "jobs.get":
					calls++
					if !reflect.DeepEqual(params, map[string]string{"job_id": "same"}) {
						t.Fatalf("unexpected lookup: %#v", params)
					}
					*result.(*api.JobDetail) = api.JobDetail{Job: &job.Row{ID: "same", State: job.StateUnavailable}}
					return rpcErr
				default:
					t.Fatalf("unexpected RPC %q", method)
				}
				return nil
			})
			root.SetArgs([]string{"--json", "actions", "open", "--dry-run"})
			err := root.ExecuteContext(context.Background())
			if calls != 1 {
				t.Fatalf("lookup count=%d, want one per distinct open job", calls)
			}
			var remote *ipc.RemoteError
			if errors.As(rpcErr, &remote) || rpcErr == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, rpcErr) {
				t.Fatalf("lost lookup failure: %v", err)
			}
		})
	}
}

func TestActionsOpenAdvisoryCannotMintAuthority(t *testing.T) {
	for _, kind := range []string{"openurl_available", "openurl_handoff", "manual_download", "verify_identity"} {
		for _, state := range []string{job.StateUnavailable, job.StateReady, job.StateCancelled} {
			for _, closed := range []bool{false, true} {
				t.Run(kind+"/"+state+"/closed="+strconv.FormatBool(closed), func(t *testing.T) {
					action := job.HumanAction{JobID: "terminal", Kind: kind, Status: "open", Detail: "https://untrusted.example/redirect"}
					if closed {
						action.Status = "resolved"
					}
					row := job.Row{ID: action.JobID, State: state, Work: work.Work{DOI: "10.48612//monograph-2025-2"}, Policy: job.Policy{AccessMode: config.ModeConservative}}
					targets, dropped := actionHandoffTargets([]job.HumanAction{action}, []job.Row{row}, nil, 0)
					if dropped != 0 {
						t.Fatal("known terminal job marked missing")
					}
					if closed || kind != "openurl_available" || state != job.StateUnavailable {
						if len(targets) != 0 {
							t.Fatalf("stale action became openable: %v", targets)
						}
						return
					}
					const canonical = "https://doi.org/10.48612//monograph-2025-2"
					if len(targets) != 1 || targets[0].Tracked || targets[0].URL != canonical {
						t.Fatalf("manual target=%v", targets)
					}
					opened := 0
					err := focusOrOpenActionURLs(context.Background(), []string{targets[0].URL}, []string{targets[0].URL}, nil, false, &bytes.Buffer{},
						func(context.Context, []string) (api.ActionsOpenResult, error) {
							t.Fatal("advisory requested browser authority")
							return api.ActionsOpenResult{}, nil
						},
						func(_ context.Context, _ string, args ...string) error {
							opened++
							if args[len(args)-1] != canonical {
								t.Fatalf("launcher args %v", args)
							}
							return nil
						})
					if err != nil || opened != 1 {
						t.Fatalf("opened=%d error=%v", opened, err)
					}
				})
			}
		}
	}
}

func TestActionsOpenAdvisoryCanonicalLink(t *testing.T) {
	for _, test := range []struct {
		work work.Work
		want string
	}{
		{work.Work{DOI: "10.1000/paper?part#section"}, "https://doi.org/10.1000/paper%3Fpart%23section"},
		{work.Work{ArXiv: "2301.08745v2"}, "https://arxiv.org/abs/2301.08745v2"},
		{work.Work{OpenAlex: "W2741809807"}, "https://openalex.org/W2741809807"},
		{work.Work{Title: "No canonical link"}, ""},
	} {
		t.Run(test.work.Describe(), func(t *testing.T) {
			got, ok := actionURL(job.HumanAction{Kind: "openurl_available", Detail: "https://untrusted.example/"}, job.Row{Work: test.work}, nil)
			if got != test.want || ok != (test.want != "") {
				t.Fatalf("canonical link=%q,%v want %q", got, ok, test.want)
			}
		})
	}
}
