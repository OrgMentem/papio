// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/acquisitionagent"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/nativeviewer"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/work"
)

type viewerTestDriver struct {
	prepare func(context.Context, nativeviewer.Request) (nativeviewer.Session, error)
}

func (d viewerTestDriver) Prepare(ctx context.Context, r nativeviewer.Request) (nativeviewer.Session, error) {
	return d.prepare(ctx, r)
}

type viewerTestSession struct {
	advances atomic.Int32
	closed   atomic.Bool
	advance  func(context.Context) (nativeviewer.Status, error)
}

func (s *viewerTestSession) Advance(ctx context.Context) (nativeviewer.Status, error) {
	s.advances.Add(1)
	return s.advance(ctx)
}
func (s *viewerTestSession) Close() error { s.closed.Store(true); return nil }

func viewerFixture(t *testing.T, body []byte) (*Bridge, *job.Store, string, string, protocol.NativeViewerSaveRequestV1Payload, *viewerTestSession) {
	t.Helper()
	source := t.TempDir()
	landing := filepath.Join(source, "papio")
	if err := os.Mkdir(landing, 0700); err != nil {
		t.Fatal(err)
	}
	b, jobs, _, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) { c.Browser.AdoptionRoot = landing })
	b.svc.Validate = adoptionProbeValidate
	session := &viewerTestSession{}
	b.SetNativeViewerDriver(viewerTestDriver{func(ctx context.Context, r nativeviewer.Request) (nativeviewer.Session, error) {
		session.advance = func(context.Context) (nativeviewer.Status, error) {
			return nativeviewer.Saved, os.WriteFile(filepath.Join(source, r.Filename), body, 0600)
		}
		return session, nil
	}})
	t.Cleanup(b.CloseAcquisitionBackend)
	runSync(t, b, helloWithFeatures(t, "1.2.3", effectPermitFeature, nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature))
	id := parkManualDownload(t, jobs, "viewer-test", handoffWork())
	actions, err := jobs.ListOpenHumanActionsForJobs(context.Background(), []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatal(actions, err)
	}
	if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET diagnosis=? WHERE id=?`, job.DiagnosisReasonNativeViewerDownload, actions[0].ID); err != nil {
		t.Fatal(err)
	}
	p := protocol.NativeViewerSaveRequestV1Payload{RequestID: "prepare-1", ActionID: actions[0].ID, ActionRevision: actions[0].Revision, BrowserEpoch: "browser-epoch", DocumentID: "document-1", SourceURL: "https://publisher.example/paper.pdf?secret=transient#page=2", Step: "prepare"}
	return b, jobs, id, source, p, session
}
func viewerRequest(t *testing.T, b *Bridge, id string, p protocol.NativeViewerSaveRequestV1Payload) *protocol.NativeViewerSaveResultV1Payload {
	t.Helper()
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgNativeViewerSaveRequestV1, id, p))
	m := firstOfType(msgs, protocol.MsgNativeViewerSaveResultV1)
	if m == nil {
		t.Fatalf("missing viewer reply: %+v", msgs)
	}
	return m.Payload.(*protocol.NativeViewerSaveResultV1Payload)
}
func viewerPrepare(t *testing.T, b *Bridge, id string, p protocol.NativeViewerSaveRequestV1Payload) protocol.NativeViewerSaveRequestV1Payload {
	t.Helper()
	r := viewerRequest(t, b, id, p)
	if r.Outcome != "prepared" {
		t.Fatalf("prepare=%+v", r)
	}
	p.OperationID = r.OperationID
	p.Selection = ""
	p.Step = "advance"
	p.RequestID = "advance-1"
	return p
}
func TestNativeViewerSaveValidatedArtifactAndDuplicate(t *testing.T) {
	body := adoptionProbePDF(handoffWork().DOI)
	b, jobs, id, _, p, session := viewerFixture(t, body)
	p = viewerPrepare(t, b, id, p)
	if got := viewerRequest(t, b, id, p); got.Outcome != "pending" {
		t.Fatal(got)
	}
	if got := viewerRequest(t, b, id, p); got.Outcome != "pending" {
		t.Fatal(got)
	}
	if session.advances.Load() != 1 {
		t.Fatal("duplicate native effect")
	}
	p.RequestID = "advance-2"
	if got := viewerRequest(t, b, id, p); got.Outcome != "ready" {
		row, _ := jobs.Get(context.Background(), id)
		t.Fatalf("result=%+v row=%+v", got, row)
	}
	sum := sha256.Sum256(body)
	row, _ := jobs.Get(context.Background(), id)
	if row.State != job.StateReady || row.ArtifactSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(row)
	}
	if session.advances.Load() != 1 {
		t.Fatal("save dispatched twice")
	}
	if got := viewerRequest(t, b, id, p); got.Outcome != "ready" {
		t.Fatal(got)
	}
	events, _ := jobs.Events(context.Background(), id)
	for _, e := range events {
		if text := fmt.Sprint(e); containsViewerSecret(text) {
			t.Fatal("source URL persisted")
		}
	}
}
func containsViewerSecret(text string) bool {
	return strings.Contains(text, "secret=transient") || strings.Contains(text, "#page=2")
}

func TestNativeViewerSaveValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{{"HTML", []byte("<html>Login</html>"), "rejected"}, {"wrong work", adoptionProbePDF("10.1234/wrong.work"), "review"}} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, id, _, p, _ := viewerFixture(t, tc.body)
			p = viewerPrepare(t, b, id, p)
			viewerRequest(t, b, id, p)
			p.RequestID = "advance-2"
			if got := viewerRequest(t, b, id, p); got.Outcome != tc.want {
				t.Fatal(got)
			}
		})
	}
}
func TestNativeViewerSavePurePrepareFailureDoesNotLatch(t *testing.T) {
	b, jobs, id, _, p, _ := viewerFixture(t, nil)
	b.SetNativeViewerDriver(viewerTestDriver{func(context.Context, nativeviewer.Request) (nativeviewer.Session, error) {
		return nil, errors.New("permission denied")
	}})
	if got := viewerRequest(t, b, id, p); got.Outcome != "unavailable" {
		t.Fatal(got)
	}
	if n := nativeEventCount(t, jobs, id, "browser.native_viewer_save_reserved"); n != 0 {
		t.Fatal("pure prepare consumed action")
	}
}
func TestNativeViewerSaveLostStateCannotRearm(t *testing.T) {
	b, _, id, _, p, _ := viewerFixture(t, nil)
	viewerPrepare(t, b, id, p)
	b.mu.Lock()
	r := b.nativeViewerSaves[id]
	b.stopNativeViewer(r, "stale", "authority_lost")
	delete(b.nativeViewerSaves, id)
	b.mu.Unlock()
	p.RequestID = "prepare-2"
	if got := viewerRequest(t, b, id, p); got.Outcome != "refused" {
		t.Fatal(got)
	}
}
func TestNativeViewerSaveActionDocumentCancellation(t *testing.T) {
	for _, change := range []string{"revision", "document", "cancel", "timeout", "disconnect"} {
		t.Run(change, func(t *testing.T) {
			b, jobs, id, _, p, s := viewerFixture(t, nil)
			p = viewerPrepare(t, b, id, p)
			switch change {
			case "revision":
				if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, p.ActionID); err != nil {
					t.Fatal(err)
				}
			case "document":
				p.DocumentID = "different-document"
			case "cancel":
				p.Step = "cancel"
			case "timeout":
				b.mu.Lock()
				b.nativeViewerSaves[id].cancel()
				b.mu.Unlock()
			case "disconnect":
				b.mu.Lock()
				b.retireNativeViewerSaves(b.nativeViewerSaves[id].sessionID)
				b.mu.Unlock()
			}
			if got := viewerRequest(t, b, id, p); got.Outcome != "stale" {
				t.Fatal(got)
			}
			if s.advances.Load() != 0 {
				t.Fatal("stale native click")
			}
		})
	}
}
func TestNativeViewerSaveRootLossAndPartPending(t *testing.T) {
	for _, change := range []string{"root", "part", "missing"} {
		t.Run(change, func(t *testing.T) {
			b, _, id, source, p, s := viewerFixture(t, adoptionProbePDF(handoffWork().DOI))
			p = viewerPrepare(t, b, id, p)
			if change == "root" {
				if err := os.Rename(filepath.Join(source, "papio"), filepath.Join(source, "old")); err != nil {
					t.Fatal(err)
				}
			}
			if change == "missing" {
				s.advance = func(context.Context) (nativeviewer.Status, error) { return nativeviewer.Saved, nil }
			}
			if change == "part" {
				if err := os.WriteFile(filepath.Join(source, "papio-viewer-"+p.OperationID+".pdf.part"), []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := viewerRequest(t, b, id, p)
			want := "pending"
			if change == "root" {
				want = "refused"
			}
			if got.Outcome != want {
				t.Fatal(got)
			}
			p.RequestID = "advance-2"
			viewerRequest(t, b, id, p)
			if s.advances.Load() != 1 {
				t.Fatal("repeated save")
			}
		})
	}
}
func TestNativeViewerSaveHelloCapAndPoll(t *testing.T) {
	b, jobs, id, _, p, _ := viewerFixture(t, nil)
	b.mu.Lock()
	ack, err := b.helloAck(sessionRoleHolder, SessionRolesMinExtensionVersion, []string{nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature})
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeBrowserMessage(ack)
	if err != nil {
		t.Fatal(err)
	}
	features := decoded.Payload.(*protocol.HelloAckPayload).Features
	if len(features) > 32 || !slices.Contains(features, protocol.NativeViewerSaveFeature) || slices.Contains(features, nativeViewerDownloadFeature) {
		t.Fatal(features)
	}
	viewerPrepare(t, b, id, p)
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest) != nil {
		t.Fatal("native session sent generic reconciliation")
	}
	permit, err := jobs.LiveEffectPermit(context.Background())
	if err != nil || permit == nil || permit.Status != job.Held {
		t.Fatal(permit, err)
	}

}

func TestNativeViewerSaveHTTPAndCancellationOccupancy(t *testing.T) {
	for _, begun := range []bool{false, true} {
		t.Run(fmt.Sprint(begun), func(t *testing.T) {
			b, jobs, id, _, p, s := viewerFixture(t, nil)
			p.SourceURL = "http://example.invalid/p.pdf"
			p = viewerPrepare(t, b, id, p)
			if begun {
				s.advance = func(context.Context) (nativeviewer.Status, error) { return nativeviewer.Pending, nil }
				viewerRequest(t, b, id, p)
			}
			b.mu.Lock()
			permitID := b.nativeViewerSaves[id].record.PermitID
			b.mu.Unlock()
			p.Step = "cancel"
			p.RequestID = "cancel-1"
			viewerRequest(t, b, id, p)
			permit, err := jobs.GetEffectPermit(context.Background(), permitID)
			want := job.Settled
			if begun {
				want = job.UnknownCompletion
			}
			if err != nil || permit.Status != want {
				t.Fatal(permit, err)
			}
		})
	}
}

func TestNativeViewerSaveRejectsBrowserProducer(t *testing.T) {
	b, jobs, id, _, p, _ := viewerFixture(t, nil)
	viewerPrepare(t, b, id, p)
	b.mu.Lock()
	r := b.nativeViewerSaves[id].record
	b.mu.Unlock()
	producer := protocol.ArtifactProducerPayload{EffectKind: string(r.Producer.Kind), DriveAttemptID: r.Producer.DriveAttemptID, Ordinal: r.Producer.Ordinal, Strategy: r.Producer.Strategy, Revision: r.Producer.Revision}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, protocol.DownloadCompletePayload{DownloadID: 3, Filename: "forged.pdf", SizeBytes: 500, Producer: &producer}))
	if firstOfType(msgs, protocol.MsgError) == nil {
		t.Fatal("forged native producer accepted")
	}
	if nativeEventCount(t, jobs, id, "browser.download_complete") != 0 {
		t.Fatal("forged correlation persisted")
	}
}

func TestNativeViewerSaveSanitizedAndRestartReceipt(t *testing.T) {
	b, jobs, id, _, p, _ := viewerFixture(t, adoptionProbePDF(handoffWork().DOI))
	enableAdoptionSanitization(t, b)
	p = viewerPrepare(t, b, id, p)
	viewerRequest(t, b, id, p)
	p.RequestID = "advance-2"
	if got := viewerRequest(t, b, id, p); got.Outcome != "ready" {
		t.Fatal(got)
	}
	b.mu.Lock()
	record, digest := b.nativeViewerSaves[id].record, b.nativeViewerSaves[id].digest
	b.mu.Unlock()
	fresh := NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, b.cfg, b.Version)
	if outcome, reason := fresh.nativeViewerOutcome(context.Background(), &nativeViewerSave{record: record, digest: digest}, false); outcome != "ready" {
		t.Fatal(outcome, reason)
	}
	row, _ := jobs.Get(context.Background(), id)
	if row.ArtifactSHA256 == digest {
		t.Fatal("test did not sanitize")
	}
}

func TestNativeViewerSavePublicationFailureRetainsAdmittedStage(t *testing.T) {
	b, jobs, id, source, p, _ := viewerFixture(t, adoptionProbePDF(handoffWork().DOI))
	p = viewerPrepare(t, b, id, p)
	viewerRequest(t, b, id, p)
	filename := "papio-viewer-" + p.OperationID + ".pdf"
	// An exclusive final-name collision makes publication fail after admission.
	if err := os.MkdirAll(filepath.Join(source, "papio", id, filename), 0700); err != nil {
		t.Fatal(err)
	}
	p.RequestID = "advance-2"
	got := viewerRequest(t, b, id, p)
	if got.Outcome != "unavailable" || got.Reason != "source_rejected" {
		t.Fatal(got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_viewer_save_admitted") != 1 {
		t.Fatal("missing admission")
	}
	if _, err := os.Stat(filepath.Join(source, "papio", "native_stage_"+p.OperationID+".tmp")); err != nil {
		t.Fatal("admitted staging lost", err)
	}
	b.mu.Lock()
	record := b.nativeViewerSaves[id].record
	b.mu.Unlock()
	permit, _ := jobs.GetEffectPermit(context.Background(), record.PermitID)
	if permit.Status != job.UnknownCompletion {
		t.Fatal(permit)
	}
}

func TestNativeViewerSaveHolderPromotionStopsOwnedHelper(t *testing.T) {
	b, jobs, id, _, p, s := viewerFixture(t, nil)
	viewerPrepare(t, b, id, p)
	runSyncAs(t, b, sessB, helloWithFeatures(t, "1.2.3", effectPermitFeature, nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature))
	if _, err := b.Claim(sessB); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !s.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.closed.Load() {
		t.Fatal("holder promotion did not stop helper")
	}
	b.mu.Lock()
	record := b.nativeViewerSaves[id].record
	b.mu.Unlock()
	permit, _ := jobs.GetEffectPermit(context.Background(), record.PermitID)
	if permit.Status != job.Settled {
		t.Fatal(permit)
	}
	msgs, _ := runSyncAs(t, b, sessB)
	if firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest) != nil {
		t.Fatal("generic reconcile for native viewer")
	}
}

// TestNativeViewerLiveFixture operates the already-open synthetic Firefox PDF.
// It is never enabled by ordinary CI or a local go test invocation. All stores
// are isolated; only its exact generated filename and job directory touch the
// real download root. The parent harness owns Firefox setup and fixture launch.
func TestNativeViewerLiveFixture(t *testing.T) {
	fixturePath := os.Getenv("PAPIO_NATIVE_VIEWER_FIXTURE")
	if fixturePath == "" {
		t.Skip("manual native fixture requires PAPIO_NATIVE_VIEWER_FIXTURE")
	}
	helperPath, workerPath, evidenceDir := os.Getenv("PAPIO_NATIVE_VIEWER_HELPER"), os.Getenv("PAPIO_NATIVE_VIEWER_WORKER"), os.Getenv("PAPIO_NATIVE_VIEWER_EVIDENCE")
	for name, path := range map[string]string{"fixture": fixturePath, "helper": helperPath, "worker": workerPath, "evidence": evidenceDir} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s must be an explicit absolute path", name)
		}
	}
	var fixture struct {
		PDFURL    string `json:"pdfURL"`
		RevokeURL string `json:"revokeURL"`
		StatusURL string `json:"statusURL"`
		Prefix    string `json:"prefix"`
		Filename  string `json:"filename"`
		SHA256    string `json:"sha256"`
		Bytes     int64  `json:"bytes"`
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^/papio-native-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(fixture.Prefix) || fixture.Filename != strings.TrimPrefix(fixture.Prefix, "/")+".pdf" || fixture.Bytes != 5661 {
		t.Fatal("not the nonce-bound public native fixture")
	}
	if digest, err := hex.DecodeString(fixture.SHA256); err != nil || len(digest) != 32 {
		t.Fatal("invalid fixture digest")
	}
	origin := ""
	for value, path := range map[string]string{fixture.PDFURL: fixture.Prefix + "/" + fixture.Filename, fixture.RevokeURL: fixture.Prefix + "/control/revoke", fixture.StatusURL: fixture.Prefix + "/control/status"} {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != path || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			t.Fatal("fixture control is not exact loopback nonce route")
		}
		if origin != "" && origin != u.Host {
			t.Fatal("fixture origins differ")
		}
		origin = u.Host
	}
	for _, path := range []string{helperPath, workerPath} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			t.Fatal("explicit helper/worker executable unavailable")
		}
	}
	driver := nativeviewer.NewDriver(helperPath)
	if driver == nil {
		t.Fatal("native viewer helper unavailable on this host")
	}
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	trace, err := newViewerFixtureTrace(filepath.Join(evidenceDir, "driver-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	driver = viewerFixtureTraceDriver{driver: driver, trace: trace}
	capability := pdf.DetectCapability()
	if capability.PDFInfo == "" || capability.PDFToText == "" {
		t.Fatal("real pdfinfo and pdftotext required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(home, "Downloads")
	landing := filepath.Join(source, "papio")
	if info, err := os.Stat(landing); err != nil || !info.IsDir() {
		t.Fatal("manual fixture requires existing ~/Downloads/papio")
	}
	b, jobs, cfg, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) { c.Browser.AdoptionRoot = landing })
	t.Cleanup(b.CloseAcquisitionBackend)
	options := pdf.ValidationOptions{Structural: pdf.DefaultStructuralOptions(), Semantic: pdf.DefaultSemanticOptions(), TitleMatchThreshold: cfg.PDF.TitleMatchThreshold}
	options.Semantic.MinChars = cfg.PDF.MinTextChars
	options.Semantic.OCRPages = cfg.PDF.MaxOCRPages
	var validationMu sync.Mutex
	var validation pdf.ValidationReport
	b.svc.Validate = func(ctx context.Context, path, mime string, target work.Work) (pdf.ValidationReport, error) {
		report, err := pdf.ValidateFile(ctx, pdf.ValidationInput{Path: path, DeclaredMIME: mime, WorkerBinary: workerPath, Capability: capability, Target: target}, options)
		validationMu.Lock()
		validation = report
		validationMu.Unlock()
		return report, err
	}
	b.svc.Sanitize = func(ctx context.Context, path, dest string) (pdf.StructuralReport, error) {
		return pdf.SanitizeEmbeddedFiles(ctx, workerPath, path, dest, options.Structural)
	}
	b.SetNativeViewerDriver(driver)
	runSync(t, b, helloWithFeatures(t, "1.2.3", effectPermitFeature, nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature))
	id := parkManualDownload(t, jobs, job.NewID("fixture"), work.Work{DOI: "10.5555/sentinel.wrap.003", Title: "Network Embedding With Adaptive Sampling for Community Detection"})
	actions, err := jobs.ListOpenHumanActionsForJobs(context.Background(), []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatal(actions, err)
	}
	if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET diagnosis=? WHERE id=?`, job.DiagnosisReasonNativeViewerDownload, actions[0].ID); err != nil {
		t.Fatal(err)
	}
	p := protocol.NativeViewerSaveRequestV1Payload{RequestID: "fixture-prepare", ActionID: actions[0].ID, ActionRevision: actions[0].Revision, BrowserEpoch: job.NewID("epoch"), DocumentID: job.NewID("document"), SourceURL: fixture.PDFURL, Step: "prepare"}
	p = viewerPrepare(t, b, id, p)
	filename := "papio-viewer-" + p.OperationID + ".pdf"
	t.Cleanup(func() {
		b.CloseAcquisitionBackend()
		_ = os.Remove(filepath.Join(source, filename))
		_ = os.Remove(filepath.Join(source, filename+".part"))
		_ = os.Remove(filepath.Join(landing, "native_stage_"+p.OperationID+".tmp"))
		_ = os.RemoveAll(filepath.Join(landing, id))
	})
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture redirect refused") }}
	control := func(method, target string) int64 {
		t.Helper()
		req, err := http.NewRequest(method, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var state struct {
			Revoked        bool  `json:"revoked"`
			AlreadyRevoked bool  `json:"alreadyRevoked"`
			Sequence       int64 `json:"revocationSequence"`
		}
		if res.StatusCode != 200 {
			t.Fatalf("fixture control HTTP %d", res.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&state); err != nil {
			t.Fatal(err)
		}
		if !state.Revoked || state.Sequence < 1 || (method == "POST" && state.AlreadyRevoked) {
			t.Fatal("fixture did not establish fresh irreversible revocation")
		}
		return state.Sequence
	}
	revokedAt := control("POST", fixture.RevokeURL) // after pure prepare, before ANY native Advance
	var result *protocol.NativeViewerSaveResultV1Payload
	deadline := time.Now().Add(nativeViewerLifetime)
	for n := 0; time.Now().Before(deadline); n++ {
		p.RequestID = fmt.Sprintf("fixture-advance-%d", n)
		result = viewerRequest(t, b, id, p)
		if result.Outcome != "pending" {
			break
		}
		time.Sleep(750 * time.Millisecond)
	}
	if result == nil || result.Outcome != "ready" {
		t.Fatalf("native fixture did not become ready: %+v", result)
	}
	if control("GET", fixture.StatusURL) != revokedAt {
		t.Fatal("fixture revocation changed")
	}
	journal, err := os.ReadFile(filepath.Join(filepath.Dir(fixturePath), "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	pdfBefore := 0
	for _, line := range bytes.Split(bytes.TrimSpace(journal), []byte("\n")) {
		var entry struct {
			Sequence int64  `json:"sequence"`
			Kind     string `json:"responseKind"`
			Bytes    int64  `json:"bytes"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Kind == "pdf" && entry.Bytes > 0 {
			if entry.Sequence >= revokedAt {
				t.Fatal("PDF bytes served after prepare/revocation")
			}
			pdfBefore++
		}
	}
	if pdfBefore == 0 {
		t.Fatal("no original PDF response recorded before revocation")
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateReady || row.ArtifactSHA256 != fixture.SHA256 {
		t.Fatalf("wrong ready artifact: %+v %v", row, err)
	}
	artifactPath, err := b.svc.Artifacts.ArtifactPath(row.ArtifactSHA256)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(artifact)
	if hex.EncodeToString(sum[:]) != fixture.SHA256 || int64(len(artifact)) != fixture.Bytes {
		t.Fatal("artifact oracle mismatch")
	}
	validationMu.Lock()
	report := validation
	validationMu.Unlock()
	if !report.Structural.Valid || report.Structural.Pages != 3 || report.Identity.Result != pdf.IdentityPass {
		t.Fatalf("normal PDF validation did not pass: %+v", report)
	}
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := map[string]any{"job_id": id, "operation_id": p.OperationID, "outcome": "ready", "sha256": row.ArtifactSHA256, "pages": report.Structural.Pages, "bytes": len(artifact), "revocation_sequence": revokedAt, "pdf_responses_after_revocation": 0}
	resultJSON, _ := json.MarshalIndent(evidence, "", "  ")
	for name, body := range map[string][]byte{filename: artifact, p.OperationID + "-result.json": resultJSON, p.OperationID + "-requests.jsonl": journal} {
		f, err := os.OpenFile(filepath.Join(evidenceDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.Write(body)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal(writeErr, closeErr)
		}
	}
	t.Logf("ready sha256=%s pages=%d bytes=%d evidence=%s", row.ArtifactSHA256, report.Structural.Pages, len(artifact), evidenceDir)
}

func TestNativeViewerSaveExplicitSelection(t *testing.T) {
	for _, selection := range []string{"", "automatic", "explicit"} {
		name := selection
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			b, jobs, id, _, p, _ := viewerFixture(t, nil)
			if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET diagnosis=? WHERE id=?`, job.DiagnosisReasonProviderAdapterMissing, p.ActionID); err != nil {
				t.Fatal(err)
			}
			p.Selection = selection
			got := viewerRequest(t, b, id, p)
			if selection == "explicit" {
				if got.Outcome != "prepared" {
					t.Fatal(got)
				}
			} else if got.Outcome != "stale" {
				t.Fatal(got)
			}
		})
	}
}

func TestNativeViewerSaveAdvanceReleasesBridgeLockAndCancels(t *testing.T) {
	b, jobs, id, _, p, s := viewerFixture(t, nil)
	p = viewerPrepare(t, b, id, p)
	entered := make(chan struct{})
	s.advance = func(ctx context.Context) (nativeviewer.Status, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	frame := inFrame(t, protocol.MsgNativeViewerSaveRequestV1, id, p)
	completed := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{frame})
		completed <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("helper did not start")
	}
	// Ordinary poll and an exact duplicate must proceed while native work waits.
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest) != nil {
		t.Fatal("native worker reconciled as generic")
	}
	if got := viewerRequest(t, b, id, p); got.Outcome != "pending" {
		t.Fatal(got)
	}
	p.Step = "cancel"
	p.RequestID = "cancel-running"
	viewerRequest(t, b, id, p)
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel failed to release native worker")
	}
	if s.advances.Load() != 1 || !s.closed.Load() {
		t.Fatal("owned helper did not close exactly after the single dispatch")
	}
	b.mu.Lock()
	permitID := b.nativeViewerSaves[id].record.PermitID
	b.mu.Unlock()
	permit, _ := jobs.GetEffectPermit(context.Background(), permitID)
	if permit.Status != job.UnknownCompletion {
		t.Fatal(permit)
	}
}

