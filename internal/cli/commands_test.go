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
			name:   "institutional",
			action: job.HumanAction{RequiresAuth: true, BlockedBy: "paywall"},
			want:   "\tsign in to your institution first, then 'papio actions open'",
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
	baseFor := func(name string) (string, bool) {
		switch name {
		case "", "default":
			return base, true
		case "institute":
			return instituteBase, true
		}
		return "", false
	}
	oaURL := "https://oa.example.test/paper.pdf"
	rows := []job.Row{
		{ID: "oa", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/oa"}},
		{ID: "institutional", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/institutional", Title: "Institutional"}},
		{ID: "review", State: job.StateNeedsReview, Work: work.Work{DOI: "10.1000/review"}},
		{ID: "manual", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/manual"}},
		{ID: "profiled", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "institute"}, Work: work.Work{DOI: "10.1000/profiled"}},
		{ID: "unknownprofile", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "gone"}, Work: work.Work{DOI: "10.1000/unknown"}},
	}
	actions := []job.HumanAction{
		{ID: 6, JobID: "profiled", Kind: "openurl_handoff", Status: "open"},
		{ID: 5, JobID: "unknownprofile", Kind: "openurl_handoff", Status: "open"},
		{ID: 4, JobID: "oa", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(oaURL)},
		{ID: 3, JobID: "institutional", Kind: "openurl_handoff", Status: "open"},
		{ID: 2, JobID: "review", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail("https://oa.example.test/review.pdf")},
		{ID: 1, JobID: "manual", Kind: "manual_download", Status: "open", Detail: "choose a file"},
	}

	want := []string{browser.OpenURL(instituteBase, rows[4].Work), oaURL, browser.OpenURL(base, rows[1].Work)}
	got, dropped := actionURLs(actions, rows, baseFor, 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs = %#v, want %#v", got, want)
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 — every action's job id is present in rows", dropped)
	}
	if limited, limitedDropped := actionURLs(actions, rows, baseFor, 1); !reflect.DeepEqual(limited, want[:1]) || limitedDropped != 0 {
		t.Fatalf("limited URLs = %#v, dropped = %d, want %#v, 0", limited, limitedDropped, want[:1])
	}

	var out bytes.Buffer
	if err := openActionURLs(context.Background(), want, true, &out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != want[0]+"\n"+want[1]+"\n"+want[2]+"\n" {
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

func TestCommandGroupsRejectUnknownVerbs(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
		t.Fatal("unknown command must not call the daemon")
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "show", "job_01"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("jobs show succeeded, want an unknown-verb error")
	}
	for _, fragment := range []string{`unknown jobs command "show"`, "valid verbs:", "get"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("jobs show error = %q, missing %q", err, fragment)
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

// TestJobsListLimitAboveDaemonMaxReportsHonestTruncation pins the
// truncated-honesty fix: internal/job.Store.List silently resets any --limit
// outside (0, ListLimitMax] to ListLimitDefault, so comparing the returned
// row count against the raw --limit flag (as agentjson.Capped used to be
// called) lies about whether more rows exist.
func TestJobsListLimitAboveDaemonMaxReportsHonestTruncation(t *testing.T) {
	rows := make([]job.Row, job.ListLimitDefault)
	for i := range rows {
		rows[i] = job.Row{ID: fmt.Sprintf("job_%03d", i), State: job.StateQueued}
	}
	var out, errOut bytes.Buffer
	var gotLimit int
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "jobs.list" {
			t.Fatalf("method = %q, want jobs.list", method)
		}
		gotLimit = params.(map[string]any)["limit"].(int)
		*result.(*[]job.Row) = rows
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "list", "--limit", "5000"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs list --limit 5000: %v (%s)", err, errOut.String())
	}
	if gotLimit != job.ListLimitDefault {
		t.Fatalf("jobs.list limit param = %d, want the daemon-effective %d (Store.List resets any out-of-range limit)", gotLimit, job.ListLimitDefault)
	}
	var page struct {
		Jobs      []job.Row `json:"jobs"`
		Truncated bool      `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Fatalf("truncated = false with %d rows at the daemon's own default cap %d — an out-of-range --limit must not silently report false completeness", len(page.Jobs), job.ListLimitDefault)
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
		case "jobs.list":
			gotJobsListParams = params.(map[string]any)
			// old_job's action is still "open", but its job row is absent
			// from this state-filtered page — exactly the omission
			// actionURLs must now surface via droppedForMissingJob.
			*result.(*[]job.Row) = nil
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
		t.Fatalf("jobs.list params = %#v, want state=%q limit=%d", gotJobsListParams, job.StateAwaitingHuman, job.ListLimitMax)
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
