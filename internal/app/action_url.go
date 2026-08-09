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

// ResolveHumanActionURL resolves the same fresh URL used by `papio actions
// open`. It performs no caching; each call evaluates the action detail and
// constructs a route from the current job and institution profile.
func ResolveHumanActionURL(action job.HumanAction, row job.Row, instFor func(string) (config.Institution, bool)) (string, bool) {
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
