// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"encoding/xml"
	"sort"
	"strings"

	"papio/internal/work"
)

// Embedded metadata is a second, independent place a PDF names itself, and it
// answers a question the text cannot.
//
// The candidate-binding predicate's hard problem is ATTRIBUTION: a DOI printed
// on page one may be the document's own or a citation of someone else's, and
// separating those from a flat text string is the whole difficulty (see
// candidate_select.go gate 5, and the identity-attribution plan under
// dev/active). Embedded metadata sidesteps it. A publisher writes prism:doi
// during production to state "this file is that DOI"; a reference list cannot
// appear there, because nothing in a document's body reaches its Info
// dictionary or its XMP packet. The field's semantics carry the attribution
// that page-one text only implies.
//
// Measured over the operator's library, restricted to the population that
// actually reaches candidate binding (empty front-matter DOI window, so the
// blind path did not already name the work): 110 of 322 documents (34.2%) name
// their own identifier in metadata, and 101 of those do NOT name it anywhere in
// the 2 KiB byline window that structural attribution would read. Checking
// every one of those documents against every other identified work in the
// library — roughly 185k pairs — found no document whose metadata named a
// DIFFERENT work.
//
// Three limits are deliberate, and each is a safety property rather than an
// omission:
//
//  1. ALLOWLISTED FIELDS ONLY, never the whole metadata blob. Free-text fields
//     (Subject, Title, Description) frequently contain the DOI too, but they
//     carry no claim about WHOSE it is; an aggregator can put anything there.
//     Only fields whose defined meaning is "this document's identifier" are
//     read. This costs coverage on purpose.
//  2. TARGET-AWARE ONLY. NamesWork asks "does this document's metadata name the
//     CANDIDATE's identifier?" There is deliberately no function that reads an
//     identifier OUT of metadata to mint one, because a production template
//     error or an aggregator rewrite would then name a work with nothing to
//     check it against — the FrontMatterDOIs hazard with a new source.
//  3. CORROBORATION, NEVER AUTHORITY. Gate 5 accepts metadata in place of a
//     page-one printed identifier, and nothing else. Title, author and year
//     must still agree, which is what keeps a supplement — whose XMP ordinarily
//     carries its PARENT article's DOI — from binding as the article.
//
// The one exposure that measurement here cannot bound: unlike text, which must
// survive visible typesetting, metadata is invisible and freely writable by
// whoever produced the file.

// MetadataField is one embedded-metadata value, from a field whose defined
// semantics name THIS document. Field is prefixed with its source ("xmp/" or
// "info/") so evidence says where a decision's support came from: a claim in
// prism:doi and a coincidence in a free-text field are not the same evidence,
// and only the former is ever collected.
type MetadataField struct {
	Field string
	Value string
}

// MetadataFields is a document's allowlisted embedded metadata. Empty means the
// file carried none that names itself, which is the ordinary case for scans and
// for anything a publisher did not produce — the predicate then behaves exactly
// as it did before metadata existed.
type MetadataFields []MetadataField

// identifierFields are the metadata fields whose DEFINED meaning is "the
// identifier of the document you are reading". Membership is the entire safety
// argument for treating a hit as self-attribution, so this list grows only with
// a documented publisher schema and a measurement, never to chase coverage.
//
// prism:* is PRISM (Publishing Requirements for Industry Standard Metadata),
// crossmark:* is Crossref's CrossMark, pdfx:* is Adobe's custom-schema
// namespace as used by publisher production tools, and dc:identifier is the
// Dublin Core element for "an unambiguous reference to the resource".
//
// Names are the lowercased "prefix:local" form xmpElementName resolves a
// namespace URI to — never the prefix as written in the file, which is
// arbitrary — and are compared lowercased because publishers disagree about
// case (crossmark:DOI and crossmark:doi both occur).
//
// Only XMP is read. The PDF specification gives the Info dictionary no
// identifier-semantic key, and measuring the operator's library confirmed the
// consequence: every Info-dict hit was in free text (Subject, Title), which
// carries no claim about whose identifier it is. So there is nothing in that
// dictionary this may trust, and it is not read at all rather than read and
// filtered to nothing.
var identifierFields = map[string]bool{
	"prism:doi":           true,
	"prism:url":           true,
	"crossmark:doi":       true,
	"crossmark:doiurl":    true,
	"pdfx:doi":            true,
	"pdfx:wps-articledoi": true,
	"dc:identifier":       true,
	"dcterms:identifier":  true,
	// Deliberately absent: prism:versionIdentifier names the version rather
	// than the article, and every free-text field (dc:title, dc:description,
	// dc:subject, prism:teaser) can hold anything an aggregator put there.
}

