package pdf

import (
	"strings"
	"testing"
)

// TestAppendOCRPageSeparatesPages pins the separator itself: pages join with a
// form feed, and none is written ahead of the first page. Without this,
// identityWindow has no page break to cut on and every front-matter rule reads
// a "page one" spanning the whole OCR'd document.
func TestAppendOCRPageSeparatesPages(t *testing.T) {
	var all strings.Builder
	appendOCRPage(&all, "page one text")
	if got := all.String(); got != "page one text" {
		t.Fatalf("first page must not be prefixed with a separator: %q", got)
	}
	appendOCRPage(&all, "page two text")
	appendOCRPage(&all, "page three text")

	want := "page one text\fpage two text\fpage three text"
	if got := all.String(); got != want {
		t.Fatalf("pages joined as %q, want %q", got, want)
	}
	if got := strings.Count(all.String(), "\f"); got != 2 {
		t.Fatalf("three pages need exactly two separators, got %d", got)
	}
}

// TestOCRPageBoundaryKeepsForeignDOIOutOfBlindWindow is the regression for the
// defect the separator fixes, stated at the level that made it dangerous.
//
// A scanned page one can carry very little text. Page two of the same document
// routinely prints another work's DOI — a reference, a related-article note, a
// data citation. FrontMatterDOIs feeds the BLIND path, which has no candidate to
// check against, so a DOI reaching that window does not corroborate an identity,
// it MINTS one: the capture is filed as whatever work page two happened to name.
//
// The two halves of this test are the before and after of one change. The
// unseparated form is not hypothetical past behaviour; it is exactly what
// extractOCR produced.
func TestOCRPageBoundaryKeepsForeignDOIOutOfBlindWindow(t *testing.T) {
	const sparseFirstPage = "Scanned Report\n\nPrepared for internal circulation.\n"
	const secondPage = "References\n\n1. Prior work, doi:10.1145/3065386, cited here.\n"

	t.Run("separated", func(t *testing.T) {
		var all strings.Builder
		appendOCRPage(&all, sparseFirstPage)
		appendOCRPage(&all, secondPage)

		if dois := FrontMatterDOIs(all.String()); len(dois) != 0 {
			t.Fatalf("page two's DOI reached the blind 1 KiB window: %v", dois)
		}
	})

	// Pins the causal claim in appendOCRPage's comment. If this ever stops
	// leaking, the separator is no longer what keeps page two out and the
	// comment above it is wrong.
	t.Run("unseparated leaks, which is why the separator exists", func(t *testing.T) {
		leaky := sparseFirstPage + secondPage
		dois := FrontMatterDOIs(leaky)
		if len(dois) == 0 {
			t.Fatal("expected the unseparated form to leak page two's DOI into the blind window")
		}
		if dois[0] != "10.1145/3065386" {
			t.Fatalf("leaked DOI is %q, want the page-two one", dois[0])
		}
	})
}

// TestOCRPageBoundaryBoundsPageOneWindows checks the wider windows too, since
// the byline and page-one bounds feed the targeted rules: an unseparated
// document let page two's authors and identifiers answer questions asked about
// page one.
func TestOCRPageBoundaryBoundsPageOneWindows(t *testing.T) {
	var all strings.Builder
	appendOCRPage(&all, "Scanned Report\n")
	appendOCRPage(&all, strings.Repeat("second page body. ", 400))
	text := all.String()

	if got := identityPageOne(text); strings.Contains(got, "second page body") {
		t.Fatalf("page-one window ran past the page break: %q", got)
	}
	if got := identityByline(text); strings.Contains(got, "second page body") {
		t.Fatalf("byline window ran past the page break: %q", got)
	}
	if got := identityFrontMatter(text); strings.Contains(got, "second page body") {
		t.Fatalf("front-matter window ran past the page break: %q", got)
	}
}
