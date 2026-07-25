// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"papio/internal/redact"
)

// maxFailureMessage bounds a retained failure message. Backend errors are
// short; anything longer is a sign of an unexpected payload and is not worth
// carrying into diagnostics.
const maxFailureMessage = 300

// BackendFailure records one discovery backend's most recent failure. It exists
// because a partial failure used to be invisible: with two backends configured
// and one broken, a search returned the survivor's results and silently dropped
// the other's error, so a user seeing thin results could not tell a dead backend
// from a work that simply is not indexed.
type BackendFailure struct {
	Source  string    `json:"source"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// SanitizeError reduces a backend error to a bounded, single-line message safe
// for logs, diagnostics, and anything shown to a user.
//
// This is not decoration. A transport failure surfaces as a *url.Error whose
// text embeds the full request URL, and a discovery request URL carries the
// configured contact email and, for key-bearing backends, an API key.
// Stack-plan invariant 11 keeps signed query values and API keys out of durable
// storage and logs, so the URL is rebuilt through redact.URL before it can be
// retained or displayed — and the same guarantee now holds for freeform error
// text via scrubCredentials, including a *url.Error's own inner error, whose
// text is not itself a URL redact.URL can parse and can otherwise echo a
// nested request's URL verbatim. Recognized shapes: scheme://user:pass@host
// userinfo, an Authorization: header value (Bearer or otherwise), and
// query-style key=/api_key=/apikey=/token=/access_token=/secret=/mailto=
// assignments — wherever in the text they occur, not only inside a query
// string.
//
// redact.URL clears a parsed URL's user, query, and fragment but keeps its
// path, so a credential embedded in a URL path segment is not scrubbed here
// either. No backend papio ships does that today: OpenAlex's contact email
// and Semantic Scholar's API key travel in the query string and an HTTP
// header respectively, never a path segment. That is a deliberate, narrow
// gap rather than an oversight — revisit it if a future backend puts a
// secret in its request path.
//
// SanitizeError is not a general secret scanner: it targets the shapes
// papio's own backends are known to produce, not arbitrary credential
// formats an unrelated dependency might emit.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		inner := "request failed"
		if urlErr.Err != nil {
			inner = scrubCredentials(urlErr.Err.Error())
		}
		return bound(fmt.Sprintf("%s %s: %s", urlErr.Op, redact.URL(urlErr.URL), inner))
	}
	return bound(scrubCredentials(err.Error()))
}

// bound trims a message to one line within maxFailureMessage.
func bound(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " "))
	if len(message) > maxFailureMessage {
		return message[:maxFailureMessage] + "…"
	}
	return message
}

// Credential shapes scrubCredentials recognizes in freeform error text.
// urlLikePattern hands any embedded absolute URL to redact.URL — the one
// place that decides what a URL is allowed to retain — rather than
// re-implementing query/userinfo stripping here. The rest catch credentials
// quoted outside a URL entirely.
var (
	urlLikePattern         = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)
	userinfoPattern        = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)
	authorizationPattern   = regexp.MustCompile(`(?i)Authorization:[ \t]*\S.*`)
	bearerPattern          = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	credentialParamPattern = regexp.MustCompile(`(?i)\b(access_token|api_key|apikey|mailto|secret|token|key)=[^\s&]+`)
)

// scrubCredentials removes credential-shaped substrings from freeform error
// text before bound() ever sees it. It is deliberately layered rather than
// one pattern: an embedded URL's query/userinfo goes through redact.URL
// first, an Authorization header (of any scheme, not only Bearer) is
// redacted to the end of its line since an HTTP header value cannot itself
// contain a newline, a bare Bearer token not preceded by "Authorization:" is
// still caught, and finally any leftover key=/api_key=/token=/... assignment
// quoted outside a URL is redacted by value. Order matters: later passes run
// on the output of earlier ones, so a credential already replaced is never
// re-matched.
func scrubCredentials(text string) string {
	text = urlLikePattern.ReplaceAllStringFunc(text, redact.URL)
	text = userinfoPattern.ReplaceAllString(text, "$1")
	text = authorizationPattern.ReplaceAllString(text, "Authorization: <redacted>")
	text = bearerPattern.ReplaceAllString(text, "Bearer <redacted>")
	text = credentialParamPattern.ReplaceAllString(text, "$1=<redacted>")
	return text
}

// recordFailure remembers a backend's failure, replacing any earlier one.
//
// Ordering under concurrent callers is last-writer-wins by completion order,
// not by request initiation order or by which HTTP response is actually
// newer: Multi is shared between interactive searches (internal/api's
// discovery.search handler) and the periodic watch runner, so two searches
// can race the same backend, and whichever call's recordFailure runs last
// decides what LastFailures reports, even if it was issued first and its
// response is by now stale relative to the other. This is a diagnostic
// surface only — no search result depends on it — so that imprecision is
// accepted rather than fixed with a generation counter.
func (m *Multi) recordFailure(name string, err error) BackendFailure {
	failure := BackendFailure{Source: name, Message: SanitizeError(err), At: m.clock()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = make(map[string]BackendFailure, len(m.sources))
	}
	m.failures[name] = failure
	return failure
}

// clearFailure forgets a backend's failure after it answers successfully, so a
// transient outage does not haunt diagnostics forever.
//
// Subject to the same last-writer-wins ordering as recordFailure: a success
// that completes after a concurrent failure for the same backend clears it,
// even if the success was issued first and the failure is the more recent
// state. See recordFailure for why that is accepted rather than fixed here.
func (m *Multi) clearFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failures, name)
}

// LastFailures reports the most recent failure of every backend that is
// currently failing, ordered by source name so diagnostic output is stable. A
// backend that has since answered successfully is absent.
func (m *Multi) LastFailures() []BackendFailure {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.failures) == 0 {
		return nil
	}
	failures := make([]BackendFailure, 0, len(m.failures))
	for _, failure := range m.failures {
		failures = append(failures, failure)
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Source < failures[j].Source })
	return failures
}

// SummarizeFailures joins every backend failure into one bounded,
// single-line message naming each source and its sanitized cause, in the
// order given. It exists because SanitizeError(errors.Join(...)) cannot do
// this: errors.As walks a joined error's branches depth-first and returns on
// the first match, so it names whichever backend happens to come first and
// silently drops the rest even when every backend failed. Callers that
// already have the per-backend breakdown — as SearchPartial's own failures
// return value always does — should build the top-level message from this
// rather than from SanitizeError on the joined error. An empty slice yields
// an empty string.
func SummarizeFailures(failures []BackendFailure) string {
	if len(failures) == 0 {
		return ""
	}
	parts := make([]string, len(failures))
	for i, failure := range failures {
		parts[i] = fmt.Sprintf("%s: %s", failure.Source, failure.Message)
	}
	return bound(strings.Join(parts, "; "))
}

// clock reports the current time, using the injected test clock if one was
// configured. now is never reassigned after construction in production —
// only tests set it, and always before any concurrent access begins — but
// the read is guarded anyway: Multi's other state is entirely
// mutex-disciplined, and an unguarded field on an otherwise-guarded struct
// is a trap for the next change that isn't as careful about ordering.
func (m *Multi) clock() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}
