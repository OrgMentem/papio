// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"net/url"
	"strconv"
	"strings"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

// OpenURL builds an OpenURL 1.0 link to the institution's resolver for one work.
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

const libKeyHost = "libkey.io"

// LibKeyURL builds the keyless LibKey.io institution link for one work.
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
	u := url.URL{Scheme: "https", Host: libKeyHost, Path: "/libraries/" + strconv.FormatInt(inst.LibKeyLibraryID, 10) + "/" + id}
	return u.String()
}

// RouteURL picks the configured LibKey route when applicable, otherwise the
// institution's OpenURL resolver route.
func RouteURL(inst config.Institution, w work.Work) string {
	if lk := LibKeyURL(inst, w); lk != "" {
		return lk
	}
	return OpenURL(inst.OpenURLBase, w)
}

func validActionURL(value string) bool {
	if len(value) == 0 || len(value) > 4000 {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

// OpenAthensRedirectorHost is the OpenAthens link redirector. Its path names
// the institution's tenant domain and its url query names the destination.
const OpenAthensRedirectorHost = "go.openathens.net"

// OpenAthensRedirectorURL wraps a DOI landing URL in the institution's
// OpenAthens redirector: https://go.openathens.net/redirector/<domain>?url=<target>.
// Only a valid tenant domain builds a URL; the domain is spliced into the path,
// so it is checked here as well as at config load.
func OpenAthensRedirectorURL(domain, target string) (string, bool) {
	if config.ValidateOpenAthensDomain(domain) != nil {
		return "", false
	}
	return "https://" + OpenAthensRedirectorHost + "/redirector/" + domain + "?url=" + url.QueryEscape(target), true
}

// PublisherHandoffURL is derived only from a job's validated DOI and the
// institution's configuration. A failed resolver's page or diagnostic cannot
// supply a replacement navigation URL. With an OpenAthens domain configured
// the DOI is wrapped in that institution's redirector, a superset of the
// plain DOI link (the redirector answers with the publisher's own sign-in for
// the institution and then the article, or the plain DOI where it has no
// setup). One derivation serves every use of the action — offer, open and
// safety domain — so they cannot disagree.
func PublisherHandoffURL(action job.HumanAction, row job.Row, inst config.Institution) (string, bool) {
	if !job.IsPublisherHandoff(action) {
		return "", false
	}
	doi, err := work.NormalizeDOI(row.Work.DOI)
	if err != nil {
		return "", false
	}
	u := url.URL{Scheme: "https", Host: "doi.org", Path: "/" + doi}
	if inst.OpenAthensDomain != "" {
		if redirected, ok := OpenAthensRedirectorURL(inst.OpenAthensDomain, u.String()); ok {
			return redirected, true
		}
	}
	return u.String(), true
}

// ResolveHumanActionURL resolves the same fresh URL used by `papio actions
// open`. It performs no caching; each call evaluates the action detail and
// constructs a route from the current job and institution profile.
func ResolveHumanActionURL(action job.HumanAction, row job.Row, instFor func(string) (config.Institution, bool)) (string, bool) {
	if job.IsPublisherHandoff(action) {
		// The profile may lack an OpenURL base yet still name an OpenAthens
		// domain, so the lookup's availability flag is not consulted.
		inst, _ := instFor(row.Policy.Resolver)
		return PublisherHandoffURL(action, row, inst)
	}
	if direct, ok := OABrowserHandoffURL(action.Detail); ok {
		return direct, true
	}
	if detail := strings.TrimSpace(action.Detail); validActionURL(detail) {
		return detail, true
	}
	if HumanActionNextStepFor(action).Command == "" {
		return "", false
	}
	inst, ok := instFor(row.Policy.Resolver)
	if !ok || inst.OpenURLBase == "" {
		return "", false
	}
	target := RouteURL(inst, row.Work)
	return target, validActionURL(target)
}
