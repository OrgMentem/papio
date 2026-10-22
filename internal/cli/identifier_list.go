package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"papio/internal/protocol"
)

// listedIdentifier is one classified line of an identifier-list file.
type listedIdentifier struct {
	line int
	text string
	ids  *protocol.Identifiers
}

// isIdentifierList reports whether batch input that Detect left as JSONL is a
// plain identifier list: its first non-blank byte opens neither a JSON object
// nor a JSON array. Empty input stays on the JSONL path and its own error.
func isIdentifierList(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	return len(trimmed) != 0 && trimmed[0] != '{' && trimmed[0] != '['
}

var pubmedURLRE = regexp.MustCompile(`(?i)^https?://pubmed\.ncbi\.nlm\.nih\.gov/([0-9]+)/?(?:[?#].*)?$`)

// parseIdentifierLines classifies one identifier per line by reusing the
// positional classifier of `papio acquire <arg>`. Blank and `#` lines are
// skipped. Any unclassifiable line refuses the whole file, naming its number.
func parseIdentifierLines(data []byte) ([]listedIdentifier, error) {
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\ufeff"))))
	scanner.Buffer(make([]byte, 4<<10), 1<<20)
	var listed []listedIdentifier
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		arg := text
		if m := pubmedURLRE.FindStringSubmatch(text); m != nil {
			arg = "pmid:" + m[1]
		}
		ids, err := normalizeIdentifiers([]string{arg}, "", "", "", "", "")
		if err != nil {
			return nil, fmt.Errorf("identifier list line %d: %w", line, err)
		}
		if ids == nil {
			return nil, fmt.Errorf("identifier list line %d: no identifier in %q", line, text)
		}
		listed = append(listed, listedIdentifier{line: line, text: text, ids: ids})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading identifier list: %w", err)
	}
	return listed, nil
}
