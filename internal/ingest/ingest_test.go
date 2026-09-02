// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ingest

import (
	"reflect"
	"strings"
	"testing"

	"papio/internal/protocol"
)

// assertCanonicalRequests compares parsed requests against the full expected
// end state. RequestID is a content hash, so it is checked for shape here and
// for identifier-derived stability in TestParseRequestIDsFollowTheIdentifier.
func assertCanonicalRequests(t *testing.T, got, want []protocol.WorkRequest) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("requests = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i].RequestID, "batch-") {
			t.Fatalf("request %d id = %q, want a batch- prefixed deterministic id", i+1, got[i].RequestID)
		}
		expected := want[i]
		expected.SchemaVersion = protocol.WorkRequestSchemaVersion
		expected.DesiredVersion = "any"
		expected.RequestID = got[i].RequestID
		if !reflect.DeepEqual(got[i], expected) {
			t.Fatalf("request %d = %+v (identifiers %+v), want %+v (identifiers %+v)",
				i+1, got[i], got[i].Identifiers, expected, expected.Identifiers)
		}
	}
}

// Acquisition parsing is strict where holdings parsing is tolerant: a user who
// asked to acquire several papers must not silently receive fewer. This guards
// the asymmetry that justifies the bibparse/ingest split.
func TestParseAbortsOnAnIdentifierlessRecord(t *testing.T) {
	input := "@article{a,\n title = {Has A DOI},\n doi = {10.1000/one},\n}\n\n@book{b,\n title = {No Identifier},\n}\n"
	_, err := Parse(FormatBibTeX, []byte(input))
	if err == nil {
		t.Fatal("a record that cannot become a work request must abort the batch")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error must name the offending record, got %v", err)
	}
}

func TestParseConvertsIdentifiers(t *testing.T) {
	input := "@article{a,\n title = {A Real Title},\n doi = {https://doi.org/10.1000/One},\n eprint = {2401.00001},\n archiveprefix = {arXiv},\n pmid = {12345678},\n year = {2024},\n}\n"
	requests, err := Parse(FormatBibTeX, []byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	ids := requests[0].Identifiers
	if ids == nil {
		t.Fatal("identifiers not carried through conversion")
	}
	if ids.DOI == "" {
		t.Fatalf("DOI missing: %+v", ids)
	}
	if ids.ArXiv != "2401.00001" {
		t.Fatalf("ArXiv = %q, want 2401.00001", ids.ArXiv)
	}
	if ids.PMID != "12345678" {
		t.Fatalf("PMID = %q, want 12345678", ids.PMID)
	}
}

func TestParseRefusesJSONL(t *testing.T) {
	_, err := Parse(FormatJSONL, []byte(`{"doi":"10.1/a"}`))
	if err == nil {
		t.Fatal("jsonl is the batch reader's format and must be refused here")
	}
	if got, want := err.Error(), "jsonl input is parsed by the batch reader, not ingest"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// Detect is re-exported so callers keep one import after the split.
func TestDetectIsReExported(t *testing.T) {
	if got := Detect("refs.ris", nil); got != FormatRIS {
		t.Fatalf("Detect = %q, want %q", got, FormatRIS)
	}
}

// Every format must land on the same canonical work request as JSONL input:
// normalized identifiers, propagated metadata, and a deterministic request ID.
// Each format reaches that shape through its own decoder, so each needs its
// own success case; BibTeX is covered by TestParseConvertsIdentifiers.
func TestParseConvertsRISRecords(t *testing.T) {
	input := "TY  - JOUR\nTI  - First Article\nAU  - Smith, Ada\nAU  - Jones, Ben\nPY  - 2023/01/15\nDO  - https://doi.org/10.1000/First\nER  - \n\nTY  - JOUR\nTI  - Second Article\nAU  - Patel, Cora\nPY  - 2024\nDO  - doi:10.1000/SECOND\nER  - \n"
	requests, err := Parse(FormatRIS, []byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertCanonicalRequests(t, requests, []protocol.WorkRequest{
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/first"},
			Title:       "First Article",
			Authors:     []string{"Smith, Ada", "Jones, Ben"},
			Year:        2023,
		},
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/second"},
			Title:       "Second Article",
			Authors:     []string{"Patel, Cora"},
			Year:        2024,
		},
	})
}

func TestParseConvertsCSLJSONItems(t *testing.T) {
	input := `[
	  {
	    "DOI": "https://doi.org/10.1000/CSL-One",
	    "PMID": 12345678,
	    "title": "A CSL Item",
	    "author": [{"given": "Ada", "family": "Lovelace"}, {"literal": "Babbage, Charles"}],
	    "issued": {"date-parts": [[1843, 6]]}
	  },
	  {
	    "DOI": "10.1000/csl-two",
	    "title": "Another CSL Item",
	    "author": [{"family": "Hopper"}],
	    "issued": {"date-parts": [["1952"]]}
	  }
	]`
	requests, err := Parse(FormatCSLJSON, []byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertCanonicalRequests(t, requests, []protocol.WorkRequest{
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/csl-one", PMID: "12345678"},
			Title:       "A CSL Item",
			Authors:     []string{"Ada Lovelace", "Babbage, Charles"},
			Year:        1843,
		},
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/csl-two"},
			Title:       "Another CSL Item",
			Authors:     []string{"Hopper"},
			Year:        1952,
		},
	})
}

