package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type cslJSONItem struct {
	DOI    string          `json:"DOI"`
	PMID   json.RawMessage `json:"PMID"`
	Title  string          `json:"title"`
	Author []cslJSONAuthor `json:"author"`
	Issued cslJSONIssued   `json:"issued"`
}

type cslJSONAuthor struct {
	Literal string `json:"literal"`
	Given   string `json:"given"`
	Family  string `json:"family"`
}

type cslJSONIssued struct {
	DateParts []json.RawMessage `json:"date-parts"`
}

func parseCSLJSON(data []byte) ([]Record, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return nil, fmt.Errorf("csl-json: expected a top-level array of items")
	}

	var items []cslJSONItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("csl-json: decode: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("csl-json: no items found")
	}

	records := make([]Record, 0, len(items))
	for _, item := range items {
		record := Record{
			DOI:     strings.TrimPrefix(item.DOI, "https://doi.org/"),
			PMID:    cslJSONValue(item.PMID),
			Title:   item.Title,
			Authors: cslJSONAuthors(item.Author),
			Year:    cslJSONYear(item.Issued.DateParts),
		}
		records = append(records, record)
	}
	return records, nil
}

func cslJSONAuthors(authors []cslJSONAuthor) []string {
	result := make([]string, 0, len(authors))
	for _, author := range authors {
		if author.Literal != "" {
			result = append(result, author.Literal)
			continue
		}
		name := strings.TrimSpace(strings.Join([]string{author.Given, author.Family}, " "))
		if name != "" {
			result = append(result, name)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func cslJSONYear(dateParts []json.RawMessage) int {
	if len(dateParts) == 0 {
		return 0
	}

	var firstPart []json.RawMessage
	if err := json.Unmarshal(dateParts[0], &firstPart); err != nil || len(firstPart) == 0 {
		return 0
	}

	year, err := strconv.Atoi(cslJSONValue(firstPart[0]))
	if err != nil {
		return 0
	}
	return year
}

func cslJSONValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}
