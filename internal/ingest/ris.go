package ingest

import (
	"errors"
	"strings"
)

// parseRIS extracts acquisition identity from RIS tagged records.
func parseRIS(data []byte) ([]Record, error) {
	lines := strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n")
	var records []Record
	var current risRecord
	inRecord := false

	finish := func() {
		if !inRecord {
			return
		}
		current.record.DOI = normalizeRISDOI(current.record.DOI)
		if current.title != "" {
			current.record.Title = current.title
		} else {
			current.record.Title = current.titleFallback
		}
		records = append(records, current.record)
		current = risRecord{}
		inRecord = false
	}

	for _, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\r")
		if tag, value, ok := risTag(line); ok {
			switch tag {
			case "TY":
				finish()
				inRecord = true
				current.last = risNone
			case "ER":
				finish()
			default:
				if inRecord {
					current.add(tag, strings.TrimSpace(value))
				}
			}
			continue
		}

		if inRecord && current.last != risNone {
			content := strings.TrimSpace(line)
			if content != "" {
				current.appendContinuation(content)
			}
		}
	}
	finish()

	if len(records) == 0 {
		return nil, errRISNoRecords
	}
	return records, nil
}

var errRISNoRecords = errors.New("ris: no records found")

type risValue uint8

const (
	risNone risValue = iota
	risDOI
	risTitle
	risTitleFallback
	risAuthor
)

type risRecord struct {
	record        Record
	title         string
	titleFallback string
	last          risValue
}

func (record *risRecord) add(tag, value string) {
	record.last = risNone
	switch tag {
	case "DO":
		record.record.DOI = value
		record.last = risDOI
	case "TI":
		record.title = value
		record.last = risTitle
	case "T1":
		record.titleFallback = value
		record.last = risTitleFallback
	case "AU", "A1":
		record.record.Authors = append(record.record.Authors, value)
		record.last = risAuthor
	case "PY", "Y1":
		record.record.Year = risYear(value)
	}
}

func (record *risRecord) appendContinuation(content string) {
	switch record.last {
	case risDOI:
		record.record.DOI += " " + content
	case risTitle:
		record.title += " " + content
	case risTitleFallback:
		record.titleFallback += " " + content
	case risAuthor:
		last := len(record.record.Authors) - 1
		record.record.Authors[last] += " " + content
	}
}

func risTag(line string) (tag, value string, ok bool) {
	if len(line) < 6 || line[2:6] != "  - " {
		return "", "", false
	}
	return line[:2], line[6:], true
}

func normalizeRISDOI(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://doi.org/", "doi:"} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	return value
}

func risYear(value string) int {
	for i := 0; i+4 <= len(value); i++ {
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
