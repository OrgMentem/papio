// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

// TestAcquireSendsConsumerAttribution pins that --consumer reaches the daemon,
// and that it goes to acquire.submit_v3 rather than the ratified v2 whose params
// ADR-0010 froze. Without attribution there is no way to partition a shared
// daemon's totals between the people using it; without the method split, adding
// it breaks every consumer pinned to v2's param set.
func TestAcquireSendsConsumerAttribution(t *testing.T) {
	var out, errOut bytes.Buffer
	var got acquireSubmitV3Params
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, params any, result any) error {
			if method != "acquire.submit_v3" {
				t.Fatalf("method = %q, want acquire.submit_v3: attribution must not widen the ratified v2 params", method)
			}
			got = params.(acquireSubmitV3Params)
			*result.(*api.SubmitV2Result) = api.SubmitV2Result{JobID: "job_consumer_01"}
			return nil
		})
	root.SetArgs([]string{"acquire", "--doi", "10.1000/attributed", "--consumer", "inscribi"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("acquire --consumer: %v (%s)", err, errOut.String())
	}
	if got.Consumer != "inscribi" {
		t.Fatalf("submitted consumer = %q, want inscribi", got.Consumer)
	}
}

func TestAcquireConsumerAttributionSerializesWireKey(t *testing.T) {
	var out, errOut bytes.Buffer
	var encoded []byte
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, params any, result any) error {
			if method != "acquire.submit_v3" {
				t.Fatalf("method = %q, want acquire.submit_v3", method)
			}
			var err error
			if encoded, err = json.Marshal(params); err != nil {
				t.Fatalf("marshal acquire params: %v", err)
			}
			*result.(*api.SubmitV2Result) = api.SubmitV2Result{JobID: "job_consumer_wire"}
			return nil
		})
	root.SetArgs([]string{"acquire", "--doi", "10.1000/attributed-wire", "--consumer", "inscribi"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("acquire --consumer: %v (%s)", err, errOut.String())
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatalf("decode acquire params: %v", err)
	}
	raw, ok := sent["consumer"]
	if !ok {
		t.Fatalf("acquire params omitted consumer: %s", encoded)
	}
	var consumer string
	if err := json.Unmarshal(raw, &consumer); err != nil {
		t.Fatalf("decode consumer wire value: %v (%s)", err, encoded)
	}
	if consumer != "inscribi" {
		t.Fatalf("wire consumer = %q, want inscribi", consumer)
	}
	if _, ok := sent["consumers"]; ok {
		t.Fatalf("wire params used misspelled consumers key: %s", encoded)
	}
}

func TestConsumerFiltersSerializeConsumerParam(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		method string
		set    func(any)
	}{
		{
			name:   "jobs",
			args:   []string{"jobs", "list", "--consumer", "inscribi"},
			method: "jobs.list_v3",
			set:    func(result any) { *result.(*api.JobsPageV3) = api.JobsPageV3{} },
		},
		{
			name:   "actions",
			args:   []string{"actions", "list", "--consumer", "inscribi"},
			method: "actions.list_v3",
			set:    func(result any) { *result.(*api.ActionsPageV3) = api.ActionsPageV3{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var encoded []byte
			root := NewInProcessRoot(&out, &errOut, config.Config{},
				func(_ context.Context, method string, params any, result any) error {
					if method != tc.method {
						t.Fatalf("method = %q, want %s", method, tc.method)
					}
					var err error
					if encoded, err = json.Marshal(params); err != nil {
						t.Fatalf("marshal %s params: %v", tc.name, err)
					}
					tc.set(result)
					return nil
				})
			root.SetArgs(tc.args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%s: %v (%s)", tc.name, err, errOut.String())
			}
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &sent); err != nil {
				t.Fatalf("decode %s params: %v", tc.name, err)
			}
			var consumer string
			raw, ok := sent["consumer"]
			if !ok {
				t.Fatalf("%s params omitted consumer: %s", tc.name, encoded)
			}
			if err := json.Unmarshal(raw, &consumer); err != nil {
				t.Fatalf("decode %s consumer: %v", tc.name, err)
			}
			if consumer != "inscribi" {
				t.Fatalf("%s consumer = %q, want inscribi", tc.name, consumer)
			}
		})
	}
}

