// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"papio/internal/work"
)

// The pipeline's own contract is ORDER, not stage behaviour: every stage is
// tested directly in the sibling files. Filing the wrong PDF under a right
// citation is this project's worst outcome, and the pipeline-level ways that
// happens are all ordering faults — a stage that ran after a gate should have
// stopped it, an error a later stage overwrote, or a verdict produced by the
// identity matcher when the evidence said "review".
//
// So these tests assert what did NOT run as well as what came back. Every fake
// external tool records its own invocation in a marker file, which makes "text
// extraction never happened" an observation rather than an inference from an
// empty report field: a stage can also leave a zero report by succeeding on an
// empty document.

// pipelineTarget is the work each artifact below claims to be.
var pipelineTarget = work.Work{
	DOI:     "10.1234/abc.9",
	Title:   "Deterministic Validation of Scholarly Article Identity",
	Authors: []string{"Ada Lovelace"},
	Year:    2026,
}

// pipelinePacket is a minimal publisher XMP packet naming pipelineTarget's DOI
// in a field whose defined meaning is self-attribution, so a report that
// carries it proves the metadata stage ran and parsed (metadata.go).
const pipelinePacket = `<?xpacket begin='' id='W5M0MpCehiHzreSzNTczkc9d'?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about="" xmlns:prism="http://prismstandard.org/namespaces/basic/2.1/">
   <prism:doi>10.1234/abc.9</prism:doi>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end='w'?>`

// markerTool builds a fake external tool that records its invocation before
// behaving as body says.
func markerTool(t *testing.T, body string) (binary, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "invoked")
	return fakeTool(t, "printf ran > '"+marker+"'\n"+body), marker
}

// toolRan reports whether the tool owning marker was invoked at all.
func toolRan(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", marker, err)
	}
	return err == nil
}

// structuralTool fakes the re-exec'd worker, answering with the supplied report
// JSON after draining the request the parent writes to stdin.
func structuralTool(t *testing.T, report string) (binary, marker string) {
	t.Helper()
	return markerTool(t, "cat >/dev/null; printf '%s\\n' '"+report+"'")
}

// textTool fakes pdftotext by printing text verbatim.
func textTool(t *testing.T, text string) (binary, marker string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "text.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return markerTool(t, "cat '"+path+"'")
}

// pipelineInfoTool fakes the one binary the pipeline drives in two unrelated
// roles: `pdfinfo path` for the structural page-count cross-check, and
// `pdfinfo -meta path` for the XMP packet. Each role gets its own marker, so a
// test can tell which of the two the pipeline actually asked for.
func pipelineInfoTool(t *testing.T, pages int, packet, metaBody string) (binary, crossMarker, metaMarker string) {
	t.Helper()
	dir := t.TempDir()
	crossMarker = filepath.Join(dir, "cross-check")
	metaMarker = filepath.Join(dir, "meta")
	packetPath := filepath.Join(dir, "packet.xmp")
	if err := os.WriteFile(packetPath, []byte(packet), 0o600); err != nil {
		t.Fatal(err)
	}
	if metaBody == "" {
		metaBody = "cat '" + packetPath + "'"
	}
	binary = fakeTool(t, "if [ \"$1\" = -meta ]; then printf ran > '"+metaMarker+"'\n"+metaBody+
		"\nelse printf ran > '"+crossMarker+"'; printf 'Pages: "+strconv.Itoa(pages)+"\\n'; fi")
	return binary, crossMarker, metaMarker
}

// pipelineDocument renders a document whose page one is front and whose body
// carries enough distinct text to clear MinChars, so the semantic stage
// succeeds without OCR and the identity rules still read exactly front.
func pipelineDocument(front string) string {
	var b strings.Builder
	b.WriteString(front)
	b.WriteString("\f")
	for i := range 60 {
		fmt.Fprintf(&b, "Body paragraph %02d develops measurement procedure %02d at length.\n", i, i)
	}
	return b.String()
}

