// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package cite projects papio's canonical work metadata into citation
// formats. The projections are normalized and deterministic, never a
// round-trip: only known values are exported, author names stay literal
// (papio stores flat name strings and splitting them into family/given
// would be inference), and nothing — type, journal, pages, publisher — is
// ever guessed from title text (r2 §4A of the integration consult;
// dev/scratch/oracle/papio-integrations-r2.md).
package cite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"papio/internal/store"
	"papio/internal/work"
)

// Record is one normalized citation: the subset of bibliographic fields
// papio actually persists, plus nothing. Absent values stay empty and every
// projection omits them.
type Record struct {
	Title     string
	Authors   []string // literal names in stored order
	Year      int
	Container string
	DOI       string
	PMID      string
	ArXiv     string
	ISBN      string
	Abstract  string
}

// FromWork projects the canonical work fields into a Record.
func FromWork(w work.Work) Record {
	authors := make([]string, 0, len(w.Authors))
	for _, author := range w.Authors {
		if name := strings.TrimSpace(author); name != "" {
			authors = append(authors, name)
		}
	}
	return Record{
		Title:     strings.TrimSpace(w.Title),
		Authors:   authors,
		Year:      w.Year,
		Container: strings.TrimSpace(w.Container),
		DOI:       strings.TrimSpace(w.DOI),
		PMID:      strings.TrimSpace(w.PMID),
		ArXiv:     strings.TrimSpace(w.ArXiv),
		ISBN:      strings.TrimSpace(w.ISBN),
	}
}

// itemType maps identifiers to a citation type. Identifier-based typing only:
// an ISBN with neither DOI nor container is a book, everything else is
// treated as a journal-style article. Title text never participates.
func (r Record) itemType() string {
	if r.ISBN != "" && r.DOI == "" && r.Container == "" {
		return "book"
	}
	return "article-journal"
}

// Identity is the canonical duplicate identity for one record: DOI, then
// PMID, then arXiv id, then normalized title + first author + year. Records
// with equal identities describe the same work for export purposes.
func (r Record) Identity() string {
	if r.DOI != "" {
		return "doi:" + strings.ToLower(r.DOI)
	}
	if r.PMID != "" {
		return "pmid:" + r.PMID
	}
	if r.ArXiv != "" {
		return "arxiv:" + strings.ToLower(r.ArXiv)
	}
	first := ""
	if len(r.Authors) > 0 {
		first = r.Authors[0]
	}
	return "meta:" + normalizeKeyText(r.Title) + "|" + normalizeKeyText(first) + "|" + strconv.Itoa(r.Year)
}

// Key is the stable citation key: firstauthor-year-titleword-hash6. The
// six-character identity hash keeps the key stable across small title
// corrections and collision-free across large exports.
func (r Record) Key() string {
	author := "anon"
	if len(r.Authors) > 0 {
		if tokens := strings.Fields(normalizeKeyText(r.Authors[0])); len(tokens) > 0 {
			author = tokens[len(tokens)-1]
		}
	}
	year := "nd"
	if r.Year > 0 {
		year = strconv.Itoa(r.Year)
	}
	word := "untitled"
	for _, token := range strings.Fields(normalizeKeyText(r.Title)) {
		if len(token) > 3 {
			word = token
			break
		}
	}
	sum := sha256.Sum256([]byte(r.Identity()))
	return author + "-" + year + "-" + word + "-" + hex.EncodeToString(sum[:])[:6]
}

// Dedupe collapses records with equal identities, keeping the first
// occurrence in scope order. The second return is how many were collapsed.
func Dedupe(records []Record) ([]Record, int) {
	seen := make(map[string]bool, len(records))
	kept := make([]Record, 0, len(records))
	for _, record := range records {
		identity := record.Identity()
		if seen[identity] {
			continue
		}
		seen[identity] = true
		kept = append(kept, record)
	}
	return kept, len(records) - len(kept)
}

// CSLJSON renders the records as a CSL-JSON array — the highest-fidelity
// projection. Author names are emitted as literal names, and papio-specific
// identifiers ride in the spec's custom object.
func CSLJSON(records []Record) ([]byte, error) {
	items := make([]map[string]any, 0, len(records))
	for _, r := range records {
		item := map[string]any{
			"type": r.itemType(),
			"id":   r.Key(),
		}
		if r.Title != "" {
			item["title"] = r.Title
		}
		if len(r.Authors) > 0 {
			authors := make([]map[string]string, 0, len(r.Authors))
			for _, name := range r.Authors {
				authors = append(authors, map[string]string{"literal": name})
			}
			item["author"] = authors
		}
		if r.Year > 0 {
			item["issued"] = map[string]any{"date-parts": [][]int{{r.Year}}}
		}
		if r.Container != "" {
			item["container-title"] = r.Container
		}
		if r.DOI != "" {
			item["DOI"] = r.DOI
		}
		if r.ISBN != "" {
			item["ISBN"] = r.ISBN
		}
		if r.Abstract != "" {
			item["abstract"] = r.Abstract
		}
		if r.PMID != "" {
			// PMID is a standard CSL 1.0.2 variable; only arXiv, which has
			// no standard slot, rides in the spec's custom object.
			item["PMID"] = r.PMID
		}
		if r.ArXiv != "" {
			item["custom"] = map[string]string{"arxiv": r.ArXiv}
		}
		items = append(items, item)
	}
	return json.MarshalIndent(items, "", "  ")
}

