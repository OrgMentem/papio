// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

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

// existingItemRoute names the route papio asks for when the Zotero item already
// exists and only its file is missing. "connector" creates a throwaway parent
// plus the file in one Zotero desktop session, moves the attachment onto the
// real item, and trashes the throwaway, so the bytes reach whatever file storage
// the operator configured in Zotero.
//
// It applies to "stored" mode only. A linked-file attachment uploads nothing, so
// there is no route to choose, and zotio ignores "--via" there silently rather
// than refusing — asking for it anyway would hide a mode mistake instead of
// surfacing it.
//
// The empty answer means "send no --via at all", which is why this returns a
// route rather than a bool: planIdempotencyKey records it, so a cached plan can
// never replay an argv the route has since changed.
func existingItemRoute(attachmentMode string) string {
	if attachmentMode == "stored" {
		return "connector"
	}
	return ""
}

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

// attachStagingBasename names the file papio hands to "attachments add". The
// import route's name is parsed by zotio's resolver and so must be identifier
// shaped; this one is not parsed at all, it becomes the attachment's TITLE in
// Zotero. Passing papio's artifact-store path directly made that title a
// 64-character SHA-256, visible in the operator's library beside siblings the
// import route titled readably. So the work's title is the name, and the
// identifier basename is the fallback for a work with no title.
//
// The cap is a filesystem limit, not taste: most filesystems bound a single
// component at 255 bytes and a paper title can exceed it.
func attachStagingBasename(w work.Work) (string, error) {
	if name := sanitizedTitleBasename(w.Title); name != "" {
		return name + ".pdf", nil
	}
	return importStagingBasename(w)
}

// attachTitleMaxBytes leaves room for the ".pdf" suffix inside a 255-byte
// component while staying well clear of any path length papio builds.
const attachTitleMaxBytes = 180

// sanitizedTitleBasename reduces a paper title to one safe path component, or
// returns "" when nothing usable survives. It removes the separators and the
// control bytes a title can carry - a title is third-party metadata, so it is
// attacker-influenced in exactly the way a captured filename is.
func sanitizedTitleBasename(title string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':':
			return '-'
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, title)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	cleaned = strings.Trim(cleaned, " .")
	if cleaned == "" {
		return ""
	}
	for len(cleaned) > attachTitleMaxBytes {
		cleaned = cleaned[:len(cleaned)-1]
	}
	for !utf8.ValidString(cleaned) {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return strings.Trim(cleaned, " .")
}
