// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"net/url"
	"strconv"
	"strings"

	"papio/internal/work"
)

// OpenURL builds an OpenURL 1.0 (ANSI/NISO Z39.88-2004) key/encoded-value link
// to the institution's resolver for one work. The resolver, not papio, decides
// which provider is entitled; papio only hands the identified work to it.
//
// The strong identifier (DOI, else PMID) is carried as rft_id; title, first
// author, and year travel as descriptive hints. All values are URL-escaped.
func OpenURL(base string, w work.Work) string {
	v := url.Values{}
	v.Set("url_ver", "Z39.88-2004")
	switch {
	case w.DOI != "":
		v.Set("rft_id", "info:doi/"+w.DOI)
	case w.PMID != "":
		v.Set("rft_id", "info:pmid/"+w.PMID)
	}
	if w.Title != "" {
		v.Set("rft.atitle", w.Title)
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

// verifiedProviderHosts are the registrable domains of the providers verified
// live behind the Example University resolver (JSTOR, ProQuest, EBSCO, Springer Nature Link).
// They ride on every offer so the extension can recognize a post-SSO landing
// on an entitled provider: the resolver host alone goes blind the moment it
// routes the tab onward. Matching is exact-or-dot-suffix on the extension side.
var verifiedProviderHosts = []string{"jstor.org", "proquest.com", "ebscohost.com", "ebsco.com", "springer.com"}

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
