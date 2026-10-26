// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"net/url"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func TestPublisherHandoffUsesOnlyJobDOI(t *testing.T) {
	for _, doi := range []string{"10.1000/example", "10.48612//monograph-2025-2", "10.1000/foo?bar#baz"} {
		action := job.HumanAction{Kind: "openurl_handoff", Detail: job.PublisherHandoffDetail}
		row := job.Row{Work: work.Work{DOI: doi}}
		target, ok := ResolveHumanActionURL(action, row, func(string) (config.Institution, bool) {
			// The resolver base is present and must not leak into the DOI route.
			return config.Institution{OpenURLBase: "https://resolver.example.edu/openurl"}, true
		})
		u, err := url.Parse(target)
		if !ok || err != nil || u.Host != "doi.org" || u.Scheme != "https" || u.Path != "/"+doi || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("DOI=%q target=%q ok=%v err=%v", doi, target, ok, err)
		}
	}
	if target, ok := PublisherHandoffURL(job.HumanAction{Detail: job.PublisherHandoffDetail + "\nhttps://evil.example"}, job.Row{Work: work.Work{DOI: "10.1000/example"}}, config.Institution{}); !ok || target != "https://doi.org/10.1000/example" {
		t.Fatal("diagnostic text changed the publisher route")
	}
	if _, ok := ResolveHumanActionURL(job.HumanAction{Detail: job.PublisherHandoffDetail}, job.Row{}, func(string) (config.Institution, bool) {
		return config.Institution{OpenURLBase: "https://resolver.example.edu/openurl", OpenAthensDomain: "example.edu"}, true
	}); ok {
		t.Fatal("accepted missing DOI")
	}
}

func TestPublisherHandoffURLRefusesDotSegmentDOIs(t *testing.T) {
	// Browsers follow the WHATWG URL Standard, which resolves "." and ".."
	// segments — including their percent-encoded spellings — so no link may
	// be built: the handoff would open a different registered DOI. The
	// refusal takes the same path as a missing DOI: no URL anywhere the
	// action resolves, so every caller takes its existing no-handoff path.
	dotty := []string{
		"10.1234/../10.5555/x",
		"10.1234/./x",
		"10.1234/%2E%2E/10.5555/x",
		"10.1234/.%2E/x",
	}
	for _, doi := range dotty {
		action := job.HumanAction{Kind: "openurl_handoff", Detail: job.PublisherHandoffDetail}
		row := job.Row{Work: work.Work{DOI: doi}}
		if target, ok := PublisherHandoffURL(action, row, config.Institution{}); ok || target != "" {
			t.Fatalf("DOI=%q target=%q, %v; want no publisher URL", doi, target, ok)
		}
		// The OpenAthens redirector must not wrap it either: it would
		// resolve the same way after the redirect.
		if target, ok := PublisherHandoffURL(action, row, config.Institution{OpenAthensDomain: "example.edu"}); ok || target != "" {
			t.Fatalf("DOI=%q redirector target=%q, %v; want none", doi, target, ok)
		}
		if got := LibKeyURL(config.Institution{LibKeyMode: "link", LibKeyLibraryID: 7}, row.Work); got != "" {
			t.Fatalf("DOI=%q LibKey URL=%q; want none", doi, got)
		}
		// `papio actions open` then takes its skipped-handoff path instead
		// of launching the wrong DOI in the OS browser.
		if target, ok := ResolveHumanActionURL(action, row, func(string) (config.Institution, bool) {
			return config.Institution{OpenURLBase: "https://resolver.example.edu/openurl", OpenAthensDomain: "example.edu"}, true
		}); ok || target != "" {
			t.Fatalf("DOI=%q resolved URL=%q, %v; want the no-handoff path", doi, target, ok)
		}
	}
	// Normal DOIs — including slash runs — keep the exact URL as before.
	for _, doi := range []string{"10.1000/abc", "10.48612//monograph-2025-2"} {
		action := job.HumanAction{Kind: "openurl_handoff", Detail: job.PublisherHandoffDetail}
		row := job.Row{Work: work.Work{DOI: doi}}
		if target, ok := PublisherHandoffURL(action, row, config.Institution{}); !ok || target != "https://doi.org/"+doi {
			t.Fatalf("DOI=%q target=%q, %v; want the plain DOI link", doi, target, ok)
		}
		if got, want := LibKeyURL(config.Institution{LibKeyMode: "link", LibKeyLibraryID: 7}, row.Work), "https://libkey.io/libraries/7/"+doi; got != want {
			t.Fatalf("DOI=%q LibKey URL=%q; want %q", doi, got, want)
		}
	}
}
