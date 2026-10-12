// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package adaptercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultURL is the raw file committed in the papio repository. A 404 there
	// means nothing has been published, which is the normal state.
	DefaultURL = "https://raw.githubusercontent.com/OrgMentem/papio/main/control/adapter-control.json"
	// CheckEvery is the cadence of the fetch. It reuses the release check's
	// cadence: one request a day, recorded whether or not it succeeds.
	CheckEvery = 24 * time.Hour
	// fetchTimeout matches the release checker's HTTP client.
	fetchTimeout = 10 * time.Second
)

// Outcomes of one fetch attempt, as recorded in the status file.
const (
	OutcomePublished          = "published"
	OutcomeUnchanged          = "unchanged"
	OutcomeNonePublished      = "none_published"
	OutcomeFetchFailed        = "fetch_failed"
	OutcomeVerificationFailed = "verification_failed"
	OutcomeRejectedOlder      = "rejected_older_sequence"
	OutcomeRejectedConflict   = "rejected_sequence_conflict"
	OutcomePersistFailed      = "persist_failed"
)

// Status is the last fetch attempt.
type Status struct {
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	LastOutcome   string    `json:"last_outcome,omitempty"`
	LastDetail    string    `json:"last_detail,omitempty"`
}

// ValidateURL accepts https, plus plain http only to a loopback host so a test
// server can stand in for the repository. The document is signed, so transport
// is not what makes it trustworthy; the restriction keeps the contact private.
func ValidateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("adapter control URL %q is not a plain URL", raw)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(parsed.Hostname()); parsed.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("adapter control URL %q must use https (http is allowed only for a loopback host)", raw)
}

// Runner fetches the published document on the release check's cadence. It
// satisfies the daemon's maintenance-runner interface.
type Runner struct {
	Service *Service
	URL     string
	Client  *http.Client
	Now     func() time.Time
	// Every overrides CheckEvery; tests only.
	Every time.Duration
}

// RunDue performs one fetch when the last attempt is older than the cadence.
// Every outcome is recorded in the status file and none is an error: an
// unreachable or unverifiable endpoint is a status, never a daemon failure,
// and never relaxes state already held.
func (r *Runner) RunDue(ctx context.Context) error {
	if r == nil || r.Service == nil || !r.Service.HasKey() {
		return nil
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	every := CheckEvery
	if r.Every > 0 {
		every = r.Every
	}
	current := now().UTC()
	status := r.Service.Status()
	// A future timestamp (clock rollback, restored data directory) is not recent.
	if last := status.LastAttemptAt; !last.IsZero() && !last.After(current) && current.Sub(last) < every {
		return nil
	}
	outcome, detail, success := r.fetch(ctx)
	status.LastAttemptAt, status.LastOutcome, status.LastDetail = current, outcome, detail
	if success {
		status.LastSuccessAt = current
	}
	r.Service.recordStatus(status)
	return nil
}

func (r *Runner) fetch(ctx context.Context) (outcome, detail string, success bool) {
	if err := ValidateURL(r.URL); err != nil {
		return OutcomeFetchFailed, err.Error(), false
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return OutcomeFetchFailed, err.Error(), false
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return OutcomeFetchFailed, "request failed: " + err.Error(), false
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Nothing published. This is not a failure and never changes held state.
		return OutcomeNonePublished, "no control document is published at the configured URL", true
	default:
		return OutcomeFetchFailed, fmt.Sprintf("unexpected HTTP status %d", response.StatusCode), false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxEnvelopeBytes+1))
	if err != nil {
		return OutcomeFetchFailed, "reading the response body failed", false
	}
	applied, err := r.Service.Apply(body)
	switch {
	case err == nil && applied.Changed:
		return OutcomePublished, fmt.Sprintf("accepted sequence %d", applied.Verified.Document.Sequence), true
	case err == nil:
		return OutcomeUnchanged, fmt.Sprintf("sequence %d is already held", applied.Verified.Document.Sequence), true
	case errors.Is(err, ErrOlderSequence):
		return OutcomeRejectedOlder, err.Error(), false
	case errors.Is(err, ErrSequenceConflict):
		return OutcomeRejectedConflict, err.Error(), false
	case errors.Is(err, ErrEnvelope), errors.Is(err, ErrSignature), errors.Is(err, ErrDocument), errors.Is(err, ErrNoTrustAnchor):
		return OutcomeVerificationFailed, err.Error(), false
	default:
		return OutcomePersistFailed, err.Error(), false
	}
}

// SetFetchURL records where this daemon fetches control from, for doctor. The
// runner owns the fetching; a service without a URL is not fetching.
func (s *Service) SetFetchURL(endpoint string) {
	s.mu.Lock()
	s.fetchURL = endpoint
	s.mu.Unlock()
}

// FetchURL returns the configured fetch URL, empty when fetching is off.
func (s *Service) FetchURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fetchURL
}

// Status returns the last recorded fetch attempt.
func (s *Service) Status() Status {
	raw, err := os.ReadFile(filepath.Join(s.dir, StatusFileName))
	if err != nil {
		return Status{}
	}
	var status Status
	if json.Unmarshal(raw, &status) != nil {
		return Status{}
	}
	return status
}

func (s *Service) recordStatus(status Status) {
	raw, err := json.Marshal(status)
	if err != nil {
		return
	}
	// Best effort: a status that cannot be written only means the next
	// maintenance pass retries the fetch.
	_ = writeFileAtomic(filepath.Join(s.dir, StatusFileName), raw)
}