// TestAcquireWithoutConsumerStaysOnRatifiedSubmitV2: the ordinary path must not
// move to a newer method just because one exists, or every unattributed
// submission gains a failure mode against an older daemon for nothing.
func TestAcquireWithoutConsumerStaysOnRatifiedSubmitV2(t *testing.T) {
	var out, errOut bytes.Buffer
	var seen string
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, _ any, result any) error {
			seen = method
			*result.(*api.SubmitV2Result) = api.SubmitV2Result{JobID: "job_plain_01"}
			return nil
		})
	root.SetArgs([]string{"acquire", "--doi", "10.1000/plain"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("acquire: %v (%s)", err, errOut.String())
	}
	if seen != "acquire.submit_v2" {
		t.Fatalf("method = %q, want the ratified acquire.submit_v2", seen)
	}
}

// TestAcquireConsumerRefusesOlderDaemon: unknown_method on submit_v3 must be
// reported, never retried against v2 without the attribution — recording the work
// as nobody's is worse than failing.
func TestAcquireConsumerRefusesOlderDaemon(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, _ any, _ any) error {
			if method != "acquire.submit_v3" {
				t.Fatalf("CLI fell back to %q and dropped the attribution", method)
			}
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		})
	root.SetArgs([]string{"acquire", "--doi", "10.1000/older", "--consumer", "inscribi"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "acquire.submit_v3") {
		t.Fatalf("error = %v, want a refusal naming acquire.submit_v3", err)
	}
}

// TestAcquireOmitsConsumerWhenUnset keeps absence honest on the wire: an
// unattributed submission must send no consumer at all rather than "".
func TestAcquireOmitsConsumerWhenUnset(t *testing.T) {
	var out, errOut bytes.Buffer
	var encoded []byte
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, params any, result any) error {
			if method != "acquire.submit_v2" {
				t.Fatalf("unexpected method %q", method)
			}
			var err error
			if encoded, err = json.Marshal(params); err != nil {
				t.Fatal(err)
			}
			*result.(*api.SubmitV2Result) = api.SubmitV2Result{JobID: "job_consumer_02"}
			return nil
		})
	root.SetArgs([]string{"acquire", "--doi", "10.1000/unattributed"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("acquire: %v (%s)", err, errOut.String())
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["consumer"]; ok {
		t.Fatalf("params carry a consumer key with no --consumer: %s", encoded)
	}
}

// TestConsumerFilterRefusesOlderDaemon: a filter the daemon cannot apply must
// fail, never silently return every consumer's rows. Returning the unfiltered
// list would be a wrong answer that gets believed.
func TestConsumerFilterRefusesOlderDaemon(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		older   []string
		wantErr string
	}{
		{"jobs list", []string{"jobs", "list", "--consumer", "inscribi"}, []string{"jobs.list_v3"}, "jobs.list_v3"},
		{"actions list", []string{"actions", "list", "--consumer", "inscribi"}, []string{"actions.list_v3"}, "actions.list_v3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
				for _, unknown := range tc.older {
					if method == unknown {
						return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
					}
				}
				t.Fatalf("CLI fell back to %q while a consumer filter was requested", method)
				return nil
			})
			root.SetArgs(tc.args)
			err := root.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want a refusal naming %s", err, tc.wantErr)
			}
		})
	}
}