// xmpValueContainers are the RDF wrapper elements a value may sit inside. XMP
// writes a repeated or language-alternative property as
// <dc:identifier><rdf:Bag><rdf:li>10.x/y</rdf:li></rdf:Bag></dc:identifier>, so
// the text node's own element name is rdf:li and says nothing. Attribution
// therefore follows the nearest enclosing element that is NOT one of these —
// reading the value's own container as its field would count an identifier
// found in dc:subject exactly like one in dc:identifier.
var xmpValueContainers = map[string]bool{
	"rdf:bag":         true,
	"rdf:seq":         true,
	"rdf:alt":         true,
	"rdf:li":          true,
	"rdf:value":       true,
	"rdf:rdf":         true,
	"x:xmpmeta":       true,
	"rdf:description": true,
}

// ExtractMetadata reads a file's XMP packet through pdfinfo, in one bounded
// subprocess, and keeps only allowlisted fields.
//
// Absence is not an error at any level: no pdfinfo, an unreadable packet,
// malformed XML, or a file with no metadata at all each yield empty fields and
// a nil error, because every one of them means the same thing to the caller —
// there is no metadata evidence, decide on text alone. A hard error is returned
// only when the context is done, which the caller must not paper over.
func ExtractMetadata(ctx context.Context, path string, capability Capability, opt SemanticOptions) (MetadataFields, error) {
	opt = normalizedSemanticOptions(opt)
	if capability.PDFInfo == "" {
		return nil, nil
	}
	packet, err := runTextTool(ctx, opt.Timeout, opt.MaxOutputBytes, capability.PDFInfo, "-meta", path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	return parseXMP(packet), nil
}

// parseXMP keeps allowlisted values from an XMP packet, attributing each text
// node to the nearest enclosing element that is not an RDF container.
//
// Malformed and truncated packets are expected rather than exceptional: the
// output is bounded, so a large packet arrives cut mid-element, and publisher
// packets carry vendor extensions of varying quality. Parsing therefore stops
// at the first error and returns what it has, which is safe because every
// value it has already attributed came from a complete element.
func parseXMP(packet string) MetadataFields {
	dec := xml.NewDecoder(strings.NewReader(packet))
	dec.Strict = false
	var stack []string
	var fields MetadataFields
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			// Any error ends the packet: EOF is the ordinary end, and a
			// mid-element cut is why parsing keeps what it already has.
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, xmpElementName(t.Name))
			// A property may also carry its value as an attribute
			// (rdf:Description prism:doi="10.x/y"), which is the compact
			// RDF form and just as authoritative as the element form.
			for _, attr := range t.Attr {
				name := xmpElementName(attr.Name)
				value := strings.TrimSpace(attr.Value)
				if value == "" || !identifierFields[name] {
					continue
				}
				if key := name + "\x00" + value; !seen[key] {
					seen[key] = true
					fields = append(fields, MetadataField{Field: "xmp/" + name, Value: value})
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			value := strings.TrimSpace(string(t))
			if value == "" {
				continue
			}
			name := xmpEnclosingField(stack)
			if name == "" || !identifierFields[name] {
				continue
			}
			if key := name + "\x00" + value; !seen[key] {
				seen[key] = true
				fields = append(fields, MetadataField{Field: "xmp/" + name, Value: value})
			}
		}
	}
	return fields
}

