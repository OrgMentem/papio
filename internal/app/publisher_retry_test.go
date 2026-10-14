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
