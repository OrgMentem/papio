// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package adaptercontrol_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/adaptercontrol"
	"papio/internal/adaptercontrol/controltest"
	"papio/internal/protocol"
)

func signed(t *testing.T, sequence int64, revisions, domains []string) []byte {
	t.Helper()
	_, private := controltest.PlaceholderKey()
	envelope, err := adaptercontrol.Sign(adaptercontrol.Document{
		Sequence: sequence, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "papio-control-test",
		SuspendedRevisions: revisions, SuspendedSafetyDomains: domains,
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// signRaw signs arbitrary payload bytes, so a test can present a correctly
// signed document the Sign helper would refuse to build.
func signRaw(t *testing.T, payload []byte) []byte {
	t.Helper()
	_, private := controltest.PlaceholderKey()
	signature := ed25519.Sign(private, append([]byte("papio-adapter-control/1\x00"), payload...))
	envelope, err := json.Marshal(adaptercontrol.Envelope{
		Schema: adaptercontrol.EnvelopeSchema, KeyID: "papio-control-test",
		Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func openService(t *testing.T, dir string) *adaptercontrol.Service {
	t.Helper()
	public, _ := controltest.PlaceholderKey()
	return adaptercontrol.Open(dir, public)
}

func TestDigestVectorsMatchTheExtension(t *testing.T) {
	// The same vectors are pinned in extension/test/control.test.ts.
	if got := protocol.SuspensionsDigest(nil, nil); got != "efcac454b135229c85b492666dc43af4fdfe5e58afe2e7937b31e404ce1d2f27" {
		t.Fatalf("empty digest = %s", got)
	}
	got := protocol.SuspensionsDigest([]string{"sciencedirect@0.3.0"}, []string{"publisher:www.sciencedirect.com"})
	if got != "d9ada45fb9029f57dacec82802fed8b4472457d678544901e1f283057552b028" {
		t.Fatalf("digest = %s", got)
	}
}

func TestVerifyAcceptsOnlyTheEmbeddedKeysExactBytes(t *testing.T) {
	public, _ := controltest.PlaceholderKey()
	other, _ := controltest.OtherKey()
	envelope := signed(t, 3, []string{"sciencedirect@0.3.0"}, []string{"publisher:www.sciencedirect.com"})

	verified, err := adaptercontrol.Verify(envelope, public)
	if err != nil {
		t.Fatalf("a document signed by the trusted key was rejected: %v", err)
	}
	if verified.Document.Sequence != 3 || verified.Digest != protocol.SuspensionsDigest(verified.Document.SuspendedRevisions, verified.Document.SuspendedSafetyDomains) {
		t.Fatalf("verified = %+v", verified)
	}

	if _, err := adaptercontrol.Verify(envelope, other); !errors.Is(err, adaptercontrol.ErrSignature) {
		t.Fatalf("wrong key: err = %v, want ErrSignature", err)
	}
	if _, err := adaptercontrol.Verify(envelope, nil); !errors.Is(err, adaptercontrol.ErrNoTrustAnchor) {
		t.Fatalf("no key: err = %v, want ErrNoTrustAnchor", err)
	}

	// Tampering with the signed bytes, by even one character, fails verification
	// before the payload is ever parsed.
	var parsed adaptercontrol.Envelope
	if err := json.Unmarshal(envelope, &parsed); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(parsed.Payload)
	tampered := strings.Replace(string(payload), `"sequence":3`, `"sequence":9`, 1)
	if tampered == string(payload) {
		t.Fatal("test did not change the payload")
	}
	parsed.Payload = base64.StdEncoding.EncodeToString([]byte(tampered))
	forged, _ := json.Marshal(parsed)
	if _, err := adaptercontrol.Verify(forged, public); !errors.Is(err, adaptercontrol.ErrSignature) {
		t.Fatalf("tampered payload: err = %v, want ErrSignature", err)
	}
	parsed.Payload = base64.StdEncoding.EncodeToString(payload)
	signature, _ := base64.StdEncoding.DecodeString(parsed.Signature)
	signature[0] ^= 0xff
	parsed.Signature = base64.StdEncoding.EncodeToString(signature)
	flipped, _ := json.Marshal(parsed)
	if _, err := adaptercontrol.Verify(flipped, public); !errors.Is(err, adaptercontrol.ErrSignature) {
		t.Fatalf("flipped signature: err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsAnythingBeyondExactIDs(t *testing.T) {
	public, _ := controltest.PlaceholderKey()
	base := func(extra string) []byte {
		return []byte(`{"schema":"papio-adapter-control/1","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":["wiley@0.1.0"],"suspended_safety_domains":[]` + extra + `}`)
	}
	cases := map[string][]byte{
		"a host field":     base(`,"hosts":["www.example.com"]`),
		"a selector field": base(`,"selector":"a.pdf"`),
		"a url field":      base(`,"url":"https://example.com/"`),
		"a threshold":      base(`,"threshold":3`),
		"trailing data":    append(base(""), []byte(` {}`)...),
		"unsorted revisions": []byte(`{"schema":"papio-adapter-control/1","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":["wiley@0.1.0","sage@0.1.0"],"suspended_safety_domains":[]}`),
		"a revision that is a host": []byte(`{"schema":"papio-adapter-control/1","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":["https://www.example.com/x@1.0.0"],"suspended_safety_domains":[]}`),
		"a domain with a path": []byte(`{"schema":"papio-adapter-control/1","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":[],"suspended_safety_domains":["publisher:www.example.com/path"]}`),
		"a missing list": []byte(`{"schema":"papio-adapter-control/1","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test","suspended_revisions":[]}`),
		"sequence zero": []byte(`{"schema":"papio-adapter-control/1","sequence":0,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":[],"suspended_safety_domains":[]}`),
		"another schema": []byte(`{"schema":"papio-adapter-control/2","sequence":1,"issued_at":"2026-10-12T00:00:00Z","key_id":"papio-control-test",` +
			`"suspended_revisions":[],"suspended_safety_domains":[]}`),
	}
	for name, payload := range cases {
		if _, err := adaptercontrol.Verify(signRaw(t, payload), public); !errors.Is(err, adaptercontrol.ErrDocument) {
			t.Errorf("%s: err = %v, want ErrDocument even though the signature is valid", name, err)
		}
	}
	if _, err := adaptercontrol.Verify(signRaw(t, base("")), public); err != nil {
		t.Fatalf("the control case is itself valid: %v", err)
	}
	if _, err := adaptercontrol.Sign(adaptercontrol.Document{Sequence: 1, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "k",
		SuspendedRevisions: []string{"wiley@0.1.0", "wiley@0.1.0"}}, mustPrivate()); err == nil {
		t.Fatal("Sign built a document with a duplicate revision")
	}
}

func mustPrivate() ed25519.PrivateKey {
	_, private := controltest.PlaceholderKey()
	return private
}

func TestServiceSequenceRules(t *testing.T) {
	dir := t.TempDir()
	service := openService(t, dir)
	if got := service.Snapshot(); got.State != adaptercontrol.StateNone || got.Mandatory() {
		t.Fatalf("fresh service = %+v, want none and not mandatory", got)
	}

	if applied, err := service.Apply(signed(t, 5, []string{"sciencedirect@0.3.0"}, nil)); err != nil || !applied.Changed {
		t.Fatalf("first apply = %+v, %v", applied, err)
	}
	snapshot := service.Snapshot()
	if snapshot.Sequence != 5 || !snapshot.RevisionSuspended("sciencedirect@0.3.0") || snapshot.RevisionSuspended("wiley@0.1.0") {
		t.Fatalf("snapshot = %+v: only the named revision may be suspended", snapshot)
	}

	// A replay or older sequence is refused and changes nothing.
	for _, sequence := range []int64{4, 1} {
		if _, err := service.Apply(signed(t, sequence, nil, nil)); !errors.Is(err, adaptercontrol.ErrOlderSequence) {
			t.Fatalf("sequence %d: err = %v, want ErrOlderSequence", sequence, err)
		}
	}
	if !service.Snapshot().RevisionSuspended("sciencedirect@0.3.0") {
		t.Fatal("an older document lifted the suspension")
	}
	// The identical document is a no-op; a different one at the same sequence is a fork.
	if applied, err := service.Apply(signed(t, 5, []string{"sciencedirect@0.3.0"}, nil)); err != nil || applied.Changed {
		t.Fatalf("identical document = %+v, %v; want unchanged", applied, err)
	}
	if _, err := service.Apply(signed(t, 5, []string{"wiley@0.1.0"}, nil)); !errors.Is(err, adaptercontrol.ErrSequenceConflict) {
		t.Fatalf("fork: err = %v, want ErrSequenceConflict", err)
	}
	// A bad signature never reaches the sequence rules.
	other, otherPrivate := controltest.OtherKey()
	_ = other
	foreign, err := adaptercontrol.Sign(adaptercontrol.Document{Sequence: 99, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "k"}, otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(foreign); !errors.Is(err, adaptercontrol.ErrSignature) {
		t.Fatalf("foreign key: err = %v, want ErrSignature", err)
	}
	if service.Snapshot().Sequence != 5 {
		t.Fatal("a document signed by another key advanced the sequence")
	}

	// A higher sequence replaces the whole set: this is how a suspension lifts.
	if applied, err := service.Apply(signed(t, 6, nil, nil)); err != nil || !applied.Changed {
		t.Fatalf("lift = %+v, %v", applied, err)
	}
	lifted := service.Snapshot()
	if lifted.Sequence != 6 || len(lifted.Revisions) != 0 || lifted.State != adaptercontrol.StateCurrent || !lifted.Mandatory() {
		t.Fatalf("lifted = %+v: sequence rises, suspension clears, control stays mandatory", lifted)
	}
}

func TestServiceStateSurvivesRestartAndFailsClosedWhenDamaged(t *testing.T) {
	dir := t.TempDir()
	service := openService(t, dir)
	if _, err := service.Apply(signed(t, 7, []string{"sciencedirect@0.3.0"}, []string{"publisher:www.sciencedirect.com"})); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, adaptercontrol.DocumentFileName))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("persisted control mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}

	restarted := openService(t, dir)
	got := restarted.Snapshot()
	if got.State != adaptercontrol.StateCurrent || got.Sequence != 7 || !got.RevisionSuspended("sciencedirect@0.3.0") || !got.DomainSuspended("publisher:www.sciencedirect.com") {
		t.Fatalf("after restart = %+v: suspension must survive a daemon restart", got)
	}
	// The restarted daemon still refuses a rollback.
	if _, err := restarted.Apply(signed(t, 3, nil, nil)); !errors.Is(err, adaptercontrol.ErrOlderSequence) {
		t.Fatalf("rollback after restart: %v", err)
	}

	// Editing the persisted file cannot forge or lift anything: it stops
	// verifying, and the daemon reports unavailable rather than none.
	path := filepath.Join(dir, adaptercontrol.DocumentFileName)
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"key_id"`, `"key_id "`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := openService(t, dir).Snapshot()
	if damaged.State != adaptercontrol.StateUnavailable || !damaged.Mandatory() || damaged.Reason == "" {
		t.Fatalf("damaged file = %+v, want unavailable with a reason", damaged)
	}
	// A build that lost its key cannot trust the file either.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := adaptercontrol.Open(dir, nil).Snapshot(); got.State != adaptercontrol.StateUnavailable {
		t.Fatalf("keyless build over a stored document = %+v, want unavailable", got)
	}
	// A valid document repairs an unavailable service at any sequence.
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	repaired := openService(t, dir)
	if _, err := repaired.Apply(signed(t, 2, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := repaired.Snapshot(); got.State != adaptercontrol.StateCurrent || got.Sequence != 2 {
		t.Fatalf("repaired = %+v", got)
	}
}

func controlServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestRunnerDistinguishesNoControlFromAFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	service := openService(t, dir)
	var body atomic.Value
	body.Store([]byte(nil))
	status := atomic.Int32{}
	status.Store(http.StatusNotFound)
	server, hits := controlServer(t, func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "x", code)
			return
		}
		_, _ = w.Write(body.Load().([]byte))
	})
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	runner := &adaptercontrol.Runner{Service: service, URL: server.URL, Now: func() time.Time { return now }}
	step := func() adaptercontrol.Status {
		now = now.Add(25 * time.Hour)
		if err := runner.RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		return service.Status()
	}

	// 404: nothing published. Not a failure, and the daemon holds "none".
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeNonePublished || service.Snapshot().State != adaptercontrol.StateNone {
		t.Fatalf("404 => %+v / %s; want none_published and state none", got, service.Snapshot().State)
	}
	// 500: a fetch failure, never state.
	status.Store(http.StatusInternalServerError)
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeFetchFailed || service.Snapshot().State != adaptercontrol.StateNone {
		t.Fatalf("500 => %+v", got)
	}
	// A valid document publishes.
	status.Store(http.StatusOK)
	body.Store(signed(t, 4, []string{"sciencedirect@0.3.0"}, nil))
	if got := step(); got.LastOutcome != adaptercontrol.OutcomePublished || got.LastSuccessAt.IsZero() || !service.Snapshot().RevisionSuspended("sciencedirect@0.3.0") {
		t.Fatalf("publish => %+v", got)
	}
	// The same document again is unchanged.
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeUnchanged {
		t.Fatalf("repeat => %+v", got)
	}
	// None of: a 404, an outage, a forged document, a replayed older sequence
	// may relax what is held.
	status.Store(http.StatusNotFound)
	step()
	status.Store(http.StatusBadGateway)
	step()
	status.Store(http.StatusOK)
	_, otherPrivate := controltest.OtherKey()
	forged, _ := adaptercontrol.Sign(adaptercontrol.Document{Sequence: 50, IssuedAt: "2026-10-12T00:00:00Z", KeyID: "k"}, otherPrivate)
	body.Store(forged)
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeVerificationFailed {
		t.Fatalf("forged => %+v", got)
	}
	body.Store(signed(t, 2, nil, nil))
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeRejectedOlder {
		t.Fatalf("replay => %+v", got)
	}
	body.Store([]byte(`{"not":"an envelope"}`))
	if got := step(); got.LastOutcome != adaptercontrol.OutcomeVerificationFailed {
		t.Fatalf("garbage => %+v", got)
	}
	held := service.Snapshot()
	if held.Sequence != 4 || !held.RevisionSuspended("sciencedirect@0.3.0") {
		t.Fatalf("held = %+v: the suspension must survive every failure", held)
	}
	// A higher sequence lifts it.
	body.Store(signed(t, 5, nil, nil))
	if got := step(); got.LastOutcome != adaptercontrol.OutcomePublished || service.Snapshot().RevisionSuspended("sciencedirect@0.3.0") {
		t.Fatalf("lift => %+v", got)
	}
	if hits.Load() == 0 {
		t.Fatal("the test server was never contacted")
	}
}

func TestRunnerKeepsTheDailyCadenceAndNeedsAKey(t *testing.T) {
	ctx := context.Background()
	server, hits := controlServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	dir := t.TempDir()
	service := openService(t, dir)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	runner := &adaptercontrol.Runner{Service: service, URL: server.URL, Now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if err := runner.RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits within a day = %d, want 1", hits.Load())
	}
	now = now.Add(24 * time.Hour)
	if err := runner.RunDue(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("hits after the cadence = %d, err %v; want 2", hits.Load(), err)
	}
	// The attempt clock is on disk, so a daemon restart does not refetch within the day.
	restarted := &adaptercontrol.Runner{Service: openService(t, dir), URL: server.URL, Now: func() time.Time { return now.Add(time.Hour) }}
	if err := restarted.RunDue(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("a restart refetched inside the cadence: hits = %d", hits.Load())
	}
	keyless := &adaptercontrol.Runner{Service: adaptercontrol.Open(t.TempDir(), nil), URL: server.URL}
	if err := keyless.RunDue(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("a build without a key contacted the server: hits = %d", hits.Load())
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{adaptercontrol.DefaultURL, "https://example.org/c.json", "http://127.0.0.1:8080/c.json", "http://localhost/c.json", "http://[::1]:9/c.json"} {
		if err := adaptercontrol.ValidateURL(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://example.org/c.json", "ftp://example.org/c.json", "https://user:pw@example.org/c.json", "https://example.org/c.json#x", "https:///c.json", "file:///etc/passwd"} {
		if err := adaptercontrol.ValidateURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestEmbeddedKeyIsNeverThePublicTestKey pins that the binary trusts a key
// whose private half is not public: the placeholder test key derives from a
// public string, so embedding it would let anyone sign control.
func TestEmbeddedKeyIsNeverThePublicTestKey(t *testing.T) {
	key, err := adaptercontrol.TrustedKey()
	if err != nil {
		t.Fatal(err)
	}
	placeholder, _ := controltest.PlaceholderKey()
	if key != nil && key.Equal(placeholder) {
		t.Fatal("the embedded control key is the public placeholder test key, which signs anything")
	}
	if _, err := adaptercontrol.ParsePublicKey("not base64!"); err == nil {
		t.Fatal("a malformed key was accepted")
	}
}