// RIS renders the records as an RIS document — a deterministic, lossy
// projection. Values are single-line by construction; tags follow the RIS
// two-letter registry and unknown identifiers use the generic accession
// slots reference managers expect.
func RIS(records []Record) []byte {
	var b strings.Builder
	for _, r := range records {
		risType := "JOUR"
		if r.itemType() == "book" {
			risType = "BOOK"
		}
		writeRIS(&b, "TY", risType)
		writeRIS(&b, "TI", r.Title)
		for _, author := range r.Authors {
			writeRIS(&b, "AU", author)
		}
		if r.Year > 0 {
			writeRIS(&b, "PY", strconv.Itoa(r.Year))
		}
		writeRIS(&b, "T2", r.Container)
		if r.DOI != "" {
			writeRIS(&b, "DO", r.DOI)
		}
		if r.ISBN != "" {
			writeRIS(&b, "SN", r.ISBN)
		}
		if r.PMID != "" {
			writeRIS(&b, "AN", "PMID:"+r.PMID)
		}
		if r.ArXiv != "" {
			writeRIS(&b, "C1", "arXiv:"+r.ArXiv)
		}
		writeRIS(&b, "AB", r.Abstract)
		writeRIS(&b, "ID", r.Key())
		b.WriteString("ER  - \r\n")
	}
	return []byte(b.String())
}

// BibTeX renders the records as a BibTeX document — a deterministic, lossy
// projection with stable keys. Values are brace-wrapped with the reserved
// characters escaped, so a title containing % or & survives compilation.
func BibTeX(records []Record) []byte {
	var b strings.Builder
	for index, r := range records {
		if index > 0 {
			b.WriteString("\n")
		}
		entryType := "article"
		if r.itemType() == "book" {
			entryType = "book"
		}
		fmt.Fprintf(&b, "@%s{%s,\n", entryType, r.Key())
		fields := make([]string, 0, 8)
		addRaw := func(name, value string) {
			if value != "" {
				fields = append(fields, "  "+name+" = {"+value+"}")
			}
		}
		add := func(name, value string) {
			addRaw(name, escapeBibTeX(value))
		}
		add("title", r.Title)
		addRaw("author", bibTeXAuthors(r.Authors))
		if r.Year > 0 {
			add("year", strconv.Itoa(r.Year))
		}
		if r.itemType() == "book" {
			add("isbn", r.ISBN)
		} else {
			add("journal", r.Container)
			add("isbn", r.ISBN)
		}
		add("doi", r.DOI)
		if r.ArXiv != "" {
			add("eprint", r.ArXiv)
			add("archiveprefix", "arXiv")
		}
		add("pmid", r.PMID)
		add("abstract", r.Abstract)
		if len(fields) == 0 {
			b.WriteString("}\n")
			continue
		}
		b.WriteString(strings.Join(fields, ",\n"))
		b.WriteString(",\n}\n")
	}
	return []byte(b.String())
}

func writeRIS(b *strings.Builder, tag, value string) {
	value = singleLine(value)
	if value == "" {
		return
	}
	b.WriteString(tag)
	b.WriteString("  - ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// singleLine collapses whitespace and, per the repo-wide rule in
// store.StripTerminalControls's doc comment, routes every projected value
// through the one control-byte filter: exported titles are third-party
// text, and the no-`-o` path writes them straight to the operator's
// terminal. (CSL-JSON is exempt by construction — encoding/json escapes
// control bytes.)
func singleLine(value string) string {
	return strings.Join(strings.Fields(store.StripTerminalControls(value)), " ")
}

var bibtexEscaper = strings.NewReplacer(
	"\\", "\\textbackslash{}",
	"{", "\\{",
	"}", "\\}",
	"%", "\\%",
	"&", "\\&",
	"$", "\\$",
	"#", "\\#",
	"_", "\\_",
	"~", "\\textasciitilde{}",
	"^", "\\textasciicircum{}",
)

// bibTeXAuthors renders author names for BibTeX. Each name is wrapped in its
// own braces because BibTeX reads a top-level " and " inside the field value as
// the author separator, so a literal name such as "Research and Development,
// Ada" would otherwise import as two authors and silently change the citation.
func bibTeXAuthors(authors []string) string {
	names := make([]string, 0, len(authors))
	for _, author := range authors {
		escaped := escapeBibTeX(author)
		if escaped == "" {
			continue
		}
		names = append(names, "{"+escaped+"}")
	}
	return strings.Join(names, " and ")
}

func escapeBibTeX(value string) string {
	return bibtexEscaper.Replace(singleLine(value))
}

// normalizeKeyText lowercases and strips non-alphanumeric runes so keys and
// metadata identities survive punctuation and case differences.
func normalizeKeyText(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Formats lists the supported export formats in documentation order.
func Formats() []string {
	formats := []string{"csl-json", "ris", "bibtex"}
	sort.Strings(formats)
	return formats
}

// Render projects records into the named format.
func Render(format string, records []Record) ([]byte, error) {
	switch format {
	case "csl-json":
		return CSLJSON(records)
	case "ris":
		return RIS(records), nil
	case "bibtex":
		return BibTeX(records), nil
	default:
		return nil, fmt.Errorf("unsupported export format %q (supported: %s)", format, strings.Join(Formats(), ", "))
	}
}
