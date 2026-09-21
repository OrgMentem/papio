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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

func writeAdoptionProbeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func adoptionTestSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		// ERROR_PRIVILEGE_NOT_HELD: ordinary Windows accounts need Developer
		// Mode or a granted symlink privilege. Other failures remain failures.
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skipf("Windows symlink privilege is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func adoptionTestFileInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// File.Stat captures Windows file identity from the open handle. os.Stat
	// can instead defer that lookup until SameFile, after a name was replaced.
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil {
		t.Fatalf("capture file identity: stat=%v close=%v", statErr, closeErr)
	}
	return info
}

// A real, one-page PDF with an uncompressed text stream, large enough for the
// production payload gate. No personal library or external tool is needed.
func adoptionProbePDF(doi string) []byte {
	text := "BT /F1 12 Tf 50 700 Td (An Institutional Paper DOI: " + doi + ") Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(text), text),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offsets []int
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	b.WriteString("%" + strings.Repeat("padding ", 700) + "\n")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// The incident oracle uses real payload and structural validation and the real
// identity matcher, instead of newBridge's always-successful validator. Only
// these trusted synthetic fixtures are parsed in-process. Text extraction here
// reads their plain stream; production still uses its isolated worker/tools.
func adoptionProbeValidate(_ context.Context, path, mime string, target work.Work) (pdf.ValidationReport, error) {
	var report pdf.ValidationReport
	var err error
	report.Payload, err = pdf.ValidatePayloadFile(path, mime)
	if err != nil || !report.Payload.OK {
		return report, err
	}
	request, err := json.Marshal(map[string]any{"path": path, "max_pages": 10})
	if err != nil {
		return report, err
	}
	var out bytes.Buffer
	if err := pdf.RunStructuralWorker(bytes.NewReader(request), &out); err != nil {
		return report, err
	}
	if err := json.Unmarshal(out.Bytes(), &report.Structural); err != nil {
		return report, err
	}
	if !report.Structural.Valid {
		return report, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	start := bytes.Index(body, []byte("stream\n"))
	end := bytes.Index(body, []byte("endstream"))
	if start < 0 || end <= start {
		return report, errors.New("fixture has no plain text stream")
	}
	excerpt := string(body[start+len("stream\n") : end])
	report.Text = pdf.TextReport{Chars: int64(len(excerpt)), Excerpt: excerpt}
	report.Identity = pdf.MatchIdentity(excerpt, target)
	return report, nil
}

func TestAdoptionProbeHTMLPreservesHandoffThenAdoptsPDF(t *testing.T) {
	for _, mode := range []string{"sweep", "poll", "sweep after restart"} {
		t.Run(mode, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_probe_html", handoffWork())
			before, err := jobs.ListHumanActions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			b.svc.Validate = func(ctx context.Context, path, mime string, target work.Work) (pdf.ValidationReport, error) {
				calls++
				return adoptionProbeValidate(ctx, path, mime, target)
			}
			scan := func() {
				t.Helper()
				if mode == "poll" {
					runSync(t, b, hello())
					return
				}
				if err := b.SweepAdoptions(ctx); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			html := []byte("<!doctype html><html>sign in</html>" + strings.Repeat(" ", 6000))
			writeAdoptionProbeFile(t, path, html)
			scan()
			after, err := jobs.ListHumanActions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("HTML consumed/replaced the original handoff: before=%+v after=%+v", before, after)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" || calls != 0 {
				t.Fatalf("HTML reached adoption: row=%+v validator calls=%d", row, calls)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				detail, _ := event["detail"].(map[string]any)
				if detail["reason"] == "adopt_browser_download" {
					t.Fatal("HTML started validation")
				}
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, html) {
				t.Fatalf("probe modified landing bytes: %v", err)
			}
			if _, ok, err := jobs.ArtifactWinner(ctx, id, 1); err != nil || ok {
				t.Fatalf("HTML won attempt: %v %v", ok, err)
			}

			// Neither a live bridge nor a replacement bridge may remember a
			// rejection that prevents reuse of the browser-selected pathname.
			if mode == "sweep after restart" {
				b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
			}
			body := adoptionProbePDF(handoffWork().DOI)
			writeAdoptionProbeFile(t, path, body)
			report, err := adoptionProbeValidate(ctx, path, "application/pdf", handoffWork())
			if err != nil || !report.Structural.Valid || report.Identity.Result != pdf.IdentityPass {
				t.Fatalf("fixture validation: %+v %v", report, err)
			}
			scan()
			row, err = jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			if row.State != job.StateReady || row.ArtifactSHA256 != hex.EncodeToString(digest[:]) || calls != 1 {
				t.Fatalf("later PDF not validated/adopted: row=%+v validator calls=%d", row, calls)
			}
		})
	}
}

func TestAdoptionProbeDoesNotReplaceFullValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"truncated PDF", []byte("%PDF-1.4\n")},
		{"malformed PDF", []byte("%PDF-1.4\n" + strings.Repeat("not a PDF\n", 700))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			b.svc.Validate = adoptionProbeValidate
			id := park(t, jobs, "wr_probe_invalid", handoffWork())
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			writeAdoptionProbeFile(t, path, tc.body)
			report, err := adoptionProbeValidate(context.Background(), path, "application/pdf", handoffWork())
			if err != nil {
				t.Fatalf("invalid fixture validator error: %+v %v", report, err)
			}
			if !probeAdoptionPDF(filepath.Dir(path), filepath.Base(path)) {
				t.Fatal("header probe unexpectedly became a full validator")
			}
			if err := b.SweepAdoptions(context.Background()); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" {
				t.Fatalf("invalid PDF accepted: %+v", row)
			}
			actions, err := jobs.ListHumanActions(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 1 || actions[0].Kind != "manual_download" {
				t.Fatalf("full validation was bypassed: %+v", actions)
			}
		})
	}
}