func TestNativeViewerSaveAllFeaturePacking(t *testing.T) {
	b, _, _, _, _, _ := viewerFixture(t, nil)
	b.SetAcquisitionBackend(agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		return acquisitionagent.Decision{}, nil
	}))
	peer := []string{nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature, agentFallbackFeature, protocol.NativeClickAdoptionFeature, protocol.AgentNavigationFeature}
	for _, enabled := range []bool{true, false} {
		if !enabled {
			b.SetNativeViewerDriver(nil)
		}
		b.mu.Lock()
		raw, err := b.helloAck(sessionRoleHolder, SessionRolesMinExtensionVersion, peer)
		b.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		features := msg.Payload.(*protocol.HelloAckPayload).Features
		if len(features) != 32 || slices.Contains(features, protocol.NativeViewerSaveFeature) != enabled || slices.Contains(features, nativeViewerDownloadFeature) == enabled {
			t.Fatal(features)
		}
		for _, required := range []string{triageSnapshotFeature, agentFallbackFeature, protocol.NativeClickAdoptionFeature, protocol.AgentNavigationFeature} {
			if !slices.Contains(features, required) {
				t.Fatal("packing removed unrelated capability", required)
			}
		}
	}
}

// The concrete production driver returns only fixed errors, context errors and
// whitelisted helper rejection codes. Never trace Request or native surface
// data here: this private acceptance trace contains no URL or filename.
type viewerFixtureTrace struct {
	mu   sync.Mutex
	path string
}
type viewerFixtureDriverEvent struct {
	Timestamp string `json:"timestamp"`
	Method    string `json:"method"`
	Status    string `json:"status"`
	Error     string `json:"error"`
}

