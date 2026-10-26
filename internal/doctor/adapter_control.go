// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"fmt"
	"strings"
	"time"

	"papio/internal/adaptercontrol"
	"papio/internal/browser"
)

// AdapterControlInput is everything the signed adapter control checks read.
// The daemon builds it from its live state, so the checks never touch the
// network or the filesystem themselves.
type AdapterControlInput struct {
	Snapshot adaptercontrol.Snapshot
	Status   adaptercontrol.Status
	// HasKey is false for a build that embeds no control public key.
	HasKey bool
	// FetchURL is empty when fetching is off (updates.check = false).
	FetchURL string
	Sessions []browser.AdapterControlSession
}

// AdapterControlChecks reports the signed emergency suspension state: what the
// daemon holds, what the last fetch did, and whether each connected browser
// applied it. A suspended adapter falls back to assisted operation, and these
// checks are where the operator learns that.
func AdapterControlChecks(in AdapterControlInput) []Check {
	checks := []Check{daemonControlCheck(in)}
	if check, ok := browserControlCheck(in); ok {
		checks = append(checks, check)
	}
	return checks
}

func daemonControlCheck(in AdapterControlInput) Check {
	const name = "adapter control"
	if !in.HasKey {
		return Check{Name: name, Status: Skip, Detail: "signed adapter control is inactive: this build embeds no control public key, so it fetches and accepts no control document"}
	}
	snapshot := in.Snapshot
	fetch := fetchSummary(in)
	switch snapshot.State {
	case adaptercontrol.StateUnavailable:
		remediation := "keep the daemon running for the next control fetch, then run papio doctor again; restore a trusted copy of the data directory if fetching cannot repair it"
		if in.FetchURL == "" {
			remediation = "enable [updates] check and restart the daemon to fetch valid control, or restore a trusted copy of the data directory"
		}
		return Check{
			Name: name, Status: Fail,
			Detail:      "the stored adapter control cannot be trusted, so every browser withholds automatic downloads: " + snapshot.Reason + ". " + fetch,
			Remediation: remediation,
		}
	case adaptercontrol.StateCurrent:
		if len(snapshot.Revisions) == 0 && len(snapshot.Domains) == 0 {
			status := Pass
			if failedFetch(in.Status.LastOutcome) {
				status = Warn
			}
			return Check{Name: name, Status: status, Detail: fmt.Sprintf("control sequence %d holds no suspensions. %s", snapshot.Sequence, fetch)}
		}
		return Check{
			Name: name, Status: Warn,
			Detail: fmt.Sprintf("control sequence %d suspends %s; affected adapters fall back to assisted operation and their papers wait for you in the inbox. %s",
				snapshot.Sequence, suspensionList(snapshot), fetch),
			Remediation: "update the browser extension when a fixed release ships; papio lifts the suspension when a higher control sequence is published",
		}
	}
	status := Pass
	if failedFetch(in.Status.LastOutcome) {
		status = Warn
	}
	return Check{Name: name, Status: status, Detail: "no adapter control is published or held. " + fetch}
}

func suspensionList(snapshot adaptercontrol.Snapshot) string {
	var parts []string
	if len(snapshot.Revisions) > 0 {
		parts = append(parts, fmt.Sprintf("%d adapter revision(s) (%s)", len(snapshot.Revisions), strings.Join(snapshot.Revisions, ", ")))
	}
	if len(snapshot.Domains) > 0 {
		parts = append(parts, fmt.Sprintf("%d safety domain(s) (%s)", len(snapshot.Domains), strings.Join(snapshot.Domains, ", ")))
	}
	return strings.Join(parts, " and ")
}

func failedFetch(outcome string) bool {
	switch outcome {
	case adaptercontrol.OutcomeFetchFailed, adaptercontrol.OutcomeVerificationFailed, adaptercontrol.OutcomeRejectedOlder,
		adaptercontrol.OutcomeRejectedConflict, adaptercontrol.OutcomePersistFailed:
		return true
	}
	return false
}

func fetchSummary(in AdapterControlInput) string {
	if in.FetchURL == "" {
		return "Fetching is off ([updates] check = false); only control already held applies."
	}
	status := in.Status
	if status.LastAttemptAt.IsZero() {
		return "No fetch has run yet."
	}
	text := fmt.Sprintf("Last fetch %s: %s", status.LastAttemptAt.Format("2006-01-02 15:04 MST"), status.LastOutcome)
	if status.LastDetail != "" && status.LastOutcome != adaptercontrol.OutcomePublished {
		text += " (" + status.LastDetail + ")"
	}
	if failedFetch(status.LastOutcome) {
		text += "; the held control was not changed"
	}
	if status.LastSuccessAt.IsZero() {
		text += "; no successful fetch is recorded"
	} else {
		text += fmt.Sprintf("; last successful fetch %s ago", time.Since(status.LastSuccessAt).Round(time.Minute))
	}
	return text + "."
}

func browserControlCheck(in AdapterControlInput) (Check, bool) {
	if len(in.Sessions) == 0 {
		return Check{}, false
	}
	const name = "adapter control (browser)"
	var problems []string
	remediation := ""
	for _, session := range in.Sessions {
		// A fresh install needs no control, but a browser can remember control
		// that this daemon no longer holds. Its refusal still needs a diagnosis.
		if !in.Snapshot.Mandatory() && (!session.Resolved || session.Outcome == "applied") {
			continue
		}
		label := fmt.Sprintf("session %s (extension v%s)", shortID(session.ID), session.ExtensionVersion)
		switch {
		case !session.Capable:
			problems = append(problems, label+" is too old to apply signed control")
			remediation = "update the browser extension to " + browser.AdapterControlMinExtensionVersion + " or later"
		case !session.Resolved:
			problems = append(problems, label+" has not reported yet")
		case session.Outcome != "applied":
			problems = append(problems, fmt.Sprintf("%s refused control (%s)", label, session.Outcome))
			if session.Outcome == "missing" {
				problems[len(problems)-1] += ": the daemon supplies no control, and the browser cannot safely treat this as a fresh install"
			}
			if remediation == "" {
				remediation = "a browser that saw a higher sequence than this daemon holds refuses automation: restore the daemon's data directory or wait for the next control fetch"
				if in.FetchURL == "" {
					remediation = "restore the daemon's data directory or enable [updates] check and restart the daemon to fetch valid control"
				}
			}
		case in.Snapshot.State == adaptercontrol.StateCurrent && session.Sequence < in.Snapshot.Sequence:
			problems = append(problems, fmt.Sprintf("%s applied sequence %d, behind %d", label, session.Sequence, in.Snapshot.Sequence))
		}
	}
	if len(problems) == 0 {
		if !in.Snapshot.Mandatory() {
			return Check{}, false
		}
		return Check{Name: name, Status: Pass, Detail: fmt.Sprintf("every connected browser applied control sequence %d", in.Snapshot.Sequence)}, true
	}
	return Check{
		Name: name, Status: Warn,
		Detail:      "automatic downloads are withheld from: " + strings.Join(problems, "; "),
		Remediation: remediation,
	}, true
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