func TestAdoptionProbeWrongWorkStillNeedsReview(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = adoptionProbeValidate
	id := park(t, jobs, "wr_probe_wrong_work", handoffWork())
	writeAdoptionProbeFile(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"), adoptionProbePDF("10.1234/different.paper"))
	if err := b.SweepAdoptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateNeedsReview || row.ArtifactSHA256 != "" {
		t.Fatalf("wrong work escaped identity review: %+v %v", row, err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != "verify_identity" {
		t.Fatalf("wrong work did not receive identity review: %+v", actions)
	}
}

func TestAdoptionProbeHeaderAndConfinement(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"pdf", "%PDF-1.4\n", true},
		{"html", "<!doctype html><html>no PDF</html>", false},
		{"empty", "", false}, {"short", "%PDF", false},
		{"offset header", " \n%PDF-1.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeAdoptionProbeFile(t, filepath.Join(dir, "paper.pdf"), []byte(tc.body))
			if got := probeAdoptionPDF(dir, "paper.pdf"); got != tc.want {
				t.Fatalf("probe=%v want %v", got, tc.want)
			}
		})
	}
	writeAdoptionProbeFile(t, filepath.Join(dir, "actual"), []byte("%PDF-1.4\n"))
	for _, name := range []string{"../actual", "missing.pdf", ".", filepath.Join(dir, "actual")} {
		if probeAdoptionPDF(dir, name) {
			t.Fatalf("accepted non-confined/nonregular file %q", name)
		}
	}
	t.Run("symlink", func(t *testing.T) {
		adoptionTestSymlink(t, "actual", filepath.Join(dir, "link.pdf"))
		if probeAdoptionPDF(dir, "link.pdf") {
			t.Fatal("accepted symlink")
		}
	})
}

func TestAdoptionProbeDefersFilesChangingDuringRead(t *testing.T) {
	for _, mode := range []string{"grow", "rewrite", "replace", "replace after unlink", "symlink", "remove", "read error", "short read"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "paper.pdf")
			body := []byte("%PDF-1.4\nfixture")
			writeAdoptionProbeFile(t, path, body)
			info := adoptionTestFileInfo(t, path)
			var replacementBlocked bool
			read := func(r io.Reader, p []byte) (int, error) {
				n, err := io.ReadFull(r, p)
				switch mode {
				case "grow":
					writeAdoptionProbeFile(t, path, append(body, 'x'))
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "rewrite":
					writeAdoptionProbeFile(t, path, bytes.ReplaceAll(body, []byte("fixture"), []byte("changed")))
					if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
				case "replace", "replace after unlink", "symlink":
					replacement := filepath.Join(dir, "replacement")
					writeAdoptionProbeFile(t, replacement, body)
					if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					if mode == "replace" {
						if err := os.Rename(replacement, path); err != nil {
							// Windows may refuse replacing an open destination.
							// Prove OS refusal left the source intact; this is not
							// evidence of the software detecting replacement.
							if runtime.GOOS != "windows" ||
								(!errors.Is(err, syscall.Errno(5)) && !errors.Is(err, syscall.Errno(32))) {
								t.Fatal(err)
							}
							replacementBlocked = true
							current := adoptionTestFileInfo(t, path)
							got, readErr := os.ReadFile(path)
							if readErr != nil || !os.SameFile(info, current) || !bytes.Equal(got, body) {
								t.Fatalf("blocked replacement altered original: %v", readErr)
							}
							t.Logf("Windows refused replacement of the open file: %v", err)
						}
					} else {
						// Root.Remove uses Windows' POSIX unlink semantics where
						// available, freeing the name while the old descriptor
						// stays open. DeleteFile alone can leave it delete-pending.
						root, err := os.OpenRoot(dir)
						if err != nil {
							t.Fatal(err)
						}
						removeErr := root.Remove(filepath.Base(path))
						_ = root.Close()
						if removeErr != nil {
							t.Fatal(removeErr)
						}
						if _, err := os.Lstat(path); !os.IsNotExist(err) {
							t.Fatalf("replacement fixture did not free source name: %v", err)
						}
						if mode == "symlink" {
							adoptionTestSymlink(t, "replacement", path)
						} else if err := os.Rename(replacement, path); err != nil {
							t.Fatal(err)
						}
					}
					if mode != "symlink" && !replacementBlocked {
						if os.SameFile(info, adoptionTestFileInfo(t, path)) {
							t.Fatal("replacement fixture did not change file identity")
						}
					}
				case "remove":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "read error":
					return n, os.ErrPermission
				case "short read":
					return 4, nil
				}
				return n, err
			}
			if got := probeAdoptionPDFWithRead(dir, "paper.pdf", read); got != replacementBlocked {
				t.Fatalf("probe=%v, Windows blocked replacement=%v", got, replacementBlocked)
			}
			if replacementBlocked {
				// The same replacement must succeed after the probe closes its
				// descriptor; otherwise this could be an unrelated ACL failure.
				if err := os.Rename(filepath.Join(dir, "replacement"), path); err != nil {
					t.Fatalf("replacement still failed after the probe closed: %v", err)
				}
				if os.SameFile(info, adoptionTestFileInfo(t, path)) {
					t.Fatal("closed-file replacement did not change identity")
				}
			}
			if mode == "symlink" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			writeAdoptionProbeFile(t, path, body)
			if !probeAdoptionPDF(dir, "paper.pdf") {
				t.Fatal("later stable file stayed rejected")
			}
		})
	}
}

