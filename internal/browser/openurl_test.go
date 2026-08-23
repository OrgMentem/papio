// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"testing"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/work"
)

func TestLibKeyURLBuildsTheDocumentedInstitutionLink(t *testing.T) {
	linked := config.Institution{
		OpenURLBase: "https://resolver.example.edu/openurl",
		LibKeyMode:  "link", LibKeyLibraryID: 1234,
	}
	for _, test := range []struct {
		name string
		inst config.Institution
		w    work.Work
		want string
	}{
		{
			name: "DOI keeps its slashes in the path",
			inst: linked,
			w:    work.Work{DOI: "10.1002/mar.21498"},
			want: "https://libkey.io/libraries/1234/10.1002/mar.21498",
		},
		{
			name: "PMID when no DOI",
			inst: linked,
			w:    work.Work{PMID: "35051190"},
			want: "https://libkey.io/libraries/1234/35051190",
		},
		{
			name: "DOI wins over PMID",
			inst: linked,
			w:    work.Work{DOI: "10.1002/mar.21498", PMID: "35051190"},
			want: "https://libkey.io/libraries/1234/10.1002/mar.21498",
		},
		{
			name: "ISBN-only work has no LibKey route",
			inst: linked,
			w:    work.Work{ISBN: "9780306406157", Title: "A book"},
			want: "",
		},
		{
			name: "mode off yields nothing even with an id",
			inst: config.Institution{OpenURLBase: "https://resolver.example.edu/openurl", LibKeyMode: "off", LibKeyLibraryID: 1234},
			w:    work.Work{DOI: "10.1002/mar.21498"},
			want: "",
		},
		{
			name: "link mode without an id yields nothing",
			inst: config.Institution{OpenURLBase: "https://resolver.example.edu/openurl", LibKeyMode: "link"},
			w:    work.Work{DOI: "10.1002/mar.21498"},
			want: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := LibKeyURL(test.inst, test.w); got != test.want {
				t.Fatalf("LibKeyURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRouteURLFallsBackToOpenURL(t *testing.T) {
	inst := config.Institution{
		OpenURLBase: "https://resolver.example.edu/openurl",
		LibKeyMode:  "link", LibKeyLibraryID: 1234,
	}
	// A DOI work routes through LibKey…
	doiWork := work.Work{DOI: "10.1002/mar.21498"}
	if got := RouteURL(inst, doiWork); got != "https://libkey.io/libraries/1234/10.1002/mar.21498" {
		t.Fatalf("RouteURL(doi) = %q, want the LibKey link", got)
	}
	// …an ISBN-only work cannot, and must land on the plain resolver instead
	// of dead-ending: LibKey augments institutional routing, never replaces it.
	bookWork := work.Work{ISBN: "9780306406157", Title: "A book"}
	if got, want := RouteURL(inst, bookWork), app.OpenURL(inst.OpenURLBase, bookWork); got != want {
		t.Fatalf("RouteURL(book) = %q, want the OpenURL fallback %q", got, want)
	}
}
