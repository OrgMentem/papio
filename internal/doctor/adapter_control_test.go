// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"strings"
	"testing"
	"time"

	"papio/internal/adaptercontrol"
	"papio/internal/browser"
	"papio/internal/protocol"
)

func controlInput(state adaptercontrol.State, sequence int64, revisions, domains []string) AdapterControlInput {
	return AdapterControlInput{
		HasKey:   true,
		FetchURL: "https://control.example/adapter-control.json",
		Snapshot: adaptercontrol.Snapshot{
			State: state, Sequence: sequence, Revisions: revisions, Domains: domains,
			Digest: protocol.SuspensionsDigest(revisions, domains), Reason: "reason text",
		},
		Status: adaptercontrol.Status{
			LastAttemptAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC),
			LastOutcome:   adaptercontrol.OutcomeNonePublished, LastDetail: "no control document is published at the configured URL",
		},
	}
}

func controlCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("no %q check in %+v", name, checks)
	return Check{}
}

func TestAdapterControlCheckNamesTheSuspensionAndTheFallback(t *testing.T) {
	in := controlInput(adaptercontrol.StateCurrent, 3, []string{"sciencedirect@0.3.0"}, []string{"publisher:www.sciencedirect.com"})
	in.Status = adaptercontrol.Status{LastAttemptAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC), LastOutcome: adaptercontrol.OutcomePublished}
	check := controlCheck(t, AdapterControlChecks(in), "adapter control")
	if check.Status != Warn {
		t.Fatalf("status = %s, want warn for an active suspension", check.Status)
	}
	for _, want := range []string{"sequence 3", "sciencedirect@0.3.0", "publisher:www.sciencedirect.com", "assisted"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail %q does not mention %q", check.Detail, want)
		}
	}
}

func TestAdapterControlCheckStates(t *testing.T) {
	// No key: inactive, no claim about control either way.
	if check := controlCheck(t, AdapterControlChecks(AdapterControlInput{}), "adapter control"); check.Status != Skip || !strings.Contains(check.Detail, "no control public key") {
		t.Fatalf("keyless = %+v", check)
	}
	// 404 (nothing published) is the normal state and passes.
	none := controlCheck(t, AdapterControlChecks(controlInput(adaptercontrol.StateNone, 0, nil, nil)), "adapter control")
	if none.Status != Pass || !strings.Contains(none.Detail, adaptercontrol.OutcomeNonePublished) {
		t.Fatalf("none = %+v", none)
	}
	// A lifted control still passes and says it holds no suspensions.
	if lifted := controlCheck(t, AdapterControlChecks(controlInput(adaptercontrol.StateCurrent, 4, nil, nil)), "adapter control"); lifted.Status != Pass || !strings.Contains(lifted.Detail, "no suspensions") {
		t.Fatalf("lifted = %+v", lifted)
	}
	// A refused document is a warning that says the held control is unchanged.
	refused := controlInput(adaptercontrol.StateNone, 0, nil, nil)
	refused.Status.LastOutcome, refused.Status.LastDetail = adaptercontrol.OutcomeVerificationFailed, "adapter control signature does not verify"
	if check := controlCheck(t, AdapterControlChecks(refused), "adapter control"); check.Status != Warn || !strings.Contains(check.Detail, "did not verify") && !strings.Contains(check.Detail, "does not verify") {
		t.Fatalf("verification failure = %+v", check)
	}
	rolledBack := controlInput(adaptercontrol.StateCurrent, 5, nil, nil)
	rolledBack.Status.LastOutcome = adaptercontrol.OutcomeRejectedOlder
	if check := controlCheck(t, AdapterControlChecks(rolledBack), "adapter control"); check.Status != Warn || !strings.Contains(check.Detail, "held control was not changed") {
		t.Fatalf("rollback = %+v", check)
	}
	// An unverifiable store is a failure with remediation: every browser stops.
	unavailable := controlCheck(t, AdapterControlChecks(controlInput(adaptercontrol.StateUnavailable, 0, nil, nil)), "adapter control")
	if unavailable.Status != Fail || unavailable.Remediation == "" || !strings.Contains(unavailable.Detail, "reason text") {
		t.Fatalf("unavailable = %+v", unavailable)
	}
	// Fetching off still reports what is held.
	off := controlInput(adaptercontrol.StateCurrent, 3, []string{"wiley@0.1.0"}, nil)
	off.FetchURL = ""
	if check := controlCheck(t, AdapterControlChecks(off), "adapter control"); !strings.Contains(check.Detail, "Fetching is off") || !strings.Contains(check.Detail, "wiley@0.1.0") {
		t.Fatalf("fetch off = %+v", check)
	}
}

