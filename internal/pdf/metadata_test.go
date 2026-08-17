// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"papio/internal/work"
)

// realPacket is the shape publisher XMP actually takes: several rdf:Description
// blocks, each binding its own prefixes, with repeated properties wrapped in an
// rdf:Bag. Every assertion below reads this rather than a hand-simplified packet,
// because the wrapping and the namespace binding are exactly what the parser has
// to get right.
const realPacket = `<?xpacket begin='' id='W5M0MpCehiHzreSzNTczkc9d'?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about="" xmlns:prism="http://prismstandard.org/namespaces/basic/2.1/">
   <prism:doi>10.1234/target</prism:doi>
  </rdf:Description>
  <rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/">
   <dc:identifier><rdf:Bag><rdf:li>10.1234/target</rdf:li></rdf:Bag></dc:identifier>
   <dc:title><rdf:Alt><rdf:li xml:lang="x-default">An Unrelated Title</rdf:li></rdf:Alt></dc:title>
   <dc:subject><rdf:Bag><rdf:li>10.9999/somebody-elses-work</rdf:li></rdf:Bag></dc:subject>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end='w'?>`

func fieldValues(t *testing.T, fields MetadataFields, name string) []string {
	t.Helper()
	var out []string
	for _, f := range fields {
		if f.Field == name {
			out = append(out, f.Value)
		}
	}
	return out
}

// TestParseXMPAttributesValuesToTheEnclosingProperty is the regression that
// motivated resolving namespaces at all. XMP wraps a repeated property's value
// in rdf:Bag/rdf:li, so the text node's own element is rdf:li and carries no
// meaning; the value belongs to the nearest enclosing PROPERTY. An earlier
// version resolved rdf:li to its full namespace URI, failed to recognise it as a
// container, and attributed every wrapped value to "rdf:li" — which silently
// dropped dc:identifier, the single largest source measured in the operator's
// library.
func TestParseXMPAttributesValuesToTheEnclosingProperty(t *testing.T) {
	fields := parseXMP(realPacket)

	if got := fieldValues(t, fields, "xmp/dc:identifier"); len(got) != 1 || got[0] != "10.1234/target" {
		t.Fatalf("dc:identifier wrapped in rdf:Bag/rdf:li was not attributed to its property: %v (all: %v)", got, fields)
	}
	if got := fieldValues(t, fields, "xmp/prism:doi"); len(got) != 1 || got[0] != "10.1234/target" {
		t.Fatalf("prism:doi = %v, want the target DOI once (all: %v)", got, fields)
	}
	for _, f := range fields {
		if f.Field == "xmp/rdf:li" || f.Field == "xmp/rdf:bag" || f.Field == "xmp/rdf:alt" {
			t.Fatalf("an RDF container was recorded as a field: %+v", f)
		}
	}
}

// TestParseXMPExcludesFreeTextFields pins the allowlist's purpose. dc:subject
// and dc:title routinely contain identifiers and titles, but neither field
// asserts whose they are — an aggregator populates them freely — so a DOI found
// there must never become self-attribution evidence.
func TestParseXMPExcludesFreeTextFields(t *testing.T) {
	fields := parseXMP(realPacket)
	for _, f := range fields {
		switch f.Field {
		case "xmp/dc:subject", "xmp/dc:title":
			t.Fatalf("free-text field collected: %+v", f)
		}
		if strings.Contains(f.Value, "somebody-elses-work") {
			t.Fatalf("a foreign identifier from a free-text field was collected: %+v", f)
		}
	}
}

