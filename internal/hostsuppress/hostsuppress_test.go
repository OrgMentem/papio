// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package hostsuppress

import (
	"sync"
	"testing"
	"time"

	"papio/internal/config"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func testConfig() config.HostSuppression {
	cfg := config.DefaultHostSuppression()
	cfg.FailureThreshold = 3
	cfg.WindowDays = 90
	cfg.ProbeIntervalDays = 7
	cfg.RateLimitBackoffCapSecs = 3600
	// The span requirement has its own test; the others isolate one rule.
	cfg.MinWallSpanHours = 0
	return cfg
}

func wall(host string, at time.Time) Row {
	return Row{Outcome: "invalid", Detail: "*fetch.Error(http-403) host=" + host + " final=" + host, At: at}
}

func challenge(host string, at time.Time) Row {
	return Row{Outcome: "invalid", Detail: "*fetch.Error(challenge) host=" + host + " final=" + host, At: at}
}

func success(host string, at time.Time) Row {
	return Row{Outcome: "success", Detail: "bytes=10 mime=application/pdf host=" + host, At: at}
}

func TestThresholdBoundary(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 2; i++ {
		tr.Observe(wall("walled.example", t0.Add(time.Duration(i)*time.Hour)))
	}
	now := t0.Add(3 * time.Hour)
	if got := tr.Decide("walled.example", now); got != Fetch {
		t.Fatalf("N-1 walls: decision = %v, want Fetch", got)
	}
	tr.Observe(wall("walled.example", t0.Add(2*time.Hour)))
	if got := tr.Decide("walled.example", now); got != Skip {
		t.Fatalf("N walls: decision = %v, want Skip", got)
	}
}

func TestChallengeAndForbiddenPoolIntoOneWall(t *testing.T) {
	tr := New(testConfig())
	tr.Observe(challenge("walled.example", t0))
	tr.Observe(wall("walled.example", t0.Add(time.Minute)))
	tr.Observe(challenge("walled.example", t0.Add(2*time.Minute)))
	if got := tr.Decide("walled.example", t0.Add(time.Hour)); got != Skip {
		t.Fatalf("2 challenges + 1 403 = %v, want Skip (one wall)", got)
	}
}

func TestDaemonSuccessInsideWindowClears(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 5; i++ {
		tr.Observe(wall("mixed.example", t0.Add(time.Duration(i)*time.Minute)))
	}
	tr.Observe(success("mixed.example", t0.Add(time.Hour)))
	if got := tr.Decide("mixed.example", t0.Add(2*time.Hour)); got != Fetch {
		t.Fatalf("host with a daemon success = %v, want Fetch", got)
	}
	// Once the success ages out of the window, the walls inside it rule again.
	later := t0.Add(95 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		tr.Observe(wall("mixed.example", later.Add(time.Duration(i)*time.Minute)))
	}
	if got := tr.Decide("mixed.example", later.Add(time.Hour)); got != Skip {
		t.Fatalf("after the success left the window = %v, want Skip", got)
	}
}

func TestWallsOutsideWindowExpire(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 3; i++ {
		tr.Observe(wall("old.example", t0.Add(time.Duration(i)*time.Minute)))
	}
	if got := tr.Decide("old.example", t0.Add(91*24*time.Hour)); got != Fetch {
		t.Fatalf("walls older than the window = %v, want Fetch", got)
	}
}

func TestProbeCycle(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 3; i++ {
		tr.Observe(wall("walled.example", t0.Add(time.Duration(i)*time.Minute)))
	}
	last := t0.Add(2 * time.Minute)
	if got := tr.Decide("walled.example", last.Add(7*24*time.Hour-time.Second)); got != Skip {
		t.Fatalf("before the probe interval = %v, want Skip", got)
	}
	due := last.Add(7 * 24 * time.Hour)
	if got := tr.Decide("walled.example", due); got != Probe {
		t.Fatalf("at the probe interval = %v, want Probe", got)
	}
	if got, _ := tr.Claim("walled.example", due); got != Probe {
		t.Fatalf("claim at the interval = %v, want Probe", got)
	}
	if got := tr.Decide("walled.example", due.Add(time.Second)); got != Skip {
		t.Fatalf("while a probe is admitted = %v, want Skip for every other candidate", got)
	}
	// The probe fails: the wall re-arms, and the next probe is one interval
	// after that failure.
	tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(http-403) host=walled.example final=walled.example probe", At: due.Add(time.Second)})
	if got := tr.Decide("walled.example", due.Add(6*24*time.Hour)); got != Skip {
		t.Fatalf("after a failed probe = %v, want Skip", got)
	}
	if got := tr.Decide("walled.example", due.Add(time.Second).Add(7*24*time.Hour)); got != Probe {
		t.Fatalf("one interval after the failed probe = %v, want Probe", got)
	}
	// A probe that succeeds clears the host.
	next := due.Add(8 * 24 * time.Hour)
	if got, _ := tr.Claim("walled.example", next); got != Probe {
		t.Fatalf("claim of the second probe = %v, want Probe", got)
	}
	tr.Observe(Row{Outcome: "success", Detail: "bytes=1 mime=application/pdf host=walled.example probe", At: next})
	if got := tr.Decide("walled.example", next.Add(time.Minute)); got != Fetch {
		t.Fatalf("after a successful probe = %v, want Fetch", got)
	}
}

func TestRedirectorsNeverSuppress(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 5; i++ {
		// doi.org handed each request to the publisher, which answered 403.
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(http-403) host=doi.org final=publisher.example", At: t0.Add(time.Duration(i) * time.Minute)})
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(http-403) host=handle.example final=publisher.example", At: t0.Add(time.Duration(i) * time.Minute)})
	}
	now := t0.Add(time.Hour)
	if got := tr.Decide("publisher.example", now); got != Skip {
		t.Fatalf("the responder = %v, want Skip: the walls are its own", got)
	}
	if got := tr.Decide("doi.org", now); got != Fetch {
		t.Fatalf("doi.org = %v, want Fetch: excluded redirector", got)
	}
	if got := tr.Decide("handle.example", now); got != Fetch {
		t.Fatalf("a pure redirector = %v, want Fetch", got)
	}
	// A candidate-host-only wall row for a pure redirector (no responder was
	// recorded) still never suppresses it.
	for i := 0; i < 3; i++ {
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(challenge) host=handle.example", At: t0.Add(time.Duration(10+i) * time.Minute)})
	}
	if got := tr.Decide("handle.example", now); got != Fetch {
		t.Fatalf("pure redirector with responder-less walls = %v, want Fetch", got)
	}
}

func TestExcludedWalledHostNeverSuppresses(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		cfg := testConfig()
		cfg.MinWallSpanHours = 2
		want := Skip
		if excluded {
			cfg.ExcludedHosts = []string{" Walled.Example "}
			want = Fetch
		}
		tr := New(cfg)
		for i := range 3 {
			tr.Observe(wall("walled.example", t0.Add(time.Duration(i)*time.Hour)))
		}
		if got := tr.Decide("walled.example", t0.Add(3*time.Hour)); got != want {
			t.Fatalf("excluded=%v: decision = %v, want %v", excluded, got, want)
		}
	}
}