// pipelineOptions bounds every stage tightly: the fakes answer instantly, so a
// generous default timeout only delays a hung-tool failure.
func pipelineOptions(threshold float64) ValidationOptions {
	return ValidationOptions{
		Structural:          StructuralOptions{Timeout: 20 * time.Second, MaxPages: 10},
		Semantic:            SemanticOptions{Timeout: 20 * time.Second},
		TitleMatchThreshold: threshold,
	}
}

func TestValidatePipelinePayloadGateRunsBeforeEveryTool(t *testing.T) {
	worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
	text, textRan := textTool(t, pipelineDocument("Deterministic validation of scholarly article identity.\n"))
	info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "")

	short := filepath.Join(t.TempDir(), "truncated.pdf")
	if err := os.WriteFile(short, []byte("%PDF-1.7\nnot enough bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := ValidationInput{
		Path:         short,
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}
	report, err := Validate(context.Background(), in, pipelineOptions(0.6))
	if err != nil {
		t.Fatalf("a rejected payload is a verdict, not an error: %v", err)
	}
	if report.Payload.OK || report.Payload.Reason == "" {
		t.Fatalf("payload = %+v, want a reasoned rejection", report.Payload)
	}
	for name, marker := range map[string]string{"worker": workerRan, "pdftotext": textRan, "pdfinfo": crossRan, "pdfinfo -meta": metaRan} {
		if toolRan(t, marker) {
			t.Fatalf("%s ran on bytes the payload gate rejected", name)
		}
	}
	if report.Structural != (StructuralReport{}) || report.Text.Chars != 0 || report.Identity.Result != "" {
		t.Fatalf("report carries stages that never ran: %+v", report)
	}
}

func TestValidatePipelineUnreadableFileErrors(t *testing.T) {
	worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
	in := ValidationInput{
		Path:         filepath.Join(t.TempDir(), "absent.pdf"),
		WorkerBinary: worker,
		Target:       pipelineTarget,
	}
	report, err := Validate(context.Background(), in, pipelineOptions(0.6))
	if err == nil {
		t.Fatalf("a file the pipeline cannot read must be an error, got report %+v", report)
	}
	if toolRan(t, workerRan) {
		t.Fatal("the worker was handed a path the parent could not even stat")
	}
}

// A structural rejection is the quarantine boundary: nothing downstream may
// touch a file pdfcpu refused. Proving that needs the absence of the text
// tool's marker, because an unparsed PDF also yields an empty TextReport.
func TestValidatePipelineInvalidStructuralReportShortCircuits(t *testing.T) {
	worker, workerRan := structuralTool(t, `{"Valid":false,"Encrypted":true,"Reason":"encrypted PDF"}`)
	text, textRan := textTool(t, pipelineDocument("Deterministic validation of scholarly article identity.\n"))
	info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "")

	in := ValidationInput{
		Path:         writeTempPDF(t),
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}
	report, err := Validate(context.Background(), in, pipelineOptions(0.6))
	if err != nil {
		t.Fatalf("a structural rejection is a verdict, not an error: %v", err)
	}
	if !toolRan(t, workerRan) {
		t.Fatal("the structural worker never ran")
	}
	if !report.Payload.OK || report.Structural.Valid || !report.Structural.Encrypted {
		t.Fatalf("report = %+v, want a payload-valid, structurally rejected artifact", report)
	}
	if toolRan(t, textRan) {
		t.Fatal("text was extracted from a structurally rejected PDF")
	}
	if toolRan(t, metaRan) {
		t.Fatal("metadata was read from a structurally rejected PDF")
	}
	if toolRan(t, crossRan) {
		t.Fatal("the page-count cross-check ran against a report that was already invalid")
	}
	if report.Text.Chars != 0 || report.Text.NeedsReview || report.Metadata != nil || report.Identity.Result != "" {
		t.Fatalf("report carries stages that never ran: %+v", report)
	}
}