func TestParseConvertsNBIBRecords(t *testing.T) {
	input := "PMID- 40123456\nTI  - First study title\n      continued across a field line\nFAU - Doe, Jane\nFAU - Smith, John Q\nDP  - 2024 Jan 15\nLID - 10.1000/first-study [doi]\n\nPMID- 40123457\nTI  - Second study\nFAU - Nguyen, Mai\nDP  - 2023 Fall\nLID - S1234-5678(23)00001-2 [pii]\nAID - 10.1000/second-study [doi]\n"
	requests, err := Parse(FormatNBIB, []byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertCanonicalRequests(t, requests, []protocol.WorkRequest{
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/first-study", PMID: "40123456"},
			Title:       "First study title continued across a field line",
			Authors:     []string{"Doe, Jane", "Smith, John Q"},
			Year:        2024,
		},
		{
			Identifiers: &protocol.Identifiers{DOI: "10.1000/second-study", PMID: "40123457"},
			Title:       "Second study",
			Authors:     []string{"Nguyen, Mai"},
			Year:        2023,
		},
	})
}

// One work exported from three different reference managers must acquire under
// one request identity: the ID is keyed on the normalized identifier, never on
// the surrounding metadata, which differs per export.
func TestParseRequestIDsFollowTheIdentifier(t *testing.T) {
	sources := map[Format][]byte{
		FormatRIS:     []byte("TY  - JOUR\nTI  - RIS Spelling Of The Title\nAU  - Smith, Ada\nPY  - 2023\nDO  - https://doi.org/10.1000/Shared\nER  - \n"),
		FormatCSLJSON: []byte(`[{"DOI":"doi:10.1000/shared","title":"CSL Spelling Of The Title","author":[{"literal":"A. Smith"}],"issued":{"date-parts":[[2024]]}}]`),
		FormatNBIB:    []byte("PMID- 40123456\nTI  - NBIB Spelling Of The Title\nFAU - Smith, Ada B\nDP  - 2022\nAID - 10.1000/SHARED [doi]\n"),
	}
	ids := map[Format]string{}
	for format, data := range sources {
		requests, err := Parse(format, data)
		if err != nil {
			t.Fatalf("Parse(%s): %v", format, err)
		}
		if len(requests) != 1 {
			t.Fatalf("Parse(%s) = %d requests, want 1", format, len(requests))
		}
		if got := requests[0].Identifiers.DOI; got != "10.1000/shared" {
			t.Fatalf("Parse(%s) DOI = %q, want 10.1000/shared", format, got)
		}
		ids[format] = requests[0].RequestID
	}
	if ids[FormatRIS] != ids[FormatCSLJSON] || ids[FormatRIS] != ids[FormatNBIB] {
		t.Fatalf("one DOI produced diverging request ids: %v", ids)
	}
}

// No parser fills Record.ISBN, so the ISBN branch of convert — normalization
// plus title/authors/year propagation — is only reachable directly. ISBN-only
// acquisition metadata arrives from other input paths and must still normalize.
func TestConvertNormalizesISBNAndPropagatesMetadata(t *testing.T) {
	request, err := convert(Record{
		ISBN:    " 978-0-306-40615-7 ",
		Title:   "  The Structure Of Scientific Revolutions  ",
		Authors: []string{"  Kuhn, Thomas S.  ", "   ", ""},
		Year:    1962,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	ids := request.Identifiers
	if ids == nil {
		t.Fatal("an ISBN-only record must still carry identifiers")
	}
	if ids.ISBN != "9780306406157" {
		t.Fatalf("ISBN = %q, want 9780306406157", ids.ISBN)
	}
	if ids.DOI != "" || ids.PMID != "" || ids.ArXiv != "" || ids.OpenAlex != "" {
		t.Fatalf("only the ISBN may be set: %+v", ids)
	}
	if request.Title != "The Structure Of Scientific Revolutions" {
		t.Fatalf("Title = %q, want the trimmed title", request.Title)
	}
	if want := []string{"Kuhn, Thomas S."}; !reflect.DeepEqual(request.Authors, want) {
		t.Fatalf("Authors = %#v, want %#v", request.Authors, want)
	}
	if request.Year != 1962 {
		t.Fatalf("Year = %d, want 1962", request.Year)
	}

	// The same book under different metadata is the same acquisition request.
	same, err := convert(Record{ISBN: "9780306406157", Title: "A Different Cataloguing Entirely", Authors: []string{"Someone Else"}, Year: 2001})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if same.RequestID != request.RequestID {
		t.Fatalf("request id %q != %q: identity must key on the normalized ISBN", same.RequestID, request.RequestID)
	}
}
