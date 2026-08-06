package landingmeta

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contractCase mirrors one entry of testdata/contract.json. The extension's
// own test suite unmarshals the same file — driving this test from JSON
// rather than a hand-copied Go table is what makes a divergence between the
// two implementations show up as a broken test instead of silent drift.
type contractCase struct {
	Name                string `json:"name"`
	File                string `json:"file"`
	Base                string `json:"base"`
	Want                string `json:"want"`
	AgreedWithExtension bool   `json:"agreed_with_extension"`
	Note                string `json:"note"`
}

func loadContract(t *testing.T) []contractCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contract.json"))
	if err != nil {
		t.Fatalf("reading contract.json: %v", err)
	}
	var cases []contractCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("unmarshaling contract.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("contract.json has no cases")
	}
	return cases
}

func TestPDFURL_Contract(t *testing.T) {
	for _, tc := range loadContract(t) {
		t.Run(tc.Name, func(t *testing.T) {
			htmlBytes, err := os.ReadFile(filepath.Join("testdata", tc.File))
			if err != nil {
				t.Fatalf("reading %s: %v", tc.File, err)
			}
			base, err := url.Parse(tc.Base)
			if err != nil {
				t.Fatalf("parsing base %q: %v", tc.Base, err)
			}

			got, err := PDFURL(htmlBytes, base)

			if tc.Name == "duplicate_conflicting" {
				if !errors.Is(err, ErrConflictingPDFURL) {
					t.Fatalf("PDFURL() error = %v, want ErrConflictingPDFURL", err)
				}
			} else if err != nil {
				t.Fatalf("PDFURL() unexpected error: %v", err)
			}

			if got != tc.Want {
				t.Errorf("PDFURL() = %q, want %q (%s)", got, tc.Want, tc.Note)
			}
		})
	}
}

// TestPDFURL_NoContentAttribute covers a case the contract corpus doesn't:
// a citation_pdf_url meta tag present with no content attribute at all
// (distinct from empty_content's content=""). It must be treated the same
// way — nothing to resolve, no PDF advertised.
func TestPDFURL_NoContentAttribute(t *testing.T) {
	html := []byte(`<!DOCTYPE html><html><head><meta name="citation_pdf_url"></head><body></body></html>`)
	base, err := url.Parse("https://publisher.example/x")
	if err != nil {
		t.Fatalf("parsing base: %v", err)
	}

	got, err := PDFURL(html, base)
	if err != nil {
		t.Fatalf("PDFURL() unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("PDFURL() = %q, want empty", got)
	}
}

// TestPDFURL_HeadBoundStopsBeforeBody proves the other half of the bound
// documented on PDFURL: scanning stops when head closes, so a multi-megabyte
// body can't force this loop to do multi-megabyte work. The real
// citation_pdf_url tag here sits in body, well past head's close tag; if the
// bound weren't enforced, PDFURL would find and return it.
func TestPDFURL_HeadBoundStopsBeforeBody(t *testing.T) {
	const target = 3 * 1024 * 1024 // 3 MB, per the assignment
	filler := strings.Repeat("a", target)

	doc := "<!DOCTYPE html><html><head><title>t</title></head><body>" +
		filler +
		`<meta name="citation_pdf_url" content="https://publisher.example/should-not-be-found.pdf">` +
		"</body></html>"
	if len(doc) < target {
		t.Fatalf("test document is only %d bytes, want at least %d", len(doc), target)
	}

	base, err := url.Parse("https://publisher.example/x")
	if err != nil {
		t.Fatalf("parsing base: %v", err)
	}

	got, err := PDFURL([]byte(doc), base)
	if err != nil {
		t.Fatalf("PDFURL() unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("PDFURL() = %q, want empty: head bound did not stop the scan before body", got)
	}
}