func TestMixedRedirectorStillSuppresses(t *testing.T) {
	tr := New(testConfig())
	tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(http-403) host=pub.example final=www.pub.example", At: t0})
	for i := range 3 {
		tr.Observe(wall("pub.example", t0.Add(time.Duration(i)*time.Hour)))
	}
	if got := tr.Decide("pub.example", t0.Add(3*time.Hour)); got != Skip {
		t.Fatalf("redirector with its own walls = %v, want Skip", got)
	}
}

func TestResponderlessWallsStillSuppress(t *testing.T) {
	tr := New(testConfig())
	for i := range 3 {
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(challenge) host=pub.example", At: t0.Add(time.Duration(i) * time.Hour)})
	}
	if got := tr.Decide("pub.example", t0.Add(3*time.Hour)); got != Skip {
		t.Fatalf("host with responder-less walls = %v, want Skip", got)
	}
}

func TestRateLimitsBackOffAndNeverSuppress(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 10; i++ {
		tr.Observe(Row{Outcome: "retryable", Detail: "*fetch.Error(http-429) host=busy.example final=busy.example", At: t0})
	}
	if got := tr.Decide("busy.example", t0.Add(time.Minute)); got != Fetch {
		t.Fatalf("ten 429s = %v, want Fetch: rate limits never suppress", got)
	}
	tr.Observe429("busy.example", 0, t0)
	until, ok := tr.BackedOff("busy.example", t0)
	if !ok || !until.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first backoff = %v/%v, want %v", until, ok, t0.Add(time.Minute))
	}
	tr.Observe429("busy.example", 0, t0)
	if until, _ = tr.BackedOff("busy.example", t0); !until.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("second backoff = %v, want doubled", until)
	}
	tr.Observe429("busy.example", 10*time.Minute, t0)
	if until, _ = tr.BackedOff("busy.example", t0); !until.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("Retry-After backoff = %v, want the named 10m", until)
	}
	tr.Observe429("busy.example", 48*time.Hour, t0)
	if until, _ = tr.BackedOff("busy.example", t0); !until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("capped backoff = %v, want the 1h cap", until)
	}
	if _, ok := tr.BackedOff("busy.example", t0.Add(time.Hour)); ok {
		t.Fatal("backoff still active at its own end")
	}
	// A daemon success resets the doubling.
	tr.Observe(success("busy.example", t0.Add(2*time.Hour)))
	tr.Observe429("busy.example", 0, t0.Add(3*time.Hour))
	if until, _ = tr.BackedOff("busy.example", t0.Add(3*time.Hour)); !until.Equal(t0.Add(3*time.Hour + time.Minute)) {
		t.Fatalf("backoff after a success = %v, want the base again", until)
	}
}