// A worker that cannot be run at all is not a rejection: nothing examined the
// file, so there is no verdict to record. The distinction matters because a
// rejection quarantines an artifact while an error must leave it pending.
func TestValidatePipelineStructuralErrorAborts(t *testing.T) {
	worker, workerRan := markerTool(t, `cat >/dev/null; printf 'worker: cannot open display' >&2; exit 3`)
	text, textRan := textTool(t, pipelineDocument("Deterministic validation of scholarly article identity.\n"))
	info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "")

	report, err := Validate(context.Background(), ValidationInput{
		Path:         writeTempPDF(t),
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}, pipelineOptions(0.6))
	if err == nil {
		t.Fatalf("a worker that failed to report must surface as an error, got %+v", report)
	}
	if !strings.Contains(err.Error(), "pdf worker failed") {
		t.Fatalf("err = %v, want the worker failure named", err)
	}
	if !toolRan(t, workerRan) {
		t.Fatal("the structural worker never ran")
	}
	if toolRan(t, textRan) || toolRan(t, metaRan) {
		t.Fatal("a semantic stage ran after the structural stage errored")
	}
	if toolRan(t, crossRan) {
		t.Fatal("the page-count cross-check ran against a worker that never reported")
	}
	if !report.Payload.OK {
		t.Fatalf("payload = %+v, want the gate that did pass preserved", report.Payload)
	}
	if report.Structural != (StructuralReport{}) || report.Identity.Result != "" {
		t.Fatalf("report = %+v, want no structural verdict and no identity decision", report)
	}
}

// ExtractText is total by design: a missing, failing or timed-out converter is
// an explicit review signal, never a pipeline error (semantic.go). The pipeline
// must therefore reach a REVIEW verdict, not abort — aborting would strand the
// artifact with no decision a human could act on.
func TestValidatePipelineTextToolFailureBecomesReview(t *testing.T) {
	cases := map[string]struct {
		body    string
		present bool
		// timeout must stay generous for the cases whose point is the
		// converter's OWN failure: a deadline short enough to kill a shell
		// before it starts turns every case into the timeout case, which is
		// exactly what the marker assertion below detects.
		timeout time.Duration
		// wantEvidence is the substring naming which failure produced the
		// review, so a case cannot pass on another case's reason.
		wantEvidence string
	}{
		"converter absent": {
			timeout:      20 * time.Second,
			wantEvidence: "capability: pdftotext unavailable",
		},
		"converter exits 1": {
			body:         `printf 'pdftotext: boom' >&2; exit 1`,
			present:      true,
			timeout:      20 * time.Second,
			wantEvidence: "pdftotext failed: pdftotext: boom",
		},
		"converter times out": {
			body:         `sleep 30`,
			present:      true,
			timeout:      100 * time.Millisecond,
			wantEvidence: "deadline",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
			capability := Capability{}
			converterRan := ""
			if tc.present {
				binary, marker := markerTool(t, tc.body)
				capability.PDFToText, converterRan = binary, marker
			}
			opt := pipelineOptions(0.6)
			opt.Semantic.Timeout = tc.timeout
			report, err := Validate(context.Background(), ValidationInput{
				Path:         writeTempPDF(t),
				DeclaredMIME: "application/pdf",
				WorkerBinary: worker,
				Capability:   capability,
				Target:       pipelineTarget,
			}, opt)
			if err != nil {
				t.Fatalf("a failed converter must not abort the pipeline: %v", err)
			}
			if !toolRan(t, workerRan) {
				t.Fatal("the review verdict was reached without the structural stage running")
			}
			// The timed-out converter is killed on a deadline it may not
			// outlive long enough to record anything, so only the cases that
			// claim the converter's own exit status assert its marker.
			if converterRan != "" && tc.timeout > time.Second && !toolRan(t, converterRan) {
				t.Fatal("the converter was never invoked, so its failure is not what produced the review")
			}
			if !report.Structural.Valid || !report.Text.NeedsReview {
				t.Fatalf("report = %+v, want a valid structure needing review", report)
			}
			if !strings.Contains(strings.Join(report.Text.Evidence, " "), tc.wantEvidence) {
				t.Fatalf("evidence = %v, want the review attributed to %q", report.Text.Evidence, tc.wantEvidence)
			}
			if report.Identity.Result != IdentityReview {
				t.Fatalf("identity = %+v, want %q", report.Identity, IdentityReview)
			}
		})
	}
}