// TestParseXMPResolvesSchemaVersionsToOnePrefix guards the version-churn hazard.
// PRISM ships basic/2.0, 2.1, 3.0 and pam/2.0, and publishers bump those URIs
// independently of anything papio controls. Matching whole URIs would fail
// closed on a version this list had never seen — a missed bind, which is the
// failure mode that hides rather than announcing itself.
func TestParseXMPResolvesSchemaVersionsToOnePrefix(t *testing.T) {
	for _, ns := range []string{
		"http://prismstandard.org/namespaces/basic/2.0/",
		"http://prismstandard.org/namespaces/basic/2.1/",
		"http://prismstandard.org/namespaces/basic/3.0/",
		"http://prismstandard.org/namespaces/pam/2.0/",
	} {
		packet := `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
			`<rdf:Description xmlns:p="` + ns + `"><p:doi>10.1/v</p:doi></rdf:Description></rdf:RDF>`
		if got := fieldValues(t, parseXMP(packet), "xmp/prism:doi"); len(got) != 1 {
			t.Errorf("namespace %s did not resolve to prism: got %v", ns, parseXMP(packet))
		}
	}
}

// TestParseXMPReadsCompactAttributeForm covers the other legal RDF spelling: a
// property may be an attribute on rdf:Description instead of a child element,
// and it is exactly as authoritative there.
func TestParseXMPReadsCompactAttributeForm(t *testing.T) {
	packet := `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description rdf:about="" xmlns:prism="http://prismstandard.org/namespaces/basic/2.1/"` +
		` prism:doi="10.1234/compact"/></rdf:RDF>`
	if got := fieldValues(t, parseXMP(packet), "xmp/prism:doi"); len(got) != 1 || got[0] != "10.1234/compact" {
		t.Fatalf("compact attribute form not read: %v", parseXMP(packet))
	}
}

// TestParseXMPTruncatedPacketKeepsCompleteValues pins that a bounded read
// degrades rather than failing. ExtractMetadata caps the subprocess output, so a
// large packet arrives cut mid-element; values already closed before the cut are
// complete evidence and must survive.
func TestParseXMPTruncatedPacketKeepsCompleteValues(t *testing.T) {
	cut := strings.Index(realPacket, "<dc:subject>")
	if cut < 0 {
		t.Fatal("fixture no longer contains the element the truncation point is taken from")
	}
	fields := parseXMP(realPacket[:cut])
	if got := fieldValues(t, fields, "xmp/prism:doi"); len(got) != 1 {
		t.Fatalf("a value closed before the truncation point was lost: %v", fields)
	}
}

// TestNamesWorkRequiresACompleteToken is the wrong-accept guard on the matcher
// seam. NamesWork delegates to corroboratingIdentifier precisely so metadata and
// page-one text can never disagree about what counts as an identifier; this pins
// the property that matters most, that a prefix of a longer identifier is not a
// match.
func TestNamesWorkRequiresACompleteToken(t *testing.T) {
	fields := MetadataFields{{Field: "xmp/dc:identifier", Value: "10.1234/target-extended"}}
	if got := fields.NamesWork(work.Work{DOI: "10.1234/target"}); got != "" {
		t.Fatalf("a DOI that is only a prefix of the metadata value matched: %q", got)
	}
	exact := MetadataFields{{Field: "xmp/dc:identifier", Value: "10.1234/target"}}
	if got := exact.NamesWork(work.Work{DOI: "10.1234/target"}); got == "" {
		t.Fatal("the document's own DOI in dc:identifier did not corroborate")
	}
}

// TestNamesWorkReadsIdentifierURLs covers how publishers actually write these
// fields: prism:url and dc:identifier normally hold a resolver URL, not a bare
// DOI.
func TestNamesWorkReadsIdentifierURLs(t *testing.T) {
	fields := MetadataFields{{Field: "xmp/prism:url", Value: "https://doi.org/10.1234/target"}}
	if got := fields.NamesWork(work.Work{DOI: "10.1234/target"}); got == "" {
		t.Fatal("a resolver URL naming the work did not corroborate")
	}
}

// TestNamesWorkEmptyIsSilent pins the fallback contract: a file with no usable
// metadata must produce no evidence and no error path, so the predicate behaves
// exactly as it did before metadata existed.
func TestNamesWorkEmptyIsSilent(t *testing.T) {
	if got := MetadataFields(nil).NamesWork(work.Work{DOI: "10.1234/target"}); got != "" {
		t.Fatalf("empty metadata produced evidence: %q", got)
	}
}

// TestNamesWorkAndCanonicalShareOneOrder pins that evidence selection and the
// provenance digest agree. autoBindProvenance stores Canonical()'s hash beside
// the winner's evidence string; if the two used different orderings, a digest
// would not correspond to the evidence recorded next to it.
func TestNamesWorkAndCanonicalShareOneOrder(t *testing.T) {
	forward := MetadataFields{
		{Field: "xmp/prism:doi", Value: "10.1234/target"},
		{Field: "xmp/dc:identifier", Value: "10.1234/target"},
	}
	reversed := MetadataFields{forward[1], forward[0]}

	if a, b := forward.Canonical(), reversed.Canonical(); a != b {
		t.Fatalf("Canonical depends on input order:\n%q\n%q", a, b)
	}
	target := work.Work{DOI: "10.1234/target"}
	if a, b := forward.NamesWork(target), reversed.NamesWork(target); a != b {
		t.Fatalf("evidence depends on input order: %q vs %q", a, b)
	}
	if got := forward.NamesWork(target); !strings.Contains(got, "dc:identifier") {
		t.Fatalf("evidence did not name the canonically first field: %q", got)
	}
}

// TestBindDocumentDigestFoldsMetadataOnlyWhenPresent pins both halves of the
// provenance digest's promise. A document with no usable metadata must digest to
// exactly its excerpt hash, so the ordinary case stays comparable with rows
// written before metadata was an input; and metadata that actually contributed
// must change the digest, because an audit row whose inputs it cannot
// distinguish cannot reconstruct its own decision.
func TestBindDocumentDigestFoldsMetadataOnlyWhenPresent(t *testing.T) {
	const excerpt = "Structural Priors in Retrieval Systems\nAlice Ciani\n2019\n"
	plain := sha256.Sum256([]byte(excerpt))
	want := hex.EncodeToString(plain[:])

	if got := (BindDocument{Excerpt: excerpt}).Digest(); got != want {
		t.Fatalf("a document with no metadata must digest to its excerpt hash:\n got %s\nwant %s", got, want)
	}
	if got := (BindDocument{Excerpt: excerpt, Metadata: MetadataFields{}}).Digest(); got != want {
		t.Fatalf("an empty (non-nil) metadata slice changed the digest: %s", got)
	}

	withMeta := BindDocument{
		Excerpt:  excerpt,
		Metadata: MetadataFields{{Field: "xmp/prism:doi", Value: "10.1234/target"}},
	}.Digest()
	if withMeta == want {
		t.Fatal("metadata the predicate read did not affect the digest")
	}

	other := BindDocument{
		Excerpt:  excerpt,
		Metadata: MetadataFields{{Field: "xmp/prism:doi", Value: "10.1234/different"}},
	}.Digest()
	if withMeta == other {
		t.Fatal("two documents whose metadata named different works share a digest")
	}
}

// TestQualifyCandidateMetadataSatisfiesIdentifierGate is the acceptance-set
// change that makes this rule candidate_auto_bind/3: a document whose page-one
// text never prints its identifier, but whose own XMP packet names it, qualifies
// instead of parking for review.
func TestQualifyCandidateMetadataSatisfiesIdentifierGate(t *testing.T) {
	const excerpt = "Structural Priors in Retrieval Systems\n" +
		"Alice Ciani, Boris Random\n" +
		"Journal of Record, volume 8, pages 44-71, 2019\n\n" +
		"Abstract\nWe study structural priors.\n"
	candidate := BindCandidate{
		Key:  "job-1",
		Work: work.Work{Title: "Structural Priors in Retrieval Systems", Authors: []string{"Ciani"}, Year: 2019, DOI: "10.1234/target"},
	}

	textOnly := QualifyCandidate(BindDocument{Excerpt: excerpt}, candidate)
	if !textOnly.Review || textOnly.Qualifies {
		t.Fatalf("without metadata this fixture must park for review, got %+v", textOnly)
	}
	if textOnly.Gate != GateIdentifier {
		t.Fatalf("expected the identifier gate to be terminal, got %q", textOnly.Gate)
	}

	withMeta := QualifyCandidate(BindDocument{
		Excerpt:  excerpt,
		Metadata: MetadataFields{{Field: "xmp/prism:doi", Value: "10.1234/target"}},
	}, candidate)
	if !withMeta.Qualifies || withMeta.Review {
		t.Fatalf("embedded metadata naming the candidate should satisfy the identifier gate, got %+v", withMeta)
	}
	var named bool
	for _, e := range withMeta.Evidence {
		if strings.Contains(e, "embedded metadata") {
			named = true
		}
	}
	if !named {
		t.Fatalf("evidence does not record that the decision rested on metadata: %v", withMeta.Evidence)
	}
}

// TestQualifyCandidateMetadataDoesNotBypassEarlierGates is the supplement fence,
// and the reason metadata joins gate 5 rather than short-circuiting the rule.
//
// A supplementary-materials PDF ordinarily carries its PARENT article's DOI in
// its XMP packet, because the publisher produced it as part of that article. If
// metadata could authorise on its own, those bytes would be filed under the
// article's citation. Title, author and year must therefore still be shown to
// agree first — the earlier gates are what tell a supplement from its article.
func TestQualifyCandidateMetadataDoesNotBypassEarlierGates(t *testing.T) {
	parentDOI := MetadataFields{{Field: "xmp/prism:doi", Value: "10.1234/parent"}}
	candidate := BindCandidate{
		Key:  "job-1",
		Work: work.Work{Title: "Structural Priors in Retrieval Systems", Authors: []string{"Ciani"}, Year: 2019, DOI: "10.1234/parent"},
	}

	supplement := "Supplementary information for\n" +
		"Structural Priors in Retrieval Systems\n" +
		"Alice Ciani, Boris Random\n2019\n\nTable S1.\n"
	got := QualifyCandidate(BindDocument{Excerpt: supplement, Metadata: parentDOI}, candidate)
	if got.Qualifies {
		t.Fatalf("a supplement carrying its parent's DOI in metadata auto-bound: %+v", got)
	}
	if got.Gate == GateIdentifier {
		t.Fatalf("the supplement reached the identifier gate; a marker gate should have stopped it first: %+v", got)
	}

	// Same metadata, but nothing about the document agrees: the author gate is
	// the earliest one that can refuse, and metadata must not rescue it.
	unrelated := "A Completely Different Paper\nZoe Nobody\n1998\n\nAbstract\n"
	if got := QualifyCandidate(BindDocument{Excerpt: unrelated, Metadata: parentDOI}, candidate); got.Qualifies {
		t.Fatalf("metadata alone bound an unrelated document: %+v", got)
	}
}