func TestLegacyAndNonAttemptRowsAreIgnored(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 10; i++ {
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error", At: t0})
		tr.Observe(Row{Outcome: "suppressed", Detail: "*fetch.Error(wall) host=walled.example suppressed=walled.example", At: t0})
		tr.Observe(Row{Outcome: "invalid", Detail: "*fetch.Error(not-a-pdf) host=walled.example final=walled.example", At: t0})
		tr.Observe(Row{Outcome: "retryable", Detail: "*fetch.Error(network) host=walled.example", At: t0})
	}
	if got := tr.Decide("walled.example", t0.Add(time.Hour)); got != Fetch {
		t.Fatalf("legacy, suppressed, not-a-pdf and network rows = %v, want Fetch", got)
	}
}

func TestDisabledRuleNeverDecidesAgainstAHost(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	tr := New(cfg)
	for i := 0; i < 5; i++ {
		tr.Observe(wall("walled.example", t0))
	}
	tr.Observe429("walled.example", time.Hour, t0)
	if got := tr.Decide("walled.example", t0.Add(time.Minute)); got != Fetch {
		t.Fatalf("disabled rule = %v, want Fetch", got)
	}
	if _, ok := tr.BackedOff("walled.example", t0.Add(time.Minute)); ok {
		t.Fatal("disabled rule backed a host off")
	}
}

// A burst of refusals inside one short span is an intermittent host (the
// operator store had one serve five seconds after such a burst); only a
// wall that persists across the span suppresses.
func TestWallsMustSpanTheMinimumBeforeSuppressing(t *testing.T) {
	cfg := testConfig()
	cfg.MinWallSpanHours = 24
	tr := New(cfg)
	for i := 0; i < 10; i++ {
		tr.Observe(wall("bursty.example", t0.Add(time.Duration(i)*time.Hour)))
	}
	if got := tr.Decide("bursty.example", t0.Add(10*time.Hour)); got != Fetch {
		t.Fatalf("ten walls inside 9h = %v, want Fetch", got)
	}
	tr.Observe(wall("bursty.example", t0.Add(24*time.Hour-time.Second)))
	if got := tr.Decide("bursty.example", t0.Add(24*time.Hour)); got != Fetch {
		t.Fatalf("walls spanning 24h-1s = %v, want Fetch", got)
	}
	tr.Observe(wall("bursty.example", t0.Add(24*time.Hour)))
	if got := tr.Decide("bursty.example", t0.Add(24*time.Hour+time.Second)); got != Skip {
		t.Fatalf("walls spanning 24h = %v, want Skip", got)
	}
}

// Claiming a probe is one atomic step: of many concurrent callers on a due
// host, exactly one gets the probe and every other one is told to skip.
func TestClaimGrantsExactlyOneConcurrentProbe(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 3; i++ {
		tr.Observe(wall("walled.example", t0.Add(time.Duration(i)*time.Minute)))
	}
	due := t0.Add(8 * 24 * time.Hour)
	const callers = 64
	results := make(chan Decision, callers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			decision, _ := tr.Claim("walled.example", due)
			results <- decision
		}()
	}
	start.Done()
	done.Wait()
	close(results)
	probes, skips := 0, 0
	for decision := range results {
		switch decision {
		case Probe:
			probes++
		case Skip:
			skips++
		}
	}
	if probes != 1 || skips != callers-1 {
		t.Fatalf("probes = %d, skips = %d; want exactly one probe", probes, skips)
	}
}

// A released claim hands the probe to the next caller; a stale release (after
// a newer claim) changes nothing.
func TestReleaseReturnsTheProbe(t *testing.T) {
	tr := New(testConfig())
	for i := 0; i < 3; i++ {
		tr.Observe(wall("walled.example", t0.Add(time.Duration(i)*time.Minute)))
	}
	due := t0.Add(8 * 24 * time.Hour)
	decision, claim := tr.Claim("walled.example", due)
	if decision != Probe {
		t.Fatalf("first claim = %v, want Probe", decision)
	}
	if again, _ := tr.Claim("walled.example", due.Add(time.Second)); again != Skip {
		t.Fatalf("second claim = %v, want Skip while the probe is held", again)
	}
	tr.Release(claim)
	decision, newer := tr.Claim("walled.example", due.Add(2*time.Second))
	if decision != Probe {
		t.Fatalf("claim after release = %v, want Probe", decision)
	}
	tr.Release(claim) // stale: must not free the newer claim
	if again, _ := tr.Claim("walled.example", due.Add(3*time.Second)); again != Skip {
		t.Fatalf("claim after a stale release = %v, want Skip", again)
	}
	_ = newer
}