// ExtractMetadata returns a hard error only when the context is done, and the
// pipeline must not paper over it: a cancelled run has no verdict, and turning
// one into a report would let a caller act on stages that never completed. The
// cancellation is triggered by the metadata tool itself, so this asserts
// ordering rather than racing a wall-clock deadline.
func TestValidatePipelineMetadataContextErrorPropagates(t *testing.T) {
	worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
	text, textRan := textTool(t, pipelineDocument("Deterministic validation of scholarly article identity.\n"))
	info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "sleep 30")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for range 1000 {
			if _, err := os.Stat(metaRan); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	report, err := Validate(ctx, ValidationInput{
		Path:         writeTempPDF(t),
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}, pipelineOptions(0.6))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancelled metadata read to surface", err)
	}
	if !toolRan(t, workerRan) || !toolRan(t, crossRan) || !toolRan(t, textRan) || !toolRan(t, metaRan) {
		t.Fatalf("cancellation did not happen at the metadata stage: worker=%v cross=%v text=%v meta=%v",
			toolRan(t, workerRan), toolRan(t, crossRan), toolRan(t, textRan), toolRan(t, metaRan))
	}
	if !report.Structural.Valid || report.Text.Chars == 0 {
		t.Fatalf("report = %+v, want the stages before metadata preserved", report)
	}
	if report.Metadata != nil || report.Identity.Result != "" {
		t.Fatalf("report = %+v, want no metadata and no verdict after a cancelled run", report)
	}
}

// A review signal from text extraction must produce the REVIEW verdict from the
// text's own evidence, never the matcher's verdict. The claim here is about the
// VERDICT, not about the call: the matcher is a pure function, so a pipeline
// that consulted it and then returned review anyway is observationally
// identical and no test can separate the two without an invocation seam that
// production has no other reason to carry. What this pins is that the reported
// verdict is the text stage's review, and that it is NOT what the matcher says
// about the same excerpt.
func TestValidatePipelineReviewVerdictComesFromTextNotTheMatcher(t *testing.T) {
	// Sparse text (below MinChars) with no OCR tools is the review path, and
	// unlike a failed converter it still leaves a non-empty excerpt for the
	// matcher, which is what makes the two verdicts distinguishable.
	excerpt := "Unrelated preprint\ndoi:10.9999/nope\n"
	matcher := MatchIdentityWithThreshold(excerpt, pipelineTarget, 0.6)
	if matcher.Result == IdentityReview {
		t.Fatalf("fixture no longer discriminates: the matcher itself returns review (%+v)", matcher)
	}

	worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
	text, textRan := textTool(t, excerpt)
	info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "")

	report, err := Validate(context.Background(), ValidationInput{
		Path:         writeTempPDF(t),
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}, pipelineOptions(0.6))
	if err != nil {
		t.Fatal(err)
	}
	if !toolRan(t, workerRan) || !toolRan(t, crossRan) || !toolRan(t, textRan) || !toolRan(t, metaRan) {
		t.Fatalf("the review path skipped a stage that runs before the identity decision: worker=%v cross=%v text=%v meta=%v",
			toolRan(t, workerRan), toolRan(t, crossRan), toolRan(t, textRan), toolRan(t, metaRan))
	}
	if !report.Text.NeedsReview || report.Text.Excerpt != excerpt {
		t.Fatalf("text = %+v, want the sparse excerpt flagged for review", report.Text)
	}
	if report.Identity.Result != IdentityReview {
		t.Fatalf("identity = %+v, want %q, not the matcher's %q", report.Identity, IdentityReview, matcher.Result)
	}
	if !reflect.DeepEqual(report.Identity.Evidence, report.Text.Evidence) {
		t.Fatalf("review evidence = %v, want the text stage's evidence %v", report.Identity.Evidence, report.Text.Evidence)
	}
	if reflect.DeepEqual(report.Identity, matcher) {
		t.Fatalf("verdict is indistinguishable from the matcher's: %+v", report.Identity)
	}
	// Metadata is read for every structurally valid PDF, before the identity
	// decision, so a reviewed artifact still carries it (pipeline.go).
	if report.Metadata.NamesWork(pipelineTarget) == "" {
		t.Fatalf("metadata = %+v, want the packet's self-attributed DOI", report.Metadata)
	}
}

