package pdf

import (
	"strings"
	"testing"

	"papio/internal/work"
)

// bookFront is the extracted text of a book whose cover is letter-spaced
// display type, so the article rules cannot read its title off page one, with
// the title and the copyright page (and its ISBNs) on the pages that follow.
func bookFront(isbn string) string {
	return "Me n t a l\nHe a l t h\n\nAt l a s\n2024\n\f" +
		"Me n t a l\nHe a l t h\n\nAt l a s\n2024\n\f" +
		"Mental Health Atlas 2024\nISBN " + isbn + " (electronic version)\nISBN 978-92-4-011449-4 (print version)\n" +
		"© World Health Organization 2025\nSome rights reserved.\n\f" +
		strings.Repeat("Mental health services are described in the chapters that follow. ", 40)
}

func TestMatchISBNBoundIdentity(t *testing.T) {
	const isbn = "9789240114487"
	titled := work.Work{ISBN: isbn, Title: "Mental health atlas 2024"}
	tests := []struct {
		name   string
		text   string
		target work.Work
		bound  string
		want   string
	}{
		{"letter-spaced cover and the ISBN on the copyright page", bookFront("978-92-4-011448-7"), titled, isbn, IdentityPass},
		{"unhyphenated ISBN", bookFront(isbn), titled, isbn, IdentityPass},
		{"ISBN-10 form printed", bookFront("92-4-011448-3"), titled, isbn, IdentityPass},
		{"another edition's ISBN is printed, not the bound one", bookFront("978-92-4-099999-1"), titled, isbn, IdentityReview},
		{"bound ISBN differs from every printed ISBN", bookFront("978-92-4-011448-7"), titled, "9789240115873", IdentityReview},
		{"title words absent from the front matter", strings.Replace(strings.Replace(bookFront("978-92-4-011448-7"), "Mental Health Atlas 2024", "A Different Report", 1), "Me n t a l\nHe a l t h\n\nAt l a s\n2024", "Cover", 2), titled, isbn, IdentityReview},
		{"named authors keep the article rules", bookFront("978-92-4-011448-7"), work.Work{ISBN: isbn, Title: "Mental health atlas 2024", Authors: []string{"Jane Smith"}}, isbn, IdentityReview},
		{"no title to compare", bookFront("978-92-4-011448-7"), work.Work{ISBN: isbn}, isbn, IdentityReview},
		{"a correction notice is never upgraded", "Erratum to: Mental health atlas 2024\n" + bookFront("978-92-4-011448-7"), titled, isbn, IdentityReview},
		{"ISBN inside a longer digit run is not the ISBN", strings.Replace(bookFront("978-92-4-011448-7"), "ISBN 978-92-4-011448-7", "No. 19789240114487", 1), titled, isbn, IdentityReview},
		{"ISBN cited after the front matter", bookFront("978-92-4-099999-1") + strings.Repeat("padding text. ", 800) + " ISBN 978-92-4-011448-7", titled, isbn, IdentityReview},
		// The title is the other half of the binding, and all of it counts: the
		// article rules drop words under five letters, which is where a volume
		// or an edition lives.
		{"another edition's title with the bound ISBN printed", "Mental Health Atlas 2020\nISBN 978-92-4-011448-7\nWorld Health Organization\n", titled, isbn, IdentityReview},
		{"title words scattered across the front matter", "Atlas\nHealth services\nMental wellbeing 2024\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"title cited inline by another book", "Public Health Review\nISBN 978-92-4-099999-1\n" +
			"Data come from the Mental health atlas 2024 (ISBN 978-92-4-011448-7) and other reports.\n", titled, isbn, IdentityReview},
		{"title cited with its imprint on one line", "Public Health Review\nMental health atlas 2024. Geneva: World Health Organization; 2025. ISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"title listed under a related-publications heading", "Public Health Review\nISBN 978-92-4-099999-1\n\nSee also\nMental health atlas 2024\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"title listed under a references heading", "Public Health Review\nISBN 978-92-4-099999-1\n\nReferences\nMental health atlas 2024\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"the title with a subtitle is a different title", "Mental health atlas 2024: country profiles\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		// The title is the document's own only on its first page; the ISBN only
		// before a references-type heading. These are structural, not a window.
		{"title on page two under a references heading", "Public Health Review\f\nReferences\nWorld Health Organization\nGeneva, 2025\nMental Health Atlas 2024\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"references heading many lines above the title on page one", "Public Health Review\nA survey of services\nChapter summaries\nMethods\nResults\nReferences\nGeneva, 2025\nWorld Health Organization\nMental Health Atlas 2024\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"title only on page two, ISBN on page one", "Public Health Review\nISBN 978-92-4-011448-7\f\nMental Health Atlas 2024\n", titled, isbn, IdentityReview},
		{"title on page one but the ISBN only after a references heading", "Mental Health Atlas 2024\nWorld Health Organization\nReferences\nISBN 978-92-4-011448-7\n", titled, isbn, IdentityReview},
		{"image-only cover leaves the title page first", "\f\nMental Health Atlas 2024\nISBN 978-92-4-011448-7\nWorld Health Organization\n", titled, isbn, IdentityPass},
		// ISBN-10 check letter X, hyphenated as printed.
		{"hyphenated ISBN-10 ending in X", "An Old Monograph\nISBN 0-8044-2957-X\n", work.Work{ISBN: "9780804429573", Title: "An old monograph"}, "9780804429573", IdentityPass},
		{"lowercase terminal x", "An Old Monograph\nISBN 0-8044-2957-x\n", work.Work{ISBN: "9780804429573", Title: "An old monograph"}, "9780804429573", IdentityPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchISBNBoundIdentity(tt.text, tt.target, tt.bound, 0.6)
			if got.Result != tt.want {
				t.Fatalf("result = %s %v, want %s", got.Result, got.Evidence, tt.want)
			}
		})
	}
}

