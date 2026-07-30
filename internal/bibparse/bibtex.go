package bibparse

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseBibTeX extracts acquisition identity from BibTeX and BibLaTeX entries.
func parseBibTeX(data []byte) ([]Record, error) {
	input := strings.TrimPrefix(string(data), "\ufeff")
	if strings.TrimSpace(input) == "" {
		return nil, noEntries("bibtex: no entries found")
	}

	var records []Record

	for offset := 0; offset < len(input); {
		at := strings.IndexByte(input[offset:], '@')
		if at < 0 {
			break
		}
		at += offset

		typeStart := at + 1
		typeEnd := typeStart
		for typeEnd < len(input) && isBibTeXTypeChar(input[typeEnd]) {
			typeEnd++
		}
		if typeEnd == typeStart {
			return nil, fmt.Errorf("bibtex: entry at byte %d: missing entry type", at)
		}

		open := typeEnd
		for open < len(input) && isBibTeXSpace(input[open]) {
			open++
		}
		if open == len(input) || input[open] != '{' {
			return nil, fmt.Errorf("bibtex: entry at byte %d: expected opening brace", at)
		}

		entry, next, err := bibTeXBraced(input, open)
		if err != nil {
			return nil, fmt.Errorf("bibtex: entry at byte %d: %w", at, err)
		}
		offset = next

		typ := strings.ToLower(input[typeStart:typeEnd])
		if typ == "comment" || typ == "preamble" || typ == "string" {
			continue
		}

		record, err := parseBibTeXEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("bibtex: entry at byte %d: %w", at, err)
		}
		records = append(records, record)
	}

	if len(records) == 0 {
		// Only truly empty input is a complete empty source. A non-empty source
		// with no bibliographic entries leaves callers unable to distinguish
		// metadata-only input from unsupported or malformed content.
		return nil, errors.New("bibtex: no entries found")
	}
	return records, nil
}

func parseBibTeXEntry(entry string) (record Record, err error) {
	comma := bibTeXTopLevelComma(entry, 0)
	if comma < 0 {
		return Record{}, errors.New("missing comma after citation key")
	}
	if equals := bibTeXTopLevelEquals(entry, 0); equals >= 0 && equals < comma {
		return Record{}, errors.New("field assignment before citation key comma")
	}

	// The eprint metadata is resolved on every successful exit, since the parse
	// loop returns from several points and declarations may follow `eprint`.
	var eprint bibTeXEprint
	defer func() {
		if err == nil {
			resolveBibTeXEprint(&record, eprint)
		}
	}()

	for pos := comma + 1; ; {
		pos = skipBibTeXDelimiters(entry, pos)
		if pos >= len(entry) {
			return record, nil
		}

		equals := bibTeXTopLevelEquals(entry, pos)
		if equals < 0 {
			return Record{}, errors.New("missing field separator")
		}
		field := strings.ToLower(strings.TrimSpace(entry[pos:equals]))
		if field == "" {
			return Record{}, errors.New("empty field name")
		}

		value, next, err := bibTeXValue(entry, equals+1)
		if err != nil {
			return Record{}, fmt.Errorf("field %q: %w", field, err)
		}
		applyBibTeXField(&record, &eprint, field, value)

		pos = skipBibTeXSpace(entry, next)
		if pos == len(entry) {
			return record, nil
		}
		if entry[pos] != ',' {
			return Record{}, fmt.Errorf("field %q: expected comma", field)
		}
	}
}

func bibTeXValue(entry string, pos int) (string, int, error) {
	pos = skipBibTeXSpace(entry, pos)
	if pos >= len(entry) {
		return "", pos, errors.New("missing value")
	}

	var value strings.Builder
	for {
		atom, next, err := bibTeXValueAtom(entry, pos)
		if err != nil {
			return "", pos, err
		}
		value.WriteString(atom)
		pos = skipBibTeXSpace(entry, next)
		if pos >= len(entry) || entry[pos] != '#' {
			return value.String(), pos, nil
		}
		pos = skipBibTeXSpace(entry, pos+1)
		if pos >= len(entry) {
			return "", pos, errors.New("missing value after concatenation")
		}
	}
}

func bibTeXValueAtom(entry string, pos int) (string, int, error) {
	switch entry[pos] {
	case '{':
		return bibTeXBraced(entry, pos)
	case '"':
		return bibTeXQuoted(entry, pos)
	default:
		end := pos
		for end < len(entry) && entry[end] != ',' && entry[end] != '#' && !isBibTeXSpace(entry[end]) {
			end++
		}
		if end == pos {
			return "", pos, errors.New("missing value")
		}
		return entry[pos:end], end, nil
	}
}

// bibTeXEprint accumulates eprint metadata across one entry's fields, because
// every archiveprefix and eprinttype declaration must be considered even when
// the same field occurs more than once.
type bibTeXEprint struct {
	id             string
	hasForeignType bool
}