// The happy path's contract is that the reported verdict IS the matcher's, on
// the excerpt the semantic stage produced, at the configured threshold.
func TestValidatePipelineHappyPathReturnsTheMatcherVerdict(t *testing.T) {
	document := pipelineDocument("Deterministic validation of scholarly article identity.\nAda Lovelace\n2026\ndoi:10.1234/abc.9\n")
	worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":2}`)
	text, textRan := textTool(t, document)
	info, crossRan, metaRan := pipelineInfoTool(t, 2, pipelinePacket, "")

	opt := pipelineOptions(0.6)
	// PDFInfoPath is deliberately left empty: the pipeline fills it from the
	// detected capability, which is what enables the independent page-count
	// cross-check at all.
	report, err := Validate(context.Background(), ValidationInput{
		Path:         writeTempPDF(t),
		DeclaredMIME: "application/pdf",
		WorkerBinary: worker,
		Capability:   Capability{PDFToText: text, PDFInfo: info},
		Target:       pipelineTarget,
	}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !toolRan(t, workerRan) || !toolRan(t, textRan) || !toolRan(t, metaRan) {
		t.Fatal("a stage of the happy path never ran")
	}
	if !toolRan(t, crossRan) {
		t.Fatal("Capability.PDFInfo did not reach StructuralOptions.PDFInfoPath, so nothing cross-checked the page count")
	}
	if !report.Payload.OK || !report.Structural.Valid || report.Structural.Pages != 2 {
		t.Fatalf("report = %+v, want a payload-valid two-page PDF", report)
	}
	if report.Text.NeedsReview || report.Text.OCRUsed || report.Text.Excerpt != document {
		t.Fatalf("text = %+v, want the extracted document without review", report.Text)
	}
	if report.Metadata.NamesWork(pipelineTarget) == "" {
		t.Fatalf("metadata = %+v, want the packet's self-attributed DOI", report.Metadata)
	}
	want := MatchIdentityWithThreshold(report.Text.Excerpt, pipelineTarget, opt.TitleMatchThreshold)
	if want.Result != IdentityPass {
		t.Fatalf("fixture no longer describes the requested work: %+v", want)
	}
	if !reflect.DeepEqual(report.Identity, want) {
		t.Fatalf("identity = %+v, want the matcher's verdict %+v", report.Identity, want)
	}
}

// TitleMatchThreshold is the caller's reject floor. If the pipeline dropped it
// the matcher would silently apply its 60% default, which is exactly the
// unordered-membership leniency the corpus punished, so the plumbing is
// asserted on a document where the two thresholds disagree.
func TestValidatePipelineThresholdReachesTheMatcher(t *testing.T) {
	// Four of the five significant title tokens are printed and the target's
	// DOI appears nowhere, so the verdict rests on the title ratio alone.
	document := pipelineDocument("Deterministic validation of scholarly identity\nAda Lovelace\n2026\n")
	lenient := MatchIdentityWithThreshold(document, pipelineTarget, 0.6)
	strict := MatchIdentityWithThreshold(document, pipelineTarget, 0.99)
	if reflect.DeepEqual(lenient, strict) {
		t.Fatalf("fixture no longer discriminates between thresholds: %+v", lenient)
	}

	for _, tc := range []struct {
		name      string
		threshold float64
		want      IdentityDecision
	}{
		{"lenient floor", 0.6, lenient},
		{"strict floor", 0.99, strict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
			text, textRan := textTool(t, document)
			info, crossRan, metaRan := pipelineInfoTool(t, 1, pipelinePacket, "")
			report, err := Validate(context.Background(), ValidationInput{
				Path:         writeTempPDF(t),
				DeclaredMIME: "application/pdf",
				WorkerBinary: worker,
				Capability:   Capability{PDFToText: text, PDFInfo: info},
				Target:       pipelineTarget,
			}, pipelineOptions(tc.threshold))
			if err != nil {
				t.Fatal(err)
			}
			if !toolRan(t, workerRan) || !toolRan(t, crossRan) || !toolRan(t, textRan) || !toolRan(t, metaRan) {
				t.Fatalf("a stage before the identity decision never ran: worker=%v cross=%v text=%v meta=%v",
					toolRan(t, workerRan), toolRan(t, crossRan), toolRan(t, textRan), toolRan(t, metaRan))
			}
			if report.Text.NeedsReview {
				t.Fatalf("text = %+v, want extraction to succeed so the threshold decides", report.Text)
			}
			if !reflect.DeepEqual(report.Identity, tc.want) {
				t.Fatalf("identity at threshold %v = %+v, want %+v", tc.threshold, report.Identity, tc.want)
			}
		})
	}
}

