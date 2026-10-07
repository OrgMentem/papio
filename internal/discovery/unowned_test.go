// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"papio/internal/work"
	"strconv"
	"testing"
)

type unownedFixture struct{ total, calls int }

func (f *unownedFixture) Name() string { return "fixture" }
func (f *unownedFixture) Search(context.Context, SearchParams) ([]DiscoveredWork, error) {
	return nil, errors.New("unpaged path must not run")
}
func (f *unownedFixture) SearchPage(_ context.Context, p SearchParams, token string) (Page, error) {
	f.calls++
	offset := 0
	if token != "" {
		var err error
		offset, err = strconv.Atoi(token)
		if err != nil {
			return Page{}, err
		}
	}
	end := min(offset+p.Limit, f.total)
	page := Page{State: PageExhausted, Works: []DiscoveredWork{}}
	for i := offset; i < end; i++ {
		page.Works = append(page.Works, DiscoveredWork{Work: work.Work{DOI: fmt.Sprintf("10.1234/%d", i), Title: fmt.Sprintf("Work %d", i)}})
	}
	if end < f.total {
		page.State = PageMore
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}
func TestUnownedSearchContinuesPastFiftyOwnedRows(t *testing.T) {
	source := &unownedFixture{total: 70}
	classifier := func(_ context.Context, rows []DiscoveredWork) error {
		for i := range rows {
			n, _ := strconv.Atoi(rows[i].Work.DOI[len("10.1234/"):])
			rows[i].Owned = n < 50
		}
		return nil
	}
	request := UnownedRequest{Search: SearchParams{Query: "fixture", Limit: 10}}
	result, err := SearchUnowned(context.Background(), source, request, classifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Works) != 10 || result.Scanned != 60 || result.Pages != 6 || result.Works[0].Work.DOI != "10.1234/50" || result.Works[9].Work.DOI != "10.1234/59" || result.Continuation == "" {
		t.Fatalf("first page: %+v", result)
	}
	request.Continuation = result.Continuation
	next, err := SearchUnowned(context.Background(), source, request, classifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Works) != 10 || next.Works[0].Work.DOI != "10.1234/60" || next.Continuation != "" || next.Truncated {
		t.Fatalf("resumed page: %+v", next)
	}
	request.Search.Query = "different"
	before := source.calls
	if _, err := SearchUnowned(context.Background(), source, request, classifier); !errors.Is(err, ErrInvalidPageToken) || source.calls != before {
		t.Fatalf("mismatched continuation performed a request: %v", err)
	}
}
func TestUnownedSearchDisclosesBudgetAndRefusesIncompleteOwnership(t *testing.T) {
	source := &unownedFixture{total: 120}
	classifier := func(_ context.Context, rows []DiscoveredWork) error {
		for i := range rows {
			n, _ := strconv.Atoi(rows[i].Work.DOI[len("10.1234/"):])
			rows[i].Owned = n < 100
		}
		return nil
	}
	request := UnownedRequest{Search: SearchParams{Query: "fixture", Limit: 10}}
	result, err := SearchUnowned(context.Background(), source, request, classifier)
	if err != nil || len(result.Works) != 0 || result.Pages != UnownedPageBudget || result.Continuation == "" || !result.Truncated {
		t.Fatalf("budget result: %+v %v", result, err)
	}
	request.Continuation = result.Continuation
	resumed, err := SearchUnowned(context.Background(), source, request, classifier)
	if err != nil || len(resumed.Works) != 10 || resumed.Works[0].Work.DOI != "10.1234/100" {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	failure := errors.New("feed unavailable")
	_, err = SearchUnowned(context.Background(), source, UnownedRequest{Search: request.Search}, func(context.Context, []DiscoveredWork) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("ownership failure hidden: %v", err)
	}
}
