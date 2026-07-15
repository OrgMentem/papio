// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"papio/internal/app"
	"papio/internal/browser"
	"papio/internal/job"
	"papio/internal/work"
)

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
	if got := actionURLs(actions, rows, baseFor, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs = %#v, want %#v", got, want)
	}
	if got := actionURLs(actions, rows, baseFor, 1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("limited URLs = %#v, want %#v", got, want[:1])
	}

	var out bytes.Buffer
	if err := openActionURLs(context.Background(), want, true, &out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != want[0]+"\n"+want[1]+"\n"+want[2]+"\n" {
		t.Fatalf("dry-run output = %q", got)
	}
}
