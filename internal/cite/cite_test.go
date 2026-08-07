// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cite

import (
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/work"
)

func article() Record {
	return FromWork(work.Work{
		Title:     "  The perils of plurality rule  ",
		Authors:   []string{"Joshua Holzer", "  ", "Ada Lovelace"},
		Year:      2022,
		Container: "PLOS ONE",
		DOI:       "10.1371/journal.pone.0262026",
		PMID:      "35051190",
	})
}

func TestFromWorkExportsOnlyKnownValues(t *testing.T) {
	r := FromWork(work.Work{Title: "Bare title"})
	payload, err := CSLJSON([]Record{r})
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(payload, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	for _, forbidden := range []string{"issued", "author", "container-title", "DOI", "ISBN", "custom", "abstract"} {
		if _, ok := items[0][forbidden]; ok {
			t.Fatalf("CSL item carries %q for a title-only work: absent values must be omitted, never invented", forbidden)
		}
	}
}

func TestIdentityFallsBackDOIThenPMIDThenArXivThenMeta(t *testing.T) {
	for _, test := range []struct {
		name string
		r    Record
		want string
	}{
		{name: "doi wins", r: Record{DOI: "10.1/X", PMID: "1", ArXiv: "2401.1"}, want: "doi:10.1/x"},
		{name: "pmid next", r: Record{PMID: "35051190", ArXiv: "2401.1"}, want: "pmid:35051190"},
		{name: "arxiv next", r: Record{ArXiv: "2401.12345v2"}, want: "arxiv:2401.12345v2"},
		{name: "meta last", r: Record{Title: "A Title!", Authors: []string{"Ada Lovelace"}, Year: 2020}, want: "meta:a title|ada lovelace|2020"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.r.Identity(); got != test.want {
				t.Fatalf("Identity = %q, want %q", got, test.want)
			}
		})
	}
}

func TestKeyIsStableAcrossTitleCorrections(t *testing.T) {
	a := article()
	b := article()
	b.Title = "The Perils of Plurality Rule?" // corrected casing/punctuation
	if a.Key() != b.Key() {
		t.Fatalf("keys %q vs %q: a DOI-identified record's key must survive a title correction", a.Key(), b.Key())
	}
	if !strings.HasPrefix(a.Key(), "holzer-2022-perils-") {
		t.Fatalf("key = %q, want firstauthor-year-titleword prefix", a.Key())
	}
}

func TestDedupeKeepsFirstOccurrenceInScopeOrder(t *testing.T) {
	first := article()
	duplicate := article()
	duplicate.Title = "Different title, same DOI"
	other := Record{Title: "Another work", ArXiv: "2401.12345"}
	kept, collapsed := Dedupe([]Record{first, duplicate, other})
	if len(kept) != 2 || collapsed != 1 {
		t.Fatalf("Dedupe = %d kept, %d collapsed", len(kept), collapsed)
	}
	if kept[0].Title != first.Title || kept[1].ArXiv != "2401.12345" {
		t.Fatalf("kept = %+v, want first occurrence retained in order", kept)
	}
}

func TestBibTeXEscapesReservedCharactersAndJoinsAuthors(t *testing.T) {
	r := article()
	r.Title = "50% of R&D {matters}_here"
	out := string(BibTeX([]Record{r}))
	if !strings.Contains(out, `50\% of R\&D \{matters\}\_here`) {
		t.Fatalf("BibTeX output does not escape reserved characters:\n%s", out)
	}
	if !strings.Contains(out, "author = {Joshua Holzer and Ada Lovelace}") {
		t.Fatalf("BibTeX output does not join literal authors with ' and ':\n%s", out)
	}
	if !strings.Contains(out, "@article{holzer-2022-") {
		t.Fatalf("BibTeX entry type or key wrong:\n%s", out)
	}
}

func TestRISUsesRegistryTagsAndCRLF(t *testing.T) {
	out := string(RIS([]Record{article()}))
	for _, want := range []string{
		"TY  - JOUR\r\n", "TI  - The perils of plurality rule\r\n",
		"AU  - Joshua Holzer\r\n", "AU  - Ada Lovelace\r\n",
		"PY  - 2022\r\n", "T2  - PLOS ONE\r\n",
		"DO  - 10.1371/journal.pone.0262026\r\n", "AN  - PMID:35051190\r\n",
		"ER  - \r\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("RIS output missing %q:\n%s", want, out)
		}
	}
}

func TestBookTypingIsIdentifierBasedOnly(t *testing.T) {
	book := FromWork(work.Work{Title: "A Monograph", ISBN: "9780306406157"})
	if book.itemType() != "book" {
		t.Fatalf("ISBN-only work typed %q, want book", book.itemType())
	}
	// A DOI or container makes it a journal-style article again — and a
	// title that LOOKS like a book must never influence the type.
	chapterish := FromWork(work.Work{Title: "Handbook of Everything", ISBN: "9780306406157", DOI: "10.1/x", Container: "Series"})
	if chapterish.itemType() != "article-journal" {
		t.Fatalf("typed %q, want article-journal: typing is identifier-based only", chapterish.itemType())
	}
	out := string(RIS([]Record{book}))
	if !strings.Contains(out, "TY  - BOOK\r\n") || !strings.Contains(out, "SN  - 9780306406157\r\n") {
		t.Fatalf("book RIS wrong:\n%s", out)
	}
}

func TestRenderRejectsUnknownFormats(t *testing.T) {
	if _, err := Render("endnote", nil); err == nil || !strings.Contains(err.Error(), "unsupported export format") {
		t.Fatalf("Render(endnote) = %v, want an unsupported-format error", err)
	}
}

func TestCSLJSONCarriesLiteralAuthorsAndCustomIdentifiers(t *testing.T) {
	r := article()
	r.ArXiv = "2401.12345"
	payload, err := CSLJSON([]Record{r})
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Type   string              `json:"type"`
		Author []map[string]string `json:"author"`
		Custom map[string]string   `json:"custom"`
		Issued struct {
			DateParts [][]int `json:"date-parts"`
		} `json:"issued"`
	}
	if err := json.Unmarshal(payload, &items); err != nil {
		t.Fatal(err)
	}
	item := items[0]
	if item.Type != "article-journal" || len(item.Author) != 2 || item.Author[0]["literal"] != "Joshua Holzer" {
		t.Fatalf("item = %+v: authors must be literal names, never split into family/given by guesswork", item)
	}
	if item.Custom["pmid"] != "35051190" || item.Custom["arxiv"] != "2401.12345" {
		t.Fatalf("custom = %+v", item.Custom)
	}
	if len(item.Issued.DateParts) != 1 || item.Issued.DateParts[0][0] != 2022 {
		t.Fatalf("issued = %+v", item.Issued)
	}
}
