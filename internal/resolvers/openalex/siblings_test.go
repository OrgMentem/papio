// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package openalex

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"papio/internal/resolver"
	"papio/internal/work"
)

// canonicalRecord is a paywalled publisher work (is_oa false).
const canonicalRecord = `{
  "id": "https://openalex.org/W1111111111",
  "doi": "https://doi.org/10.1145/3531146.3533202",
  "title": "How Explanations Shape Trust",
  "publication_year": 2022,
  "authorships": [
    {"author": {"display_name": "Andrea Ferrario"}},
    {"author": {"display_name": "Michele Loi"}}
  ],
  "open_access": {"is_oa": false, "oa_status": "closed"}
}`

func siblingSearchBody(title string, year int, doi, author, pdf string) string {
	return `{"results":[{
      "id": "https://openalex.org/W2222222222",
      "doi": "https://doi.org/` + doi + `",
      "title": "` + title + `",
      "publication_year": ` + strconv.Itoa(year) + `,
      "authorships": [{"author": {"display_name": "` + author + `"}}],
      "open_access": {"is_oa": true, "oa_status": "green"},
      "best_oa_location": {"is_oa": true, "pdf_url": "` + pdf + `", "version": "submittedVersion", "license": "cc-by"}
    }]}`
}

func siblingResolver(t *testing.T, searchBody string) *Resolver {
	t.Helper()
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("search") != "" {
			return responseFor(200, searchBody, nil), nil
		}
		return responseFor(200, canonicalRecord, nil), nil
	})
	return NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "key", BaseURL: "https://api.test/works", SiblingTitleSearch: true})
}

// canonicalWork is the requested work as the app hands it over once the
// canonical DOI has already been resolved this pass: title, year and authors
// are the same values the record carries. ResolveSiblings no longer GETs that
// record — it reuses a Resolve memo, or these caller-supplied fields.
func canonicalWork() work.Work {
	return work.Work{
		DOI: "10.1145/3531146.3533202", Title: "How Explanations Shape Trust",
		Year: 2022, Authors: []string{"Andrea Ferrario", "Michele Loi"},
	}
}

func TestResolveSiblingsFindsOAPreprintOfPaywalledDOI(t *testing.T) {
	r := siblingResolver(t, siblingSearchBody(
		"How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"))
	// Seed the memo the way the pipeline does: the canonical DOI is resolved
	// first (that GET is what writes the record), and the sibling hop reuses it
	// instead of paying for a second canonical GET.
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	candidates, err := r.ResolveSiblings(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want exactly one sibling", candidates)
	}
	got := candidates[0]
	if got.ResolvedWork.DOI != "10.2139/ssrn.4020557" || got.URL != "https://ssrn.example/paper.pdf" {
		t.Fatalf("sibling candidate = %#v", got)
	}
	if !strings.Contains(strings.Join(got.Evidence, " "), "sibling_of=10.1145/3531146.3533202") {
		t.Fatalf("evidence missing sibling_of: %#v", got.Evidence)
	}
}

func TestResolveSiblingsMatchesAcrossPunctuationDrift(t *testing.T) {
	r := siblingResolver(t, siblingSearchBody(
		"How Explanations Shape Trust.", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"))
	candidates, err := r.ResolveSiblings(context.Background(), canonicalWork())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, err = %v; want punctuation-equivalent title to match", candidates, err)
	}
}

func TestResolveSiblingsRejectsWeakMatches(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "different title", body: siblingSearchBody(
			"A Completely Different Paper", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf")},
		{name: "no shared author", body: siblingSearchBody(
			"How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Grace Hopper", "https://ssrn.example/paper.pdf")},
		{name: "year too far", body: siblingSearchBody(
			"How Explanations Shape Trust", 2015, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf")},
		{name: "missing year rejected when canonical is dated", body: siblingSearchBody(
			"How Explanations Shape Trust", 0, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf")},
		{name: "same doi is not a sibling", body: siblingSearchBody(
			"How Explanations Shape Trust", 2022, "10.1145/3531146.3533202", "Andrea Ferrario", "https://ssrn.example/paper.pdf")},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := siblingResolver(t, test.body)
			candidates, err := r.ResolveSiblings(context.Background(), canonicalWork())
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %#v, err = %v; want none", candidates, err)
			}
		})
	}
}

func TestResolveSiblingsWithoutDOIIsNoOp(t *testing.T) {
	r := siblingResolver(t, `{"results":[]}`)
	candidates, err := r.ResolveSiblings(context.Background(), work.Work{Title: "No DOI"})
	if err != nil || candidates != nil {
		t.Fatalf("candidates = %#v, err = %v; want nil", candidates, err)
	}
}

func TestSiblingGateOffMakesNoRequest(t *testing.T) {
	calls := 0
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return responseFor(200, `{"results":[]}`, nil), nil
	})
	r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "key", BaseURL: "https://api.test/works"})
	// default is off
	if _, err := r.ResolveSiblings(context.Background(), canonicalWork()); !errors.Is(err, resolver.ErrNoSearchBasis) {
		t.Fatalf("err = %v, want ErrNoSearchBasis when gated off", err)
	}
	if calls != 0 {
		t.Fatalf("requests = %d, want 0 when gated off", calls)
	}
}

func TestSiblingGateOnPreservesBehaviour(t *testing.T) {
	calls := 0
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("search") != "" {
			calls++
			return responseFor(200, siblingSearchBody("How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"), nil), nil
		}
		return responseFor(200, canonicalRecord, nil), nil
	})
	r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "key", BaseURL: "https://api.test/works", SiblingTitleSearch: true})
	cands, err := r.ResolveSiblings(context.Background(), canonicalWork())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) == 0 {
		t.Fatal("gated-on sibling search returned no candidates")
	}
	if calls == 0 {
		t.Fatal("gated-on sibling search made no request")
	}
}