// TestActionsListReportsStaleRows pins that the daemon's staleness verdict
// reaches both surfaces: the JSON row keys a consumer reads, and the human
// listing where a weeks-old handoff was previously indistinguishable from one
// queued this morning.
func TestActionsListReportsStaleRows(t *testing.T) {
	rows := []api.ActionRow{
		{
			HumanAction: job.HumanAction{ID: 2, JobID: "job_stale", Kind: "openurl_handoff", Status: "open", CreatedAt: "2026-07-01T00:00:00Z"},
			AgeSeconds:  33 * 24 * 60 * 60,
			Stale:       true,
		},
		{
			HumanAction: job.HumanAction{ID: 1, JobID: "job_fresh", Kind: "openurl_handoff", Status: "open", CreatedAt: "2026-08-03T00:00:00Z"},
			AgeSeconds:  120,
		},
	}
	newRoot := func(out, errOut *bytes.Buffer) *cobra.Command {
		return NewInProcessRoot(out, errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
			if method != "actions.list_v3" {
				t.Fatalf("unexpected method %q", method)
			}
			*result.(*api.ActionsPageV3) = api.ActionsPageV3{Actions: rows}
			return nil
		})
	}

	var jsonOut, jsonErr bytes.Buffer
	root := newRoot(&jsonOut, &jsonErr)
	root.SetArgs([]string{"--json", "actions", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions list --json: %v (%s)", err, jsonErr.String())
	}
	var page struct {
		Actions []struct {
			ID         int64 `json:"id"`
			AgeSeconds int64 `json:"age_seconds"`
			Stale      bool  `json:"stale"`
		} `json:"actions"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%q)", err, jsonOut.String())
	}
	if len(page.Actions) != 2 {
		t.Fatalf("rows = %+v", page.Actions)
	}
	if !page.Actions[0].Stale || page.Actions[0].AgeSeconds == 0 {
		t.Fatalf("stale row = %+v, want stale with a nonzero age", page.Actions[0])
	}
	if page.Actions[1].Stale {
		t.Fatalf("fresh row = %+v, want stale false", page.Actions[1])
	}

	var textOut, textErr bytes.Buffer
	root = newRoot(&textOut, &textErr)
	root.SetArgs([]string{"actions", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions list: %v (%s)", err, textErr.String())
	}
	lines := strings.Split(strings.TrimSpace(textOut.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("text lines = %q", lines)
	}
	if !strings.Contains(lines[0], "stale: waiting 33d") {
		t.Fatalf("stale line = %q, want it to name how long it has waited", lines[0])
	}
	if strings.Contains(lines[1], "stale") {
		t.Fatalf("fresh line = %q, must not be marked stale", lines[1])
	}
}

// TestListingsStateTruncationOnTheTextSurface closes the other half of the
// truncation contract: --json has carried a proven `truncated` since ADR-0007,
// while the human listing stopped at the limit and looked complete.
func TestListingsStateTruncationOnTheTextSurface(t *testing.T) {
	t.Run("jobs", func(t *testing.T) {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
			if method != "jobs.list_v3" {
				t.Fatalf("unexpected method %q", method)
			}
			*result.(*api.JobsPageV3) = api.JobsPageV3{
				Jobs:      []api.JobRow{{Row: job.Row{ID: "job_01", State: job.StateQueued}}},
				Truncated: true,
			}
			return nil
		})
		root.SetArgs([]string{"jobs", "list", "--limit", "1"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("jobs list: %v (%s)", err, errOut.String())
		}
		if !strings.Contains(out.String(), "more exist behind this page") {
			t.Fatalf("output = %q, want a stated truncation", out.String())
		}
	})
	t.Run("actions", func(t *testing.T) {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
			if method != "actions.list_v3" {
				t.Fatalf("unexpected method %q", method)
			}
			*result.(*api.ActionsPageV3) = api.ActionsPageV3{
				Actions:   []api.ActionRow{{HumanAction: job.HumanAction{ID: 1, JobID: "job_01", Kind: "openurl_handoff", Status: "open"}}},
				Truncated: true,
			}
			return nil
		})
		root.SetArgs([]string{"actions", "list", "--limit", "1"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("actions list: %v (%s)", err, errOut.String())
		}
		if !strings.Contains(out.String(), "more exist behind this page") {
			t.Fatalf("output = %q, want a stated truncation", out.String())
		}
	})
	t.Run("json output stays exactly two keys", func(t *testing.T) {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
			*result.(*api.JobsPageV3) = api.JobsPageV3{Truncated: true}
			return nil
		})
		root.SetArgs([]string{"--json", "jobs", "list"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("jobs list --json: %v (%s)", err, errOut.String())
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v (%q)", err, out.String())
		}
		if len(page) != 2 {
			t.Fatalf("page keys = %v, want exactly the two envelope keys — the notice is a human surface only", page)
		}
	})
}

// TestBatchRefusesRequestIDWithTheRealMechanism: the flag used to be refused
// with advice ("put per-work values in JSONL") that could not work — the JSONL
// work decoder is strict and has no request_id field — so the caller was left
// with no option at all.
func TestBatchRefusesRequestIDWithTheRealMechanism(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{AccessMode: config.ModeConservative},
		func(_ context.Context, method string, _ any, _ any) error {
			t.Fatalf("rejected batch reached the daemon (%q)", method)
			return nil
		})
	root.SetArgs([]string{"acquire", "--batch", "-", "--request-id", "wr_manual"})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("--batch --request-id succeeded; it cannot, one key cannot name many works")
	}
	if strings.Contains(err.Error(), "put per-work values in JSONL") {
		t.Fatalf("error still promises a JSONL request_id field that the strict work decoder rejects: %v", err)
	}
	for _, want := range []string{"single work", "deterministic per-work request ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name the real mechanism (%q)", err, want)
		}
	}
}

// TestBatchWorkDecoderStillRejectsRequestID is the other half of the pair above:
// it pins WHY the old advice was wrong, so nobody restores it without also
// giving JSONL a request_id field.
func TestBatchWorkDecoderStillRejectsRequestID(t *testing.T) {
	_, err := parseBatch(strings.NewReader(`{"doi":"10.1000/x","request_id":"wr_manual"}`))
	if err == nil {
		t.Fatal("JSONL accepted request_id; if that is now intended, batch.NewManifest must stop overwriting it and the flag refusal must be revisited")
	}
	if !strings.Contains(err.Error(), "request_id") {
		t.Fatalf("error = %v, want it to name the unknown field", err)
	}
}

// TestArtifactsValidationPrintsTheFullDocument: `artifacts get` returns the
// shared artifact row, which is all it can return; the per-job evidence a
// consumer needs to justify a rights or quality decision arrives here, whole.
func TestArtifactsValidationPrintsTheFullDocument(t *testing.T) {
	const document = `{"schema_version":"validation-report/1","payload":{"ok":true,"size_bytes":9,"has_header":true,"has_eof":true},` +
		`"structural":{"valid":true,"pages":12,"encrypted":false,"has_javascript":false,"has_embedded_files":false},` +
		`"text":{"chars":4096,"ocr_used":false,"needs_review":false},"identity":{"result":"pass","evidence":["title matched"]}}`
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "artifacts.validation" {
			t.Fatalf("unexpected method %q", method)
		}
		*result.(*api.ValidationResult) = api.ValidationResult{
			JobID: "job_validated",
			Reports: []api.ValidationReport{{
				CandidateID: 7, SHA256: "abc", Outcome: "pass", RecordedAt: "2026-08-01T00:00:00Z",
				Accepted: true, SchemaVersion: "validation-report/1", Document: document,
			}},
		}
		return nil
	})
	root.SetArgs([]string{"artifacts", "validation", "job_validated"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("artifacts validation: %v (%s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, document) {
		t.Fatalf("output = %q, want the complete report document, not a summary of it", got)
	}
	for _, want := range []string{"candidate 7", "pass", "accepted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

// TestArtifactsValidationNamesAnAbsenceHonestly: a job validated before papio
// recorded evidence has none, and no report can be reconstructed for it. Saying
// so beats printing an empty list that reads like a clean bill of health.
func TestArtifactsValidationNamesAnAbsenceHonestly(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.ValidationResult) = api.ValidationResult{JobID: "job_old", Reports: []api.ValidationReport{}}
		return nil
	})
	root.SetArgs([]string{"artifacts", "validation", "job_old"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("artifacts validation: %v (%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "no validation evidence recorded") {
		t.Fatalf("output = %q, want the absence named", out.String())
	}
}

// TestActionsOpenRefusesAnAmbiguousJobSelector: a job may hold open actions of
// different kinds at once, and "open one of them" would make which one an
// accident of row order. The selector names a row; when a job names several, the
// caller has to say which.
func TestActionsOpenRefusesAnAmbiguousJobSelector(t *testing.T) {
	target := app.OABrowserHandoffActionDetail("https://oa.example.test/ambiguous.pdf")
	actions := []job.HumanAction{
		{ID: 9, JobID: "job_two", Kind: "openurl_handoff", Status: "open", Detail: target},
		{ID: 8, JobID: "job_two", Kind: "manual_download", Status: "open", Detail: target},
	}
	rows := []job.Row{{ID: "job_two", State: job.StateAwaitingHuman}}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "actions.list":
			*result.(*[]job.HumanAction) = actions
		case "jobs.list_v2":
			*result.(*api.JobsPage) = api.JobsPage{Jobs: rows}
		default:
			t.Fatalf("unexpected method %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"actions", "open", "--dry-run", "--job", "job_two"})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("ambiguous --job succeeded and emitted %q; it must name the candidates instead", out.String())
	}
	for _, want := range []string{"2 open actions", "--action", "9 (openurl_handoff)", "8 (manual_download)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}
