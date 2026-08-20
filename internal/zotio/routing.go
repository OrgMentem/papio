// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"papio/internal/work"
)

const newItemRoutingRefusal = "new-item Zotio routing requires a DOI, PMID, arXiv ID, or ISBN"

// newItemRoute is the zotio item-creation route papio asks for when it creates
// a Zotero item. "auto" uses the local Zotero desktop when it is reachable and
// falls back to api.zotero.org otherwise.
//
// It must not be "web". That route uploads the attachment into Zotero's own
// file storage and so ignores the file storage the operator configured inside
// Zotero. On a WebDAV setup it consumes a storage plan the operator never chose
// to use, and when that plan fills, every filing stops with a bare HTTP 413.
// Handing the file to the desktop lets Zotero honour its own configuration.
const newItemRoute = "auto"

// planIdempotencyKey identifies a cached zotio plan. Every input that changes
// the mutation papio would perform belongs in it, because a cached plan is
// replayed verbatim: the route is here for the same reason the attachment mode
// is, and omitting it meant a route change left already-planned papers still
// pointed at the old destination.
func planIdempotencyKey(jobID, artifactSHA256, attachmentMode, collection, route string) string {
	return "zotio_plan:" + jobID + ":" + artifactSHA256 + ":" + attachmentMode + ":" + collection + ":" + route
}

// LookupWorkFrom copies the stable identifiers papio uses for new-item Zotio
// routing and library ownership checks. Title is deliberately excluded: it
// describes works rather than asserting identity (ADR-0019).
//
// It is exported because discovery asks the same ownership question about its
// own results, and it must ask it with the same identifiers. A second hand-built
// LookupWork in internal/discovery carried only DOI and ArXiv, so a PubMed-only
// paper already in the library reported as unowned and invited a duplicate
// acquisition. One converter, one answer.
func LookupWorkFrom(w work.Work) LookupWork {
	lw := LookupWork{DOI: w.DOI, ArXiv: w.ArXiv, PMID: w.PMID}
	if isbn := normalizedISBN(w.ISBN); isbn != "" {
		lw.ISBN = isbn
	}
	return lw
}

func hasNewItemRoutingIdentifier(w work.Work) bool {
	lw := LookupWorkFrom(w)
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
		return "", errors.New(newItemRoutingRefusal)
	}
}