// ValidateFile refuses to run without a worker binary rather than reaching the
// structural stage and failing there; with one it must enter the same pipeline,
// payload gate first.
func TestValidatePipelineValidateFileRequiresAWorker(t *testing.T) {
	truncated := filepath.Join(t.TempDir(), "truncated.pdf")
	if err := os.WriteFile(truncated, []byte("%PDF-1.7\nnot enough bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no worker", func(t *testing.T) {
		report, err := ValidateFile(context.Background(), ValidationInput{
			Path:         truncated,
			DeclaredMIME: "application/pdf",
			Target:       pipelineTarget,
		}, pipelineOptions(0.6))
		if err == nil || !strings.Contains(err.Error(), "worker binary is required") {
			t.Fatalf("err = %v, want a refusal naming the missing worker", err)
		}
		if !reflect.DeepEqual(report, ValidationReport{}) {
			t.Fatalf("report = %+v, want nothing decided", report)
		}
	})

	t.Run("worker present reaches the payload gate", func(t *testing.T) {
		worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
		report, err := ValidateFile(context.Background(), ValidationInput{
			Path:         truncated,
			DeclaredMIME: "application/pdf",
			WorkerBinary: worker,
			Target:       pipelineTarget,
		}, pipelineOptions(0.6))
		if err != nil {
			t.Fatalf("a rejected payload is a verdict, not an error: %v", err)
		}
		if report.Payload.OK || report.Payload.Reason == "" || report.Payload.SizeBytes == 0 {
			t.Fatalf("payload = %+v, want the gate's reasoned rejection", report.Payload)
		}
		if toolRan(t, workerRan) {
			t.Fatal("ValidateFile parsed bytes the payload gate rejects")
		}
	})

	t.Run("worker present reaches the full pipeline", func(t *testing.T) {
		document := pipelineDocument("Deterministic validation of scholarly article identity.\nAda Lovelace\n2026\ndoi:10.1234/abc.9\n")
		worker, workerRan := structuralTool(t, `{"Valid":true,"Pages":1}`)
		text, textRan := textTool(t, document)
		report, err := ValidateFile(context.Background(), ValidationInput{
			Path:         writeTempPDF(t),
			DeclaredMIME: "application/pdf",
			WorkerBinary: worker,
			Capability:   Capability{PDFToText: text},
			Target:       pipelineTarget,
		}, pipelineOptions(0.6))
		if err != nil {
			t.Fatal(err)
		}
		if !toolRan(t, workerRan) || !toolRan(t, textRan) {
			t.Fatal("ValidateFile did not drive the pipeline's stages")
		}
		if report.Identity.Result != IdentityPass {
			t.Fatalf("identity = %+v, want %q", report.Identity, IdentityPass)
		}
	})
}