func TestAdoptionProbePreservesAmbiguityAndGrabSelection(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "job-probe")
	writeAdoptionProbeFile(t, filepath.Join(dir, "wrapper.pdf"), []byte("<html>no PDF</html>"))
	if name, ok := b.settledFileIn(dir); !ok || name != "wrapper.pdf" {
		t.Fatal("grab settled-file contract changed")
	}
	if _, ok := b.scanAdoptionDir(context.Background(), "job-probe"); ok {
		t.Fatal("job scan accepted HTML")
	}
	writeAdoptionProbeFile(t, filepath.Join(dir, "paper.pdf"), adoptionProbePDF(handoffWork().DOI))
	if _, ok := b.scanAdoptionDir(context.Background(), "job-probe"); ok {
		t.Fatal("HTML was filtered to make an ambiguous directory adoptable")
	}
	if err := os.Remove(filepath.Join(dir, "wrapper.pdf")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".part", ".crdownload", ".download"} {
		path := filepath.Join(dir, "writing"+suffix)
		writeAdoptionProbeFile(t, path, []byte("partial"))
		if _, ok := b.scanAdoptionDir(context.Background(), "job-probe"); ok {
			t.Fatalf("ignored %s", suffix)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if name, ok := b.scanAdoptionDir(context.Background(), "job-probe"); !ok || name != "paper.pdf" {
		t.Fatal("settled PDF did not recover")
	}
}

func TestAdoptionProbeDoesNotFallThroughToShadowedLegacyPDF(t *testing.T) {
	b, jobs, cfg := legacyRootBridge(t)
	b.svc.Validate = adoptionProbeValidate
	id := park(t, jobs, "wr_probe_legacy", handoffWork())
	effective := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
	writeAdoptionProbeFile(t, effective, []byte("<html>no PDF</html>"))
	writeAdoptionProbeFile(t, filepath.Join(cfg.LegacyAdoptionRoot(), id, "paper.pdf"), adoptionProbePDF(handoffWork().DOI))
	if err := b.SweepAdoptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != handoffActionKind {
		t.Fatalf("legacy probe ingested shadowing HTML: %+v", actions)
	}
	if err := os.Remove(effective); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(effective)); err != nil {
		t.Fatal(err)
	}
	if err := b.SweepAdoptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateReady {
		t.Fatalf("unshadowed legacy PDF not adopted: %+v %v", row, err)
	}
}

func TestAdoptionProbeBlockedReadIsBoundedAndLatched(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	compressAdoptionScanDeadline(t)
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "job-probe")
	writeAdoptionProbeFile(t, filepath.Join(dir, "paper.pdf"), []byte("%PDF-1.4\n"))
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	probe := func(dir, name string) bool {
		calls.Add(1)
		defer close(finished)
		return probeAdoptionPDFWithRead(dir, name, func(r io.Reader, p []byte) (int, error) {
			<-release
			return io.ReadFull(r, p)
		})
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		<-finished
	})
	start := time.Now()
	if _, ok := b.scanAdoptionDirWithProbe("job-probe", probe); ok {
		t.Fatal("timed-out probe adopted")
	}
	if time.Since(start) > time.Second || !b.adoptionLatchUnhealthy() {
		t.Fatal("blocked read did not bound/latch")
	}
	for range 3 {
		if _, ok := b.scanAdoptionDirWithProbe("job-probe", probe); ok {
			t.Fatal("latched probe adopted")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("stacked %d blocked reads", calls.Load())
	}
	close(release)
	released = true
	<-finished
	deadline := time.Now().Add(time.Second)
	for b.adoptionLatchUnhealthy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.adoptionLatchUnhealthy() {
		t.Fatal("probe never resumed after read returned")
	}
	if _, ok := b.scanAdoptionDir(context.Background(), "job-probe"); !ok {
		t.Fatal("fresh scan did not recover")
	}
}
