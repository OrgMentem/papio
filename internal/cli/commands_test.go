// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/browser"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/work"
)

func TestAccessHintClassifiesOpenAndInstitutionalAccess(t *testing.T) {
	tests := []struct {
		name   string
		action job.HumanAction
		want   string
	}{
		{
			name:   "open access",
			action: job.HumanAction{RequiresAuth: false, BlockedBy: "anti_bot"},
			want:   "\topen access — no login needed",
		},
		{
			name:   "institutional handoff",
			action: job.HumanAction{Kind: "openurl_handoff", RequiresAuth: true, BlockedBy: "paywall"},
			want:   "\tsign in to your institution first, then 'papio actions open'",
		},
		{
			// `papio actions open` cannot open a manual_download — actionURL
			// rejects the kind — so naming it here sent the user to a command
			// that silently did nothing for eight rows on one machine.
			name:   "manual download behind a paywall",
			action: job.HumanAction{Kind: "manual_download", RequiresAuth: true, BlockedBy: "paywall"},
			want:   "\tsign in to your institution, then download the PDF yourself — papio will adopt it",
		},
		{
			name:   "manual download, open access",
			action: job.HumanAction{Kind: "manual_download", RequiresAuth: false, BlockedBy: "landing_page"},
			want:   "\tdownload the PDF yourself — papio will adopt it; no login needed",
		},
		{
			name: "unclassified",
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := accessHint(test.action); got != test.want {
				t.Fatalf("access hint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestActionURLsSelectAwaitingActionsMostRecentAndDryRun(t *testing.T) {
	base := "https://openurl.example.test/resolve"
	instituteBase := "https://institute.example.test/resolve"
	instFor := func(name string) (config.Institution, bool) {
		switch name {
		case "", "default":
			return config.Institution{OpenURLBase: base}, true
		case "institute":
			return config.Institution{OpenURLBase: instituteBase}, true
		case "libkeyed":
			return config.Institution{OpenURLBase: instituteBase, LibKeyMode: "link", LibKeyLibraryID: 1234}, true
		}
		return config.Institution{}, false
	}
	oaURL := "https://oa.example.test/paper.pdf"
	rows := []job.Row{
		{ID: "oa", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/oa"}},
		{ID: "institutional", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/institutional", Title: "Institutional"}},
		{ID: "review", State: job.StateNeedsReview, Work: work.Work{DOI: "10.1000/review"}},
		{ID: "manual", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/manual"}},
		{ID: "profiled", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "institute"}, Work: work.Work{DOI: "10.1000/profiled"}},
		{ID: "unknownprofile", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "gone"}, Work: work.Work{DOI: "10.1000/unknown"}},
		{ID: "libkeyrouted", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "libkeyed"}, Work: work.Work{DOI: "10.1000/libkey"}},
	}
	actions := []job.HumanAction{
		{ID: 7, JobID: "libkeyrouted", Kind: "openurl_handoff", Status: "open"},
		{ID: 6, JobID: "profiled", Kind: "openurl_handoff", Status: "open"},
		{ID: 5, JobID: "unknownprofile", Kind: "openurl_handoff", Status: "open"},
		{ID: 4, JobID: "oa", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(oaURL)},
		{ID: 3, JobID: "institutional", Kind: "openurl_handoff", Status: "open"},
		{ID: 2, JobID: "review", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail("https://oa.example.test/review.pdf")},
		{ID: 1, JobID: "manual", Kind: "manual_download", Status: "open", Detail: "choose a file"},
	}

	want := []string{
		"https://libkey.io/libraries/1234/10.1000/libkey",
		browser.OpenURL(instituteBase, rows[4].Work), oaURL, browser.OpenURL(base, rows[1].Work),
	}
	got, dropped := actionURLs(actions, rows, instFor, 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs = %#v, want %#v", got, want)
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 — every action's job id is present in rows", dropped)
	}
	if limited, limitedDropped := actionURLs(actions, rows, instFor, 1); !reflect.DeepEqual(limited, want[:1]) || limitedDropped != 0 {
		t.Fatalf("limited URLs = %#v, dropped = %d, want %#v, 0", limited, limitedDropped, want[:1])
	}

	var out bytes.Buffer
	if err := openActionURLs(context.Background(), want, true, &out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("dry-run output = %q", got)
	}
}

func TestBrowserOpenCommandCarriesTargetOnEveryPlatform(t *testing.T) {
	const target = "https://resolver.example.test/open"
	name, args := browserOpenCommand(target)
	if name == "" {
		t.Fatal("browserOpenCommand returned empty launcher")
	}
	if len(args) == 0 || args[len(args)-1] != target {
		t.Fatalf("browserOpenCommand args = %v, want target last", args)
	}
	if runtime.GOOS == "darwin" && (name != "open" || args[0] != "-b" || args[1] != chromeBundleID) {
		t.Fatalf("darwin launcher = %s %v, want Chrome-pinned open", name, args)
	}
}

func TestOpenActionURLsReportsActionableBrowserFailure(t *testing.T) {
	runErr := errors.New("open: exit status 1")
	err := openActionURLs(context.Background(), []string{"https://resolver.example.test/open"}, false, &bytes.Buffer{}, func(context.Context, string, ...string) error {
		return runErr
	})
	if !errors.Is(err, runErr) {
		t.Fatalf("open error = %v, want wrapped runner error", err)
	}
	for _, fragment := range []string{"browser handoff could not open", "papio extension enabled", "papio doctor"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("open error = %q, missing %q", err, fragment)
		}
	}
}

func TestFocusOrOpenActionURLsPrefersLiveCompatibleHolder(t *testing.T) {
	const target = "https://resolver.example.test/open"
	var out bytes.Buffer
	focusCalls := 0
	openCalls := 0
	err := focusOrOpenActionURLs(context.Background(), []string{target}, []string{}, []string{"job_focus_001"}, false, &out,
		func(_ context.Context, ids []string) (api.ActionsOpenResult, error) {
			focusCalls++
			if !reflect.DeepEqual(ids, []string{"job_focus_001"}) {
				t.Fatalf("focus job IDs = %v", ids)
			}
			return api.ActionsOpenResult{Queued: 1, SessionLive: true}, nil
		},
		func(context.Context, string, ...string) error {
			openCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if focusCalls != 1 || openCalls != 0 {
		t.Fatalf("focus/open calls = %d/%d, want 1/0", focusCalls, openCalls)
	}

	const manual = "https://manual.example.test/open"
	focusCalls, openCalls = 0, 0
	err = focusOrOpenActionURLs(context.Background(), []string{target, manual}, []string{manual}, []string{"job_focus_001"}, false, &out,
		func(context.Context, []string) (api.ActionsOpenResult, error) {
			focusCalls++
			return api.ActionsOpenResult{Queued: 1, SessionLive: true}, nil
		},
		func(_ context.Context, _ string, args ...string) error {
			openCalls++
			if args[len(args)-1] != manual {
				t.Fatalf("untracked fallback args = %v, want %q last", args, manual)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if focusCalls != 1 || openCalls != 1 {
		t.Fatalf("mixed focus/open calls = %d/%d, want 1/1", focusCalls, openCalls)
	}

	focusCalls, openCalls = 0, 0
	err = focusOrOpenActionURLs(context.Background(), []string{target}, []string{}, []string{"job_focus_001"}, false, &out,
		func(context.Context, []string) (api.ActionsOpenResult, error) {
			focusCalls++
			return api.ActionsOpenResult{}, nil
		},
		func(context.Context, string, ...string) error {
			openCalls++
			return nil
		},
	)
	if !errors.Is(err, errNoConnectedBrowserSession) {
		t.Fatalf("no-session error = %v, want %q", err, errNoConnectedBrowserSession)
	}
	if got, want := err.Error(), "no connected browser extension session - open Chrome with the papio extension enabled; check papio doctor"; got != want {
		t.Fatalf("no-session error = %q, want %q", got, want)
	}
	if focusCalls != 1 || openCalls != 0 {
		t.Fatalf("no-session focus/open calls = %d/%d, want 1/0", focusCalls, openCalls)
	}

	out.Reset()
	err = focusOrOpenActionURLs(context.Background(), []string{target}, []string{}, []string{"job_focus_001"}, true, &out,
		func(context.Context, []string) (api.ActionsOpenResult, error) {
			t.Fatal("dry-run must not request a browser focus")
			return api.ActionsOpenResult{}, nil
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != target+"\n" {
		t.Fatalf("dry-run output = %q, want URL list unchanged", got)
	}
}

func TestActionsOpenFocusesTrackedHandoffWithoutChangingJSONURLs(t *testing.T) {
	const target = "https://oa.example.test/focus.pdf"
	action := job.HumanAction{
		ID: 1, JobID: "job_focus_001", Kind: "openurl_handoff", Status: "open",
		Detail: app.OABrowserHandoffActionDetail(target),
	}
	row := job.Row{ID: action.JobID, State: job.StateAwaitingHuman}
	var out, errOut bytes.Buffer
	var focusParams map[string]any
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		switch method {
		case "actions.list":
			*result.(*[]job.HumanAction) = []job.HumanAction{action}
		case "jobs.list_v2":
			*result.(*api.JobsPage) = api.JobsPage{Jobs: []job.Row{row}}
		case "actions.open":
			focusParams = params.(map[string]any)
			*result.(*api.ActionsOpenResult) = api.ActionsOpenResult{Queued: 1, SessionLive: true}
		default:
			t.Fatalf("unexpected method %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"--json", "actions", "open"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open: %v (%s)", err, errOut.String())
	}
	if !reflect.DeepEqual(focusParams, map[string]any{"job_ids": []string{action.JobID}}) {
		t.Fatalf("actions.open params = %#v", focusParams)
	}
	var page struct {
		URLs      []string `json:"urls"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.URLs, []string{target}) || page.Truncated {
		t.Fatalf("JSON URLs = %+v, want [%q] without truncation", page, target)
	}
}

func TestCommandGroupsRejectUnknownVerbs(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
		t.Fatal("unknown command must not call the daemon")
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "frobnicate", "job_01"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("unknown jobs verb succeeded, want an unknown-verb error")
	}
	for _, fragment := range []string{`unknown jobs command "frobnicate"`, "valid verbs:", "get", "show"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("unknown jobs verb error = %q, missing %q", err, fragment)
		}
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("unknown verb wrote output: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestBareCommandGroupPrintsHelpNotSilence(t *testing.T) {
	// Regression: the unknown-verb validator installs a RunE on non-runnable
	// groups; a no-op there made bare `papio jobs` exit 0 with no output —
	// the same silent class the validator exists to kill.
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
		t.Fatal("bare group must not call the daemon")
		return nil
	})
	root.SetArgs([]string{"jobs"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("bare jobs = %v, want help output with nil error", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "Usage:") {
		t.Fatalf("bare jobs printed no help: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestRunnableParentKeepsPositionalArgs(t *testing.T) {
	// Regression: the unknown-verb validator must not touch runnable parents.
	// `watch digest <id>` owns the `clear` subcommand AND takes a positional
	// id; rejecting its argument as an unknown verb broke the documented read.
	probe := errors.New("digest rpc reached")
	var method string
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, m string, _, _ any) error {
		method = m
		return probe
	})
	root.SetArgs([]string{"watch", "digest", "7"})

	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, probe) {
		t.Fatalf("watch digest 7 = %v, want the probe RPC error (dispatch to RunE)", err)
	}
	if method != "watch.digest" {
		t.Fatalf("RPC method = %q, want watch.digest", method)
	}
}

func TestJobsFailuresCommandOutputsGroups(t *testing.T) {
	want := jobsFailuresResult{
		Failures: []job.FailureGroup{{Count: 2, State: job.StateFailed, Provider: "api.example.test", Reason: "timeout", Sample: "job_01"}},
	}
	tests := []struct {
		name string
		json bool
	}{
		{name: "aligned rows"},
		{name: "json", json: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var gotParams map[string]any
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
				if method != "jobs.failures" {
					t.Fatalf("method = %q, want jobs.failures", method)
				}
				gotParams = params.(map[string]any)
				*result.(*jobsFailuresResult) = want
				return nil
			})
			args := []string{"jobs", "failures", "--since", "30d", "--limit", "2"}
			if tc.json {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("jobs failures: %v", err)
			}
			if !reflect.DeepEqual(gotParams, map[string]any{"since": "30d", "limit": 2}) {
				t.Fatalf("params = %#v", gotParams)
			}
			if tc.json {
				// Compare the exact document. Decoding back into the result
				// struct is what let a stray `since,omitempty` key ride along
				// unnoticed: a struct decode ignores keys it has no field for,
				// so it cannot see the page shape a consumer actually receives.
				const wantJSON = `{"failures":[{"state":"failed","provider":"api.example.test","reason":"timeout","count":2,"sample":"job_01"}],"truncated":false}` + "\n"
				if got := out.String(); got != wantJSON {
					t.Fatalf("JSON = %s, want %s", got, wantJSON)
				}
				return
			}
			if got := out.String(); got != "2 | failed | api.example.test | timeout (sample: job_01)\n" {
				t.Fatalf("output = %q", got)
			}
		})
	}

	command := newJobsCommand(&options{})
	failures, _, err := command.Find([]string{"failures"})
	if err != nil {
		t.Fatalf("find failures command: %v", err)
	}
	if failures.Annotations["mcp:read-only"] != "true" {
		t.Fatalf("failures annotations = %#v", failures.Annotations)
	}
}

// TestJobsFailuresDecodesTheDaemonsSinceField drives the real JSON decode
// path — ipc.DecodeResult, which DisallowUnknownFields (internal/ipc/
// protocol.go) — with the exact payload internal/api/failures.go emits when
// --since resolves to a window. Every other stub in this file assigns
// *result.(*jobsFailuresResult) = want directly and so structurally cannot
// see a decode-time "unknown field" error; this one can, and does fail
// before jobsFailuresResult regained its Since field (confirmed locally by
// deleting the field and re-running: the command errors with
// `decode ipc result: json: unknown field "since"`).
func TestJobsFailuresDecodesTheDaemonsSinceField(t *testing.T) {
	const daemonJSON = `{"failures":[{"state":"failed","provider":"api.example.test","reason":"timeout","count":1,"sample":"job_01"}],"since":"2026-06-25T00:00:00Z"}`
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "jobs.failures" {
			t.Fatalf("method = %q, want jobs.failures", method)
		}
		return ipc.DecodeResult(json.RawMessage(daemonJSON), result)
	})
	root.SetArgs([]string{"--json", "jobs", "failures", "--since", "30d"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs failures with a since-bearing daemon payload: %v (stderr: %s)", err, errOut.String())
	}
	var page map[string]any
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v (%q)", err, out.String())
	}
	if len(page) != 2 {
		t.Fatalf("page = %v, want exactly the 2 envelope keys — since must be decoded but never re-emitted on the output side", page)
	}
	if _, ok := page["since"]; ok {
		t.Fatalf("page = %v, since leaked into the output envelope", page)
	}
}

// TestJobsListReportsProvenTruncation pins the ADR-0007 contract: `truncated`
// comes from the daemon, which reached one row past the limit to answer it, so a
// full page is no longer assumed to be truncated. agentjson.Capped could not
// express the second case — it infers truncation from a full page and so lies
// about an exactly-full final page.
func TestJobsListReportsProvenTruncation(t *testing.T) {
	rows := make([]job.Row, job.ListLimitDefault)
	for i := range rows {
		rows[i] = job.Row{ID: fmt.Sprintf("job_%03d", i), State: job.StateQueued}
	}
	for _, daemonSaysTruncated := range []bool{true, false} {
		t.Run(fmt.Sprintf("truncated_%t", daemonSaysTruncated), func(t *testing.T) {
			var out, errOut bytes.Buffer
			var gotLimit int
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
				if method != "jobs.list_v3" {
					t.Fatalf("method = %q, want jobs.list_v3", method)
				}
				gotLimit = params.(map[string]any)["limit"].(int)
				page := api.JobsPageV3{Jobs: make([]api.JobRow, 0, len(rows)), Truncated: daemonSaysTruncated}
				for _, row := range rows {
					page.Jobs = append(page.Jobs, api.JobRow{Row: row})
				}
				*result.(*api.JobsPageV3) = page
				return nil
			})
			root.SetArgs([]string{"--json", "jobs", "list", "--limit", "5000"})
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("jobs list --limit 5000: %v (%s)", err, errOut.String())
			}
			if gotLimit != job.ListLimitMax {
				t.Fatalf("limit param = %d, want the daemon-effective %d: an over-large limit clamps DOWN to the maximum, it does not reset to the default", gotLimit, job.ListLimitMax)
			}
			var page struct {
				Jobs      []job.Row `json:"jobs"`
				Truncated bool      `json:"truncated"`
			}
			if err := json.Unmarshal(out.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if page.Truncated != daemonSaysTruncated {
				t.Fatalf("truncated = %t with a full page of %d rows, want the daemon's proof %t",
					page.Truncated, len(page.Jobs), daemonSaysTruncated)
			}
		})
	}
}

// An older daemon predates jobs.list_v3 and jobs.list_v2. The CLI must walk the
// chain down rather than fail, degrading to Capped's "there may be more" — the
// remedy is the same. Attribution is simply absent on the older rows, which is
// the truth: a daemon without the column recorded none.
func TestJobsListFallsBackToOlderDaemonMethods(t *testing.T) {
	rows := make([]job.Row, job.ListLimitDefault)
	for i := range rows {
		rows[i] = job.Row{ID: fmt.Sprintf("job_%03d", i), State: job.StateQueued}
	}
	var out, errOut bytes.Buffer
	var sawV3, sawV2, sawV1 bool
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.list_v3":
			sawV3 = true
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		case "jobs.list_v2":
			sawV2 = true
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		case "jobs.list":
			sawV1 = true
			*result.(*[]job.Row) = rows
			return nil
		}
		t.Fatalf("unexpected method %q", method)
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs list: %v (%s)", err, errOut.String())
	}
	if !sawV3 || !sawV2 || !sawV1 {
		t.Fatalf("v3 attempted = %t, v2 attempted = %t, v1 fallback used = %t; want all three", sawV3, sawV2, sawV1)
	}
	var page struct {
		Jobs      []job.Row `json:"jobs"`
		Truncated bool      `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != len(rows) || !page.Truncated {
		t.Fatalf("fallback page = %d rows truncated %t, want %d and the conservative true", len(page.Jobs), page.Truncated, len(rows))
	}
}

// jobs.get_v3 is `jobs get`'s ADR-0017 Decision 5 addition: a delivery
// section on top of jobs.get_v2's attribution. An older daemon that predates
// it answers unknown_method, and the CLI must fall back to jobs.get_v2 rather
// than fail — the fallback simply renders no delivery section, which is the
// truth: a daemon without the table recorded nothing.
func TestJobsGetFallsBackToOlderDaemonMethodWhenV3Unknown(t *testing.T) {
	detail := api.JobDetailV2{
		Job: &api.JobRow{Row: job.Row{ID: "job_fallback", State: job.StateReady, Work: work.Work{Title: "Fallback title"}}},
	}
	var out, errOut bytes.Buffer
	var sawV3, sawV2 bool
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.get_v3":
			sawV3 = true
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		case "jobs.get_v2":
			sawV2 = true
			*result.(*api.JobDetailV2) = detail
			return nil
		}
		t.Fatalf("unexpected method %q", method)
		return nil
	})
	root.SetArgs([]string{"jobs", "get", "job_fallback"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs get: %v (%s)", err, errOut.String())
	}
	if !sawV3 || !sawV2 {
		t.Fatalf("v3 attempted = %t, v2 fallback used = %t; want both", sawV3, sawV2)
	}
	want := "job_fallback\tready\ttitle:Fallback title\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q — the v2 fallback must render with no delivery section", out.String(), want)
	}
}

// jobs.get_v3's delivery section renders as labeled human-output lines when
// present (ADR-0017 Decision 5), and is entirely absent from the fallback
// case above.
func TestJobsGetRendersDeliverySection(t *testing.T) {
	detail := api.JobDetailV3{
		Job: &api.JobRow{Row: job.Row{ID: "job_delivery", State: job.StateRetryWait, Work: work.Work{Title: "Delivery title"}}},
		Delivery: &api.DeliverySummary{
			Provider: "illiad", Reference: "REF-9", State: "submitted",
			NextCheckAt: "2026-08-08T00:00:00Z", GateClass: "prefill_only",
			GateBlockers: []string{"api_credential_missing"},
		},
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "jobs.get_v3" {
			t.Fatalf("method = %q, want jobs.get_v3", method)
		}
		*result.(*api.JobDetailV3) = detail
		return nil
	})
	root.SetArgs([]string{"jobs", "get", "job_delivery"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs get: %v (%s)", err, errOut.String())
	}
	want := "job_delivery\tretry_wait\ttitle:Delivery title\n" +
		"  delivery provider: illiad\n" +
		"  delivery reference: REF-9\n" +
		"  delivery state: submitted\n" +
		"  delivery next check: 2026-08-08T00:00:00Z\n" +
		"  delivery gate: prefill_only (blocked by: api_credential_missing)\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

// row.Work.Describe() falls back to a raw Title when no strong identifier
// narrows it further, and that Title is third-party bibliographic metadata
// normalized with only strings.TrimSpace by internal/discovery (see the
// search-row comment in search.go). Before this fix, the text-mode `jobs
// list` row printed it straight to the terminal.
func TestJobsListStripsTerminalControlBytes(t *testing.T) {
	rows := []api.JobRow{
		{Row: job.Row{ID: "job-1", State: job.StateQueued, Work: work.Work{Title: "Evil\x1b]0;pwned\x07 Title\u009b31m"}}},
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		// jobs.list_v3 is tried first; the jobs.list_v2 fallback is reserved
		// for a daemon that predates attribution.
		if method != "jobs.list_v3" {
			t.Fatalf("method = %q, want jobs.list_v3", method)
		}
		*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows}
		return nil
	})
	root.SetArgs([]string{"jobs", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs list: %v (%s)", err, errOut.String())
	}
	got := out.String()
	want := "job-1\tqueued\ttitle:Evil]0;pwned Title31m\n"
	if got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, r := range got {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Errorf("control byte %#U survived in %q", r, got)
		}
	}
}

// TestActionsOpenFiltersJobsByStateAndFoldsDroppedRowsIntoTruncated pins two
// parts of the "actions open" completeness fix: the jobs.list join request
// is filtered to state=awaiting_human (strictly narrower than the old
// unfiltered newest-500, so an old awaiting_human job stops falling out of
// the cap), and an open action whose job id is missing from that page — the
// join's own omission — folds into `truncated`, rather than silently
// vanishing behind `truncated:false`.
func TestActionsOpenFiltersJobsByStateAndFoldsDroppedRowsIntoTruncated(t *testing.T) {
	action := job.HumanAction{
		ID: 1, JobID: "old_job", Kind: "openurl_handoff", Status: "open",
		Detail: app.OABrowserHandoffActionDetail("https://oa.example.test/old.pdf"),
	}
	var out, errOut bytes.Buffer
	var gotJobsListParams map[string]any
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		switch method {
		case "actions.list":
			*result.(*[]job.HumanAction) = []job.HumanAction{action}
			return nil
		case "jobs.list_v2":
			gotJobsListParams = params.(map[string]any)
			// old_job's action is still "open", but its job row is absent
			// from this state-filtered page — exactly the omission
			// actionURLs must now surface via droppedForMissingJob.
			*result.(*api.JobsPage) = api.JobsPage{}
			return nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil
		}
	})
	root.SetArgs([]string{"--json", "actions", "open", "--dry-run"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open --dry-run: %v (%s)", err, errOut.String())
	}
	if !reflect.DeepEqual(gotJobsListParams, map[string]any{"state": job.StateAwaitingHuman, "limit": job.ListLimitMax}) {
		t.Fatalf("jobs.list_v2 params = %#v, want state=%q limit=%d", gotJobsListParams, job.StateAwaitingHuman, job.ListLimitMax)
	}
	var page struct {
		URLs      []string `json:"urls"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.URLs) != 0 {
		t.Fatalf("urls = %v, want none — the action's job row was omitted from the state-filtered page", page.URLs)
	}
	if !page.Truncated {
		t.Fatal("truncated = false despite an open action whose job row was omitted from the state-filtered jobs.list page — want true")
	}
}

// TestFocusReportsHandoffsTheDaemonWouldNotOpen covers the gap left when
// FocusHandoffs became honest about its queued count. The daemon skips a parked
// job whose access mode cannot be expressed as a handoff offer; the CLI used to
// branch only on SessionLive, so those jobs produced no frame, no browser tab,
// and no output — papio reporting success for something it had not done.
//
// The fallback must not open them either: they were skipped because their mode
// forbids institutional access, so launching the URL would defeat the ceiling.
func TestFocusReportsHandoffsTheDaemonWouldNotOpen(t *testing.T) {
	var opened []string
	var out bytes.Buffer
	focus := func(context.Context, []string) (api.ActionsOpenResult, error) {
		return api.ActionsOpenResult{Queued: 1, SessionLive: true}, nil
	}
	run := func(_ context.Context, _ string, args ...string) error {
		opened = append(opened, args[len(args)-1])
		return nil
	}
	err := focusOrOpenActionURLs(context.Background(),
		[]string{"https://a.test", "https://b.test"}, nil,
		[]string{"job_aaaaaaaaaaaaaaaaaaaaaaaaaa", "job_bbbbbbbbbbbbbbbbbbbbbbbbbb"},
		false, &out, focus, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("opened %v; a skipped handoff must not be launched in the OS browser", opened)
	}
	if !strings.Contains(out.String(), "1 of 2 handoffs were not opened") {
		t.Fatalf("output %q does not report the skipped handoff", out.String())
	}
}
