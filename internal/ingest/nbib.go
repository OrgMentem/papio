package ingest

import (
	"fmt"
	"strings"
)

// parseNBIB parses MEDLINE/PubMed NBIB records into acquisition identities.
func parseNBIB(data []byte) ([]Record, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var records []Record
	var current nbibRecord
	var currentTag, fieldValue string

	flush := func() {
		if !current.seen {
			return
		}
		if !current.hasFAU {
			current.record.Authors = current.au
		}
		records = append(records, current.record)
		current = nbibRecord{}
		currentTag = ""
		fieldValue = ""
	}

	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}

		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !current.seen || currentTag == "" {
				return nil, fmt.Errorf("nbib: continuation without a field")
			}
			continuation := strings.TrimSpace(line)
			fieldValue = joinNBIBField(fieldValue, continuation)
			current.appendContinuation(currentTag, continuation, fieldValue)
			continue
		}

		tag, value, ok := splitNBIBField(line)
		if !ok {
			return nil, fmt.Errorf("nbib: malformed line %q", line)
		}
		if tag == "PMID" && current.seen {
			flush()
		}
		current.seen = true
		currentTag = tag
		fieldValue = value
		current.addField(tag, value)
	}
	flush()

	if len(records) == 0 {
		return nil, fmt.Errorf("nbib: no records found")
	}
	return records, nil
}

type nbibRecord struct {
	record Record
	au     []string
	hasFAU bool
	seen   bool
}

func (r *nbibRecord) addField(tag, value string) {
	switch tag {
	case "PMID":
		r.record.PMID = value
	case "TI":
		r.record.Title = value
	case "FAU":
		r.hasFAU = true
		if value != "" {
			r.record.Authors = append(r.record.Authors, value)
		}
	case "AU":
		if value != "" {
			r.au = append(r.au, value)
		}
	case "DP":
		r.record.Year = nbibYear(value)
	case "LID", "AID":
		r.setDOI(value)
	}
}

func (r *nbibRecord) appendContinuation(tag, value, fieldValue string) {
	if value == "" {
		return
	}
	switch tag {
	case "PMID":
		r.record.PMID = joinNBIBField(r.record.PMID, value)
	case "TI":
		r.record.Title = joinNBIBField(r.record.Title, value)
	case "FAU":
		if len(r.record.Authors) > 0 {
			r.record.Authors[len(r.record.Authors)-1] = joinNBIBField(r.record.Authors[len(r.record.Authors)-1], value)
		} else {
			r.record.Authors = append(r.record.Authors, value)
		}
	case "AU":
		if len(r.au) > 0 {
			r.au[len(r.au)-1] = joinNBIBField(r.au[len(r.au)-1], value)
		} else {
			r.au = append(r.au, value)
		}
	case "DP":
		if r.record.Year == 0 {
			r.record.Year = nbibYear(value)
		}
	case "LID", "AID":
		r.setDOI(fieldValue)
	}
}

func (r *nbibRecord) setDOI(value string) {
	if r.record.DOI != "" || !strings.HasSuffix(value, " [doi]") {
		return
	}
	r.record.DOI = strings.TrimSuffix(value, " [doi]")
}
func joinNBIBField(value, continuation string) string {
	if value == "" {
		return continuation
	}
	return value + " " + continuation
}

func splitNBIBField(line string) (tag, value string, ok bool) {
	hyphen := strings.IndexByte(line, '-')
	if hyphen < 1 {
		return "", "", false
	}
	tag = strings.TrimSpace(line[:hyphen])
	if tag == "" || len(tag) > 4 {
		return "", "", false
	}
	for _, char := range tag {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return "", "", false
		}
	}
	return tag, strings.TrimSpace(line[hyphen+1:]), true
}

func nbibYear(value string) int {
	for i := 0; i+3 < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			continue
		}
		if value[i+1] >= '0' && value[i+1] <= '9' &&
			value[i+2] >= '0' && value[i+2] <= '9' &&
			value[i+3] >= '0' && value[i+3] <= '9' {
			return int(value[i]-'0')*1000 + int(value[i+1]-'0')*100 + int(value[i+2]-'0')*10 + int(value[i+3]-'0')
		}
	}
	return 0
}