func newViewerFixtureTrace(path string) (*viewerFixtureTrace, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return &viewerFixtureTrace{path: path}, nil
}
func (trace *viewerFixtureTrace) record(method, status string, err error) error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	event := viewerFixtureDriverEvent{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Method: method, Status: status}
	if err != nil {
		event.Error = err.Error()
	}
	// Each event closes its descriptor. Asynchronous owned-session cleanup may
	// append after the test's synchronous bridge Close without racing file close.
	file, openErr := os.OpenFile(trace.path, os.O_WRONLY|os.O_APPEND, 0600)
	if openErr != nil {
		return errors.New("native fixture trace write failed")
	}
	writeErr := json.NewEncoder(file).Encode(event)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("native fixture trace write failed")
	}
	return nil
}

type viewerFixtureTraceDriver struct {
	driver nativeviewer.Driver
	trace  *viewerFixtureTrace
}

func (d viewerFixtureTraceDriver) Prepare(ctx context.Context, request nativeviewer.Request) (nativeviewer.Session, error) {
	session, err := d.driver.Prepare(ctx, request)
	status := ""
	if err == nil {
		status = "prepared"
	}
	if traceErr := d.trace.record("prepare", status, err); traceErr != nil {
		if session != nil {
			_ = session.Close()
		}
		return nil, traceErr
	}
	if session == nil {
		return nil, err
	}
	return viewerFixtureTraceSession{session: session, trace: d.trace}, err
}