func applyBibTeXField(record *Record, eprint *bibTeXEprint, field, value string) {
	switch field {
	case "doi":
		record.DOI = normalizeBibTeXDOI(cleanBibTeXValue(value))
	case "pmid":
		record.PMID = cleanBibTeXValue(value)
	case "arxiv":
		// An explicit arxiv field names its archive; no prefix needed.
		record.ArXiv = cleanBibTeXValue(value)
	case "eprint":
		eprint.id = cleanBibTeXValue(value)
	case "archiveprefix", "eprinttype":
		archive := cleanBibTeXValue(value)
		if archive != "" && !strings.EqualFold(archive, "arxiv") {
			eprint.hasForeignType = true
		}
	case "title":
		record.Title = cleanBibTeXValue(value)
	case "author":
		for _, author := range splitBibTeXAuthors(value) {
			if author = cleanBibTeXValue(author); author != "" {
				record.Authors = append(record.Authors, author)
			}
		}
	case "year":
		record.Year = bibTeXYear(cleanBibTeXValue(value))
	}
}

// resolveBibTeXEprint promotes an eprint id to an arXiv id only when every
// explicit archive declaration says arXiv, or neither declaration is present.
// BibLaTeX's `eprint` is archive-agnostic — it also carries JSTOR, HAL, and
// PubMed ids — so one foreign `archiveprefix` or `eprinttype` must prevent
// promotion regardless of field order or duplicates. An explicit arxiv field,
// already applied, wins.
func resolveBibTeXEprint(record *Record, eprint bibTeXEprint) {
	if record.ArXiv != "" || eprint.id == "" || eprint.hasForeignType {
		return
	}
	record.ArXiv = eprint.id
}

func normalizeBibTeXDOI(value string) string {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://doi.org/", "doi:"} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	return value
}

func bibTeXYear(value string) int {
	for start := range len(value) - 3 {
		if !isBibTeXDigit(value[start]) || !isBibTeXDigit(value[start+1]) || !isBibTeXDigit(value[start+2]) || !isBibTeXDigit(value[start+3]) {
			continue
		}
		year, _ := strconv.Atoi(value[start : start+4])
		return year
	}
	return 0
}

func splitBibTeXAuthors(value string) []string {
	var authors []string
	start, depth := 0, 0
	for pos := 0; pos < len(value); pos++ {
		switch value[pos] {
		case '\\':
			if pos+1 < len(value) {
				pos++
			}
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		default:
			// BibTeX treats the name separator case-insensitively ("and",
			// "AND", "And" all delimit).
			if depth == 0 && pos > start && isBibTeXSpace(value[pos-1]) && pos+3 < len(value) && strings.EqualFold(value[pos:pos+3], "and") && isBibTeXSpace(value[pos+3]) {
				authors = append(authors, value[start:pos])
				pos += 3
				start = pos + 1
			}
		}
	}
	authors = append(authors, value[start:])
	return authors
}

func cleanBibTeXValue(value string) string {
	value = strings.ReplaceAll(value, "---", "-")
	value = strings.ReplaceAll(value, "--", "-")

	var cleaned strings.Builder
	for pos := 0; pos < len(value); pos++ {
		switch value[pos] {
		case '{', '}':
			continue
		case '\\':
			if pos+1 >= len(value) {
				continue
			}
			pos++
			if isBibTeXLetter(value[pos]) {
				for pos+1 < len(value) && isBibTeXLetter(value[pos+1]) {
					pos++
				}
			}
			continue
		default:
			cleaned.WriteByte(value[pos])
		}
	}
	return strings.Join(strings.Fields(cleaned.String()), " ")
}

func bibTeXBraced(value string, start int) (string, int, error) {
	depth := 0
	for pos := start; pos < len(value); pos++ {
		switch value[pos] {
		case '\\':
			if pos+1 < len(value) {
				pos++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return value[start+1 : pos], pos + 1, nil
			}
		}
	}
	return "", start, errors.New("unterminated braced value")
}

func bibTeXQuoted(value string, start int) (string, int, error) {
	for pos := start + 1; pos < len(value); pos++ {
		if value[pos] == '\\' && pos+1 < len(value) {
			pos++
			continue
		}
		if value[pos] == '"' {
			return value[start+1 : pos], pos + 1, nil
		}
	}
	return "", start, errors.New("unterminated quoted value")
}

func bibTeXTopLevelComma(value string, start int) int {
	return bibTeXTopLevel(value, start, ',')
}

func bibTeXTopLevelEquals(value string, start int) int {
	return bibTeXTopLevel(value, start, '=')
}

func bibTeXTopLevel(value string, start int, target byte) int {
	depth := 0
	quoted := false
	for pos := start; pos < len(value); pos++ {
		switch value[pos] {
		case '\\':
			if pos+1 < len(value) {
				pos++
			}
		case '"':
			quoted = !quoted
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		default:
			if !quoted && depth == 0 && value[pos] == target {
				return pos
			}
		}
	}
	return -1
}

func skipBibTeXDelimiters(value string, pos int) int {
	for pos < len(value) && (isBibTeXSpace(value[pos]) || value[pos] == ',') {
		pos++
	}
	return pos
}

func skipBibTeXSpace(value string, pos int) int {
	for pos < len(value) && isBibTeXSpace(value[pos]) {
		pos++
	}
	return pos
}

func isBibTeXTypeChar(value byte) bool {
	return isBibTeXLetter(value) || isBibTeXDigit(value) || value == '-' || value == '_'
}

func isBibTeXLetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isBibTeXDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isBibTeXSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}
