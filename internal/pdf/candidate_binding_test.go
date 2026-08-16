// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"strings"
	"testing"
)

func TestCheckConclusiveIdentityAbsentWhenNoDOI(t *testing.T) {
	text := "This document has no identifier in its front matter.\nJust prose and a title.\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/example"}); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/example"}); got.Blocks() {
		t.Fatalf("VetoAbsent must not block")
	}
	if got := CheckConclusiveIdentity(text, nil); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q", VetoAbsent, got.Verdict)
	}
}

func TestCheckConclusiveIdentityCompatible(t *testing.T) {
	text := "Front matter DOI: 10.1234/compatible-work is printed at the top.\n"
	bound := []string{"10.1234/compatible-work"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); got.Blocks() {
		t.Fatalf("VetoCompatible must not block")
	}
}

func TestCheckConclusiveIdentityDoubledSlashCollapse(t *testing.T) {
	// Legacy APA PDFs collapse a doubled slash after the registrant for
	// identity comparison only (normalizeDOI). The veto must honour that
	// same collapse so 10.48612//monograph-2025-2 and
	// 10.48612/monograph-2025-2 compare equal.
	text := "DOI: 10.48612//monograph-2025-2 appears in the front matter.\n"
	bound := []string{"10.48612/monograph-2025-2"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); got.Blocks() {
		t.Fatalf("VetoCompatible must not block")
	}
	// Reverse the sides to pin that the collapse is comparison-scoped on
	// both sides, not only in the document.
	text2 := "DOI: 10.48612/monograph-2025-2 appears in the front matter.\n"
	bound2 := []string{"10.48612//monograph-2025-2"}
	if got := CheckConclusiveIdentity(text2, bound2); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
}

func TestCheckConclusiveIdentityAmbiguous(t *testing.T) {
	text := "DOIs in front matter: 10.1234/first and 10.1234/second both printed.\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); got.Verdict != VetoAmbiguous {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAmbiguous, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); !got.Blocks() {
		t.Fatalf("VetoAmbiguous must block")
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); len(got.DOIs) != 2 {
		t.Fatalf("want 2 DOIs got %v", got.DOIs)
	}
}

func TestCheckConclusiveIdentityForeignWithEmptyBound(t *testing.T) {
	// A PMID-only or arXiv-only job has no DOI recorded at all. A
	// conclusive front-matter DOI that names a work the job is not bound
	// to must be VetoForeign and must block, never VetoAbsent.
	text := "Front matter DOI: 10.1234/only-doi printed.\n"
	if got := CheckConclusiveIdentity(text, nil); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q evidence %v", VetoForeign, got.Verdict, got.Evidence)
	}
	if got := CheckConclusiveIdentity(text, nil); !got.Blocks() {
		t.Fatalf("VetoForeign must block")
	}
	if got := CheckConclusiveIdentity(text, []string{}); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q", VetoForeign, got.Verdict)
	}
}

func TestCheckConclusiveIdentityForeignDifferentWork(t *testing.T) {
	text := "DOI: 10.1234/foreign-work is in the front matter.\n"
	bound := []string{"10.1234/bound-work"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoForeign, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); !got.Blocks() {
		t.Fatalf("VetoForeign must block")
	}
}

func TestCheckConclusiveIdentityIgnoresDOIPastFrontMatterWindow(t *testing.T) {
	// The veto is deliberately narrow: it reads only the 1 KiB front-matter
	// window (identityFrontMatterBytes), never whole-document or
	// identityPageOne (4 KiB). A DOI that appears only past that window
	// must not be treated as a conclusive naming of the document, so the
	// verdict stays VetoAbsent. This is a separately gated decision — widening
	// the window is out of scope here and would reintroduce bibliography
	// false positives.
	filler := strings.Repeat("x ", 600) // > 1200 bytes, pushes the DOI past 1 KiB
	text := filler + "\n10.1234/past-window\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/past-window"}); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/past-window"}); got.Blocks() {
		t.Fatalf("VetoAbsent must not block even when the DOI beyond the window matches bound")
	}
}

func TestCheckConclusiveIdentityBoundSetIgnoresJunkEntries(t *testing.T) {
	text := "DOI: 10.1234/good is in front matter.\n"
	bound := []string{"", "   ", "not-a-doi", "10.1234/good", "not-a-doi/again"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); got.Blocks() {
		t.Fatalf("VetoCompatible must not block even with junk in bound set")
	}
}