type viewerFixtureTraceSession struct {
	session nativeviewer.Session
	trace   *viewerFixtureTrace
}

func (s viewerFixtureTraceSession) Advance(ctx context.Context) (nativeviewer.Status, error) {
	status, err := s.session.Advance(ctx)
	if traceErr := s.trace.record("advance", string(status), err); traceErr != nil {
		return status, traceErr
	}
	return status, err
}
func (s viewerFixtureTraceSession) Close() error {
	err := s.session.Close()
	status := ""
	if err == nil {
		status = "closed"
	}
	if traceErr := s.trace.record("close", status, err); traceErr != nil {
		return traceErr
	}
	return err
}
func TestNativeViewerFixtureTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver-events.jsonl")
	trace, err := newViewerFixtureTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	safeError := errors.New("native viewer: native surface refused (viewer_save_control_changed)")
	session := &viewerTestSession{advance: func(context.Context) (nativeviewer.Status, error) { return "", safeError }}
	driver := viewerFixtureTraceDriver{driver: viewerTestDriver{prepare: func(context.Context, nativeviewer.Request) (nativeviewer.Session, error) { return session, nil }}, trace: trace}
	wrapped, err := driver.Prepare(context.Background(), nativeviewer.Request{URL: "https://example.invalid/private?secret=transient", Filename: "private-file.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Advance(context.Background()); !errors.Is(err, safeError) || err.Error() != safeError.Error() {
		t.Fatal(err)
	}
	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "https:") {
		t.Fatal("trace leaked request data")
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("events=%s", raw)
	}
	for i, method := range []string{"prepare", "advance", "close"} {
		var event viewerFixtureDriverEvent
		if err := json.Unmarshal(lines[i], &event); err != nil {
			t.Fatal(err)
		}
		if event.Method != method {
			t.Fatal(event)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			t.Fatal(err)
		}
		if method == "advance" && event.Error != safeError.Error() {
			t.Fatal("safe diagnostic lost", event)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("trace is not private", info, err)
	}
}
