// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"net/url"
	"strconv"
	"strings"

	"papio/internal/config"
	"papio/internal/work"
)

// OpenURL builds an OpenURL 1.0 (ANSI/NISO Z39.88-2004) key/encoded-value link
// to the institution's resolver for one work. The resolver, not papio, decides
// which provider is entitled; papio only hands the identified work to it.
//
// The strong identifier (DOI, else PMID) is carried as rft_id; title, first
// author, and year travel as descriptive hints.
//
// A work carrying only an ISBN is described as a book: rft.isbn plus
// rft.btitle under the book metadata format. Sending a monograph's title in
// rft.atitle asks the resolver to find an *article* by that name, which is how
// printed books used to reach the catalogue as an unmatchable article query.
// All values are URL-escaped.
func OpenURL(base string, w work.Work) string {
	v := url.Values{}
	v.Set("url_ver", "Z39.88-2004")
	book := w.DOI == "" && w.PMID == "" && w.ISBN != ""
	switch {
	case w.DOI != "":
		v.Set("rft_id", "info:doi/"+w.DOI)
	case w.PMID != "":
		v.Set("rft_id", "info:pmid/"+w.PMID)
	}
	if book {
		v.Set("rft_val_fmt", "info:ofi/fmt:kev:mtx:book")
		v.Set("rft.genre", "book")
		v.Set("rft.isbn", w.ISBN)
	}
	if w.Title != "" {
		if book {
			v.Set("rft.btitle", w.Title)
		} else {
			v.Set("rft.atitle", w.Title)
		}
	}
	if len(w.Authors) > 0 && w.Authors[0] != "" {
		v.Set("rft.au", w.Authors[0])
	}
	if w.Year > 0 {
		v.Set("rft.date", strconv.Itoa(w.Year))
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + v.Encode()
}

// libKeyHost is the documented keyless LibKey.io linking origin. Third Iron
// documents the institution-affiliated DOI/PMID route as programmatically
// constructible: https://libkey.io/libraries/{library_id}/{doi-or-pmid}
// (ADR-0016 Decision 2). The api mode against public-api.thirdiron.com is a
// separate, unimplemented capability.
const libKeyHost = "libkey.io"

// LibKeyURL builds the keyless LibKey.io institution link for one work, or ""
// when the profile has no usable link mode or the work carries neither a DOI
// nor a PMID (LibKey resolves only those two identifiers; everything else
// stays on the plain OpenURL route).
func LibKeyURL(inst config.Institution, w work.Work) string {
	if inst.LibKeyMode != "link" || inst.LibKeyLibraryID <= 0 {
		return ""
	}
	id := ""
	switch {
	case w.DOI != "":
		id = w.DOI
	case w.PMID != "":
		id = w.PMID
	default:
		return ""
	}
	u := url.URL{
		Scheme: "https",
		Host:   libKeyHost,
		Path:   "/libraries/" + strconv.FormatInt(inst.LibKeyLibraryID, 10) + "/" + id,
	}
	return u.String()
}

// RouteURL picks the institutional browser route for one work: the profile's
// LibKey link when configured and applicable, else the OpenURL resolver link.
// LibKey augments institutional routing and never replaces it (ADR-0016
// Decision 6): a profile without a usable LibKey route falls through to the
// resolver, and eligibility gates elsewhere still key on the OpenURL base.
func RouteURL(inst config.Institution, w work.Work) string {
	if lk := LibKeyURL(inst, w); lk != "" {
		return lk
	}
	return OpenURL(inst.OpenURLBase, w)
}

// verifiedProviderHosts are the registrable domains of providers with
// declarative adapters (or adapters in progress). They ride on every offer so
// the extension can recognize a post-SSO landing on an entitled provider: the
// resolver host alone goes blind the moment it routes the tab onward. Matching
// is exact-or-dot-suffix on the extension side.
//
// The browser protocol caps provider_hosts at 20 entries and extensions
// before 0.4.1 fail-closed on longer lists, so this list must stay within the
// cap and cannot simply grow with the adapter registry. Extensions from 0.4.1
// also recognize any host in their own adapter registry (the registry is the
// authoritative adapter-host source); this list only needs the families whose
// entitled landings predate that behavior.
var verifiedProviderHosts = []string{
	"jstor.org",
	"proquest.com",
	"ebscohost.com",
	"ebsco.com",
	"springer.com",
	"sciencedirect.com",
	"elsevier.com",
	"acm.org",
	"wiley.com",
	"tandfonline.com",
	"sagepub.com",
	"apa.org",
	"oup.com",
	"cell.com",
}

// resolverHost returns the hostname of the OpenURL base; it joins the verified
// provider hosts on an offer (the resolver host is the tab papio opens; the
// entitled provider host is where the resolver lands it).
func resolverHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