func TestAdapterControlBrowserCheckReportsEachUnappliedSession(t *testing.T) {
	in := controlInput(adaptercontrol.StateCurrent, 3, []string{"wiley@0.1.0"}, nil)
	in.Sessions = []browser.AdapterControlSession{
		{ID: "applied-session-id", ExtensionVersion: "0.16.0", Capable: true, Resolved: true, Outcome: "applied", Sequence: 3},
	}
	if check := controlCheck(t, AdapterControlChecks(in), "adapter control (browser)"); check.Status != Pass {
		t.Fatalf("applied = %+v", check)
	}
	in.Sessions = append(in.Sessions,
		browser.AdapterControlSession{ID: "old-session-id", ExtensionVersion: "0.15.0"},
		browser.AdapterControlSession{ID: "rollback-session", ExtensionVersion: "0.16.0", Capable: true, Resolved: true, Outcome: "rolled_back", Sequence: 9},
	)
	check := controlCheck(t, AdapterControlChecks(in), "adapter control (browser)")
	if check.Status != Warn || !strings.Contains(check.Detail, "too old") || !strings.Contains(check.Detail, "rolled_back") || !strings.Contains(check.Remediation, "0.16.0") {
		t.Fatalf("check = %+v", check)
	}
	// Nothing published: no per-session check at all, so a fresh install is quiet.
	quiet := controlInput(adaptercontrol.StateNone, 0, nil, nil)
	quiet.Sessions = []browser.AdapterControlSession{
		{ID: "fresh-session", ExtensionVersion: "0.16.0", Capable: true, Resolved: true, Outcome: "applied"},
		{ID: "waiting-session", ExtensionVersion: "0.16.0", Capable: true},
		{ID: "old-session", ExtensionVersion: "0.15.0"},
	}
	for _, c := range AdapterControlChecks(quiet) {
		if c.Name == "adapter control (browser)" {
			t.Fatalf("a daemon with no control reported a browser check: %+v", c)
		}
	}
}

// A fetch that never reached a conclusion (outage, timeout, 5xx) is not the
// normal "nothing published" 404: the daemon cannot say whether control exists.
func TestAdapterControlCheckWarnsWhenTheFetchFailed(t *testing.T) {
	for _, in := range []AdapterControlInput{
		controlInput(adaptercontrol.StateNone, 0, nil, nil),
		controlInput(adaptercontrol.StateCurrent, 4, nil, nil),
		controlInput(adaptercontrol.StateCurrent, 4, []string{"wiley@0.1.0"}, nil),
	} {
		in.Status.LastOutcome, in.Status.LastDetail = adaptercontrol.OutcomeFetchFailed, "unexpected HTTP status 503"
		check := controlCheck(t, AdapterControlChecks(in), "adapter control")
		if check.Status != Warn || !strings.Contains(check.Detail, adaptercontrol.OutcomeFetchFailed) {
			t.Errorf("state %s: check = %+v, want warn naming the failed fetch", in.Snapshot.State, check)
		}
	}
}

func TestAdapterControlBrowserCheckReportsRefusalWithoutDaemonControl(t *testing.T) {
	for _, outcome := range []string{"missing", "rolled_back", "verification_failed"} {
		t.Run(outcome, func(t *testing.T) {
			in := controlInput(adaptercontrol.StateNone, 0, nil, nil)
			in.Sessions = []browser.AdapterControlSession{
				{ID: "refusing-session", ExtensionVersion: "0.16.0", Capable: true, Resolved: true, Outcome: outcome},
			}
			check := controlCheck(t, AdapterControlChecks(in), "adapter control (browser)")
			if check.Status != Warn || !strings.Contains(check.Detail, outcome) || !strings.Contains(check.Detail, "withheld") {
				t.Fatalf("browser refusal = %+v", check)
			}
			if outcome == "missing" && !strings.Contains(check.Detail, "daemon supplies no control") {
				t.Fatalf("missing refusal has no cause: %+v", check)
			}
			if !strings.Contains(check.Remediation, "restore") || !strings.Contains(check.Remediation, "fetch") {
				t.Fatalf("browser refusal has no recovery advice: %+v", check)
			}
		})
	}
}

func TestAdapterControlUnavailableRemediationPreservesControl(t *testing.T) {
	in := controlInput(adaptercontrol.StateUnavailable, 0, nil, nil)
	check := controlCheck(t, AdapterControlChecks(in), "adapter control")
	if strings.Contains(check.Remediation, "remove") || !strings.Contains(check.Remediation, "next control fetch") {
		t.Fatalf("unsafe recovery advice: %+v", check)
	}
	in.FetchURL = ""
	check = controlCheck(t, AdapterControlChecks(in), "adapter control")
	if !strings.Contains(check.Remediation, "enable [updates] check") {
		t.Fatalf("disabled fetching has no recovery advice: %+v", check)
	}
}

func TestAdapterControlCheckReportsLastSuccessfulFetchAge(t *testing.T) {
	in := controlInput(adaptercontrol.StateCurrent, 4, nil, nil)
	in.Status.LastOutcome = adaptercontrol.OutcomeFetchFailed
	in.Status.LastSuccessAt = time.Now().Add(-48 * time.Hour)
	check := controlCheck(t, AdapterControlChecks(in), "adapter control")
	if !strings.Contains(check.Detail, "last successful fetch 48h0m0s ago") {
		t.Fatalf("missing successful fetch age: %+v", check)
	}
	in.Status.LastSuccessAt = time.Time{}
	check = controlCheck(t, AdapterControlChecks(in), "adapter control")
	if !strings.Contains(check.Detail, "no successful fetch is recorded") {
		t.Fatalf("missing absent success diagnosis: %+v", check)
	}
}