// A file the article rules already pass is still not a Pass here unless it
// prints the bound ISBN: the binding says which EDITION the record holds.
func TestMatchISBNBoundIdentityPassNeedsThePrintedISBN(t *testing.T) {
	const isbn = "9789240114487"
	target := work.Work{ISBN: isbn, Title: "Mental health atlas 2024"}
	otherEdition := "Mental Health Atlas 2024\nISBN 978-92-4-099999-1\nWorld Health Organization\n"
	if base := MatchIdentityWithThreshold(otherEdition, target, 0.6); base.Result != IdentityPass {
		t.Fatalf("test premise: the article rules should pass this file, got %s", base.Result)
	}
	if got := MatchISBNBoundIdentity(otherEdition, target, isbn, 0.6); got.Result != IdentityReview {
		t.Fatalf("a different edition with the same title reached %s: %v", got.Result, got.Evidence)
	}
	sameEdition := "Mental Health Atlas 2024\nISBN 978-92-4-011448-7\nWorld Health Organization\n"
	got := MatchISBNBoundIdentity(sameEdition, target, isbn, 0.6)
	if got.Result != IdentityPass || !strings.Contains(strings.Join(got.Evidence, " "), "repository record binds") {
		t.Fatalf("the bound edition must pass with the binding in its evidence: %+v", got)
	}
}

// A Reject from the article rules is never changed.
func TestMatchISBNBoundIdentityLeavesRejectAlone(t *testing.T) {
	const isbn = "9789240114487"
	titled := work.Work{ISBN: isbn, Title: "Mental health atlas 2024"}
	for name, text := range map[string]string{
		"non-article marker": "Supporting Information\nMental Health Atlas 2024\nISBN 978-92-4-011448-7\n",
		"title absent":       "Quantum chromodynamics on the lattice\nISBN 978-92-4-011448-7\n",
	} {
		if got := MatchISBNBoundIdentity(text, titled, isbn, 0.6); got.Result != IdentityReject {
			t.Errorf("%s: %+v, want reject", name, got)
		}
	}
}

func TestISBN10Of(t *testing.T) {
	for thirteen, want := range map[string]string{
		"9780306406157": "0306406152",
		"9789240114487": "9240114483",
		"9780198526636": "0198526636",
		"9780804429573": "080442957X",
	} {
		if got, ok := isbn10Of(thirteen); !ok || got != want {
			t.Errorf("isbn10Of(%s) = %q, %v; want %q", thirteen, got, ok, want)
		}
	}
	if _, ok := isbn10Of("9791234567896"); ok {
		t.Error("a 979 ISBN-13 has no ISBN-10 form")
	}
}
