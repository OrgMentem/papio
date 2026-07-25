// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"errors"
	"fmt"
	"net/url"
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
// retained or displayed.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		inner := "request failed"
		if urlErr.Err != nil {
			inner = urlErr.Err.Error()
		}
		return bound(fmt.Sprintf("%s %s: %s", urlErr.Op, redact.URL(urlErr.URL), inner))
	}
	return bound(err.Error())
}

// bound trims a message to one line within maxFailureMessage.
func bound(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " "))
	if len(message) > maxFailureMessage {
		return message[:maxFailureMessage] + "…"
	}
	return message
}

// recordFailure remembers a backend's failure, replacing any earlier one.
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

func (m *Multi) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}