// xmpElementName renders a parsed name as the lowercased "prefix:local" form
// the allowlist is written in. encoding/xml resolves a prefix to its namespace
// URI, so the prefix as written in the document is not recoverable from
// Name.Space; the schema URI is what actually identifies the vocabulary, and
// mapping it back to a conventional prefix is what keeps the allowlist readable.
func xmpElementName(n xml.Name) string {
	local := strings.ToLower(n.Local)
	space := strings.ToLower(n.Space)
	if space == "" {
		return local
	}
	for _, v := range xmpVocabularies {
		if strings.Contains(space, v.marker) {
			return v.prefix + ":" + local
		}
	}
	// An unknown vocabulary can never match the allowlist, and rendering it
	// with its full URI keeps that visible in evidence rather than silently
	// colliding with a known prefix's local name.
	return space + ":" + local
}

// xmpVocabularies maps the schemas papio recognises onto the prefixes
// identifierFields and xmpValueContainers are written in.
//
// Matching is by a stable substring of the namespace URI rather than the whole
// URI, because the URI carries a schema VERSION that publishers bump
// independently: PRISM alone ships basic/2.0, 2.1, 3.0 and pam/2.0, and a file
// declaring several of them is what produces the prism_1_ style prefixes seen in
// the wild. Exact-URI matching therefore fails closed on version churn —
// as a missed bind rather than a wrong one, which is the failure that hides.
//
// The prefix written in the file is never consulted: it is arbitrary, and two
// files may bind the same prefix to different schemas.
var xmpVocabularies = []struct {
	marker string
	prefix string
}{
	{"prismstandard.org", "prism"},
	{"crossref.org/crossmark", "crossmark"},
	{"ns.adobe.com/pdfx", "pdfx"},
	{"purl.org/dc/elements", "dc"},
	{"purl.org/dc/terms", "dcterms"},
	{"22-rdf-syntax-ns", "rdf"},
	{"adobe:ns:meta", "x"},
}

// xmpEnclosingField returns the nearest element on the stack that names a
// property rather than an RDF container, or "" when the value is not inside one.
func xmpEnclosingField(stack []string) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if !xmpValueContainers[stack[i]] {
			return stack[i]
		}
	}
	return ""
}

// NamesWork returns evidence when an allowlisted field carries target's
// identifier, or "" when none does.
//
// The comparison is corroboratingIdentifier — the same matcher the page-one
// text gate uses — for two reasons. It already tolerates the ways a real
// identifier is written (a full https://doi.org/ URL, letter spacing, a
// wrapped token) while requiring a COMPLETE token, so PMID 12345 does not match
// a field naming 123456. And single-sourcing it means metadata and text can
// never disagree about what counts as this work's identifier; two matchers that
// differ would be a new way to accept the wrong document.
//
// Fields are examined in canonical order, so the evidence string for a document
// that names itself several times does not depend on the order pdfinfo emitted
// its packet.
func (m MetadataFields) NamesWork(target work.Work) string {
	for _, f := range m.sorted() {
		if corroboratingIdentifier(f.Value, target) != "" {
			return "identifier in embedded metadata (" + f.Field + ")"
		}
	}
	return ""
}

// Canonical renders the fields as one deterministic string, for callers that
// must pin exactly what the predicate read — in practice the auto-bind
// provenance digest.
//
// It exists because that digest's promise is "the bytes the predicate read",
// and under candidate_auto_bind/3 the predicate reads embedded metadata as well
// as text. Hashing the excerpt alone would let two decisions that differed only
// in metadata — one binding, one parking — record the same digest, which is an
// audit trail that cannot reconstruct its own decision. The rule version is
// stored beside the digest, so a row records which definition applied to it.
func (m MetadataFields) Canonical() string {
	if len(m) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range m.sorted() {
		b.WriteString(f.Field)
		b.WriteByte('=')
		b.WriteString(f.Value)
		b.WriteByte('\n')
	}
	return b.String()
}

// sorted returns the fields in canonical order — allowlisted field name, then
// value — without mutating the receiver. Order is single-sourced here because
// evidence selection and the provenance digest must agree about it; two
// orderings would make a digest that does not correspond to the evidence stored
// next to it.
func (m MetadataFields) sorted() MetadataFields {
	ordered := make(MetadataFields, len(m))
	copy(ordered, m)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Field != ordered[j].Field {
			return ordered[i].Field < ordered[j].Field
		}
		return ordered[i].Value < ordered[j].Value
	})
	return ordered
}
