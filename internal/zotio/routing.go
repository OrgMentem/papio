// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"fmt"
	"net/url"
	"strings"

	"papio/internal/work"
)

const newItemRoutingRefusal = "new-item Zotio routing requires a DOI, PMID, arXiv ID, or ISBN"

// lookupWorkFrom copies the stable identifiers papio uses for new-item Zotio
// routing and library ownership checks. Title is deliberately excluded: it
// describes works rather than asserting identity (ADR-0019).
func lookupWorkFrom(w work.Work) LookupWork {
	lw := LookupWork{DOI: w.DOI, ArXiv: w.ArXiv, PMID: w.PMID}
	if isbn := normalizedISBN(w.ISBN); isbn != "" {
		lw.ISBN = isbn
	}
	return lw
}

func hasNewItemRoutingIdentifier(w work.Work) bool {
	lw := lookupWorkFrom(w)
	return lw.DOI != "" || lw.ArXiv != "" || lw.PMID != "" || lw.ISBN != ""
}

func importStagingBasename(w work.Work) (string, error) {
	switch {
	case strings.TrimSpace(w.DOI) != "":
		doi, err := work.NormalizeDOI(w.DOI)
		if err != nil {
			return "", fmt.Errorf("normalizing DOI for import staging: %w", err)
		}
		return url.PathEscape(strings.ToLower(doi)) + ".pdf", nil
	case strings.TrimSpace(w.ArXiv) != "":
		arxiv, err := work.NormalizeArXiv(w.ArXiv)
		if err != nil {
			return "", fmt.Errorf("normalizing arXiv ID for import staging: %w", err)
		}
		return "arxiv-" + url.PathEscape(strings.ToLower(arxiv)) + ".pdf", nil
	case strings.TrimSpace(w.PMID) != "":
		pmid, err := work.NormalizePMID(w.PMID)
		if err != nil {
			return "", fmt.Errorf("normalizing PMID for import staging: %w", err)
		}
		return "pmid-" + pmid + ".pdf", nil
	default:
		if isbn := normalizedISBN(w.ISBN); isbn != "" {
			return "isbn-" + isbn + ".pdf", nil
		}
		return "", fmt.Errorf(newItemRoutingRefusal)
	}
}
