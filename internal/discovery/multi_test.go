// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"papio/internal/work"
)

type fakeSource struct {
	name  string
	works []DiscoveredWork
	err   error
	calls int
}

func (s *fakeSource) Name() string {
	return s.name
}

func (s *fakeSource) Search(_ context.Context, _ SearchParams) ([]DiscoveredWork, error) {
	s.calls++
	return s.works, s.err
}

func TestMultiSearch(t *testing.T) {
	firstWorks := []DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/duplicate", Title: "First DOI copy"}},
		{Work: work.Work{Title: "A  title without DOI"}},
		{Work: work.Work{DOI: "10.1000/first-only", Title: "First only"}},
	}
	secondWorks := []DiscoveredWork{
		{Work: work.Work{DOI: "https://doi.org/10.1000/DUPLICATE", Title: "Second DOI copy"}},
		{Work: work.Work{Title: " a title WITHOUT doi "}},
		{Work: work.Work{DOI: "10.1000/second-only", Title: "Second only"}},
	}
	firstFailure := errors.New("first unavailable")
	secondFailure := errors.New("second unavailable")

	for _, test := range []struct {
		name       string
		params     SearchParams
		makeSource func() []*fakeSource
		wantTitles []string
		wantCalls  []int
		wantErr    []error
	}{
		{
			name:   "merges by DOI and title in preference order",
			params: SearchParams{Query: "test"},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", works: append([]DiscoveredWork(nil), firstWorks...)}, {name: "second", works: append([]DiscoveredWork(nil), secondWorks...)}}
			},
			wantTitles: []string{"First DOI copy", "A  title without DOI", "First only", "Second only"},
			wantCalls:  []int{1, 1},
		},
		{
			name:   "caps merged results at requested limit",
			params: SearchParams{Query: "test", Limit: 2},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", works: append([]DiscoveredWork(nil), firstWorks...)}, {name: "second", works: append([]DiscoveredWork(nil), secondWorks...)}}
			},
			wantTitles: []string{"First DOI copy", "A  title without DOI"},
			wantCalls:  []int{1, 1},
		},
		{
			name:   "continues after a source failure",
			params: SearchParams{Query: "test"},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", err: firstFailure}, {name: "second", works: []DiscoveredWork{{Work: work.Work{Title: "Available"}}}}}
			},
			wantTitles: []string{"Available"},
			wantCalls:  []int{1, 1},
		},
		{
			name:   "returns joined errors when every source fails",
			params: SearchParams{Query: "test"},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", err: firstFailure}, {name: "second", err: secondFailure}}
			},
			wantCalls: []int{1, 1},
			wantErr:   []error{firstFailure, secondFailure},
		},
		{
			name:   "routes to exactly the requested source",
			params: SearchParams{Query: "test", Source: "second"},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", works: firstWorks}, {name: "second", works: []DiscoveredWork{{Work: work.Work{Title: "Selected"}}}}}
			},
			wantTitles: []string{"Selected"},
			wantCalls:  []int{0, 1},
		},
		{
			name:   "rejects unknown source",
			params: SearchParams{Query: "test", Source: "missing"},
			makeSource: func() []*fakeSource {
				return []*fakeSource{{name: "first", works: firstWorks}, {name: "second", works: secondWorks}}
			},
			wantCalls: []int{0, 0},
			wantErr:   []error{errors.New(`unknown discovery source "missing"`)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := test.makeSource()
			actualSources := make([]Source, len(sources))
			for i, source := range sources {
				actualSources[i] = source
			}
			works, err := NewMulti(actualSources...).Search(context.Background(), test.params)
			if len(test.wantErr) == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("Search succeeded, want error")
				}
				for _, want := range test.wantErr {
					if !strings.Contains(err.Error(), want.Error()) {
						t.Fatalf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			if len(works) != len(test.wantTitles) {
				t.Fatalf("works = %+v, want %d results", works, len(test.wantTitles))
			}
			for i, want := range test.wantTitles {
				if works[i].Work.Title != want {
					t.Fatalf("works[%d].Title = %q, want %q", i, works[i].Work.Title, want)
				}
			}
			for i, source := range sources {
				if source.calls != test.wantCalls[i] {
					t.Fatalf("source %q calls = %d, want %d", source.name, source.calls, test.wantCalls[i])
				}
			}
		})
	}
}

func TestMultiAddsSourceProvenance(t *testing.T) {
	source := &fakeSource{name: "only", works: []DiscoveredWork{{Work: work.Work{Title: "One"}}}}
	works, err := NewMulti(source).Search(context.Background(), SearchParams{Query: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 || works[0].Source != "only" {
		t.Fatalf("works = %+v", works)
	}
}

func TestMultiSearchUsesDefaultAggregateLimit(t *testing.T) {
	makeWorks := func(source string) []DiscoveredWork {
		works := make([]DiscoveredWork, 0, 15)
		for i := range 15 {
			works = append(works, DiscoveredWork{Work: work.Work{Title: fmt.Sprintf("%s %d", source, i)}})
		}
		return works
	}

	works, err := NewMulti(
		&fakeSource{name: "first", works: makeWorks("first")},
		&fakeSource{name: "second", works: makeWorks("second")},
	).Search(context.Background(), SearchParams{Query: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != defaultLimit {
		t.Fatalf("works = %d, want default limit %d", len(works), defaultLimit)
	}
	if works[len(works)-1].Work.Title != "second 4" {
		t.Fatalf("last work = %q, want %q", works[len(works)-1].Work.Title, "second 4")
	}
}

func TestMultiSearchKeepsWorksWithoutIdentity(t *testing.T) {
	works, err := NewMulti(
		&fakeSource{name: "first", works: []DiscoveredWork{{Work: work.Work{Year: 2024}}}},
		&fakeSource{name: "second", works: []DiscoveredWork{{Work: work.Work{Year: 2025}}}},
	).Search(context.Background(), SearchParams{Query: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 2 {
		t.Fatalf("works = %+v, want both empty-identity works", works)
	}
	if works[0].Source != "first" || works[1].Source != "second" {
		t.Fatalf("sources = %q, %q, want first, second", works[0].Source, works[1].Source)
	}
}
