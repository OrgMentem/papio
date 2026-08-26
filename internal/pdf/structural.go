// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// WorkerArgument is deliberately not advertised by the CLI. The executable
// which owns dispatch should invoke RunStructuralWorker when it sees it.
const WorkerArgument = "--papio-pdf-worker"

// StructuralOptions bounds the untrusted worker process and optional independent
// pdfinfo cross-check. A zero value is filled from DefaultStructuralOptions.
type StructuralOptions struct {
	Timeout        time.Duration
	MaxPages       int
	MaxOutputBytes int64
	PDFInfoPath    string
}

func DefaultStructuralOptions() StructuralOptions {
	return StructuralOptions{
		Timeout:        30 * time.Second,
		MaxPages:       200,
		MaxOutputBytes: 64 << 10,
	}
}

type workerRequest struct {
	Path     string `json:"path"`
	MaxPages int    `json:"max_pages"`
	// SanitizeTo asks the worker to write a copy of Path without its embedded
	// files, and to report on THAT copy rather than on Path.
	SanitizeTo string `json:"sanitize_to,omitempty"`
}

// ValidateStructural starts the supplied papio executable in worker mode. It
// never opens or parses path in this process: the only parent-side payload is a
// small JSON pathname request.
func ValidateStructural(ctx context.Context, binary, path string, opt StructuralOptions) (StructuralReport, error) {
	return runStructuralWorker(ctx, binary, workerRequest{Path: path}, path, opt)
}

// SanitizeEmbeddedFiles writes a copy of path without its embedded files to
// dest and returns the structural report for THAT copy, never for path. A
// caller may therefore trust the answer without trusting the removal: a report
// that still carries HasEmbeddedFiles means the rewrite did not do the job, and
// the original quarantine decision stands.
//
// Publisher PDFs routinely bundle one supplementary attachment, which is the
// only active-content marker on most quarantined papers. Stripping it removes
// the risk instead of asking a human to accept it (ADR-0022, amendment
// 2026-08-27), at the cost of adopting bytes the publisher did not emit - which
// is why provenance records both digests.
func SanitizeEmbeddedFiles(ctx context.Context, binary, path, dest string, opt StructuralOptions) (StructuralReport, error) {
	if dest == "" {
		return StructuralReport{}, errors.New("sanitized PDF destination is required")
	}
	if dest == path {
		return StructuralReport{}, errors.New("sanitized PDF destination must differ from the source")
	}
	return runStructuralWorker(ctx, binary, workerRequest{Path: path, SanitizeTo: dest}, dest, opt)
}

// runStructuralWorker starts the supplied papio executable in worker mode. It
// never opens or parses a PDF in this process: the only parent-side payload is
// a small JSON pathname request. reportPath names the file the returned report
// describes, which is the rewrite destination for a sanitize request.
func runStructuralWorker(ctx context.Context, binary string, req workerRequest, reportPath string, opt StructuralOptions) (StructuralReport, error) {
	if binary == "" {
		return StructuralReport{}, errors.New("pdf worker binary is required")
	}
	if req.Path == "" {
		return StructuralReport{}, errors.New("PDF path is required")
	}
	opt = normalizedStructuralOptions(opt)
	req.MaxPages = opt.MaxPages
	payload, err := json.Marshal(req)
	if err != nil { // presently impossible, but keeps the contract total.
		return StructuralReport{}, fmt.Errorf("encode pdf worker request: %w", err)
	}
	if len(payload) > 16<<10 {
		return StructuralReport{}, errors.New("pdf worker request exceeds 16KiB")
	}

	workerCtx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(workerCtx, binary, WorkerArgument)
	configureProcessTree(cmd)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout cappedBuffer
	stdout.limit = opt.MaxOutputBytes
	var stderr cappedBuffer
	stderr.limit = 8 << 10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if workerCtx.Err() != nil {
		return StructuralReport{}, fmt.Errorf("pdf worker timed out: %w", workerCtx.Err())
	}
	if stdout.exceeded {
		return StructuralReport{}, fmt.Errorf("pdf worker output exceeds %d bytes", opt.MaxOutputBytes)
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return StructuralReport{}, fmt.Errorf("pdf worker failed: %w: %s", err, detail)
		}
		return StructuralReport{}, fmt.Errorf("pdf worker failed: %w", err)
	}
	var report StructuralReport
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return StructuralReport{}, fmt.Errorf("decode pdf worker response: %w", err)
	}
	// A second JSON value is never part of the one-response protocol.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return StructuralReport{}, errors.New("pdf worker emitted multiple responses")
	}
	if err := validateWorkerReport(&report, opt.MaxPages); err != nil {
		report.Valid = false
		report.Reason = err.Error()
		return report, nil
	}
	if err := crossCheckPDFInfo(workerCtx, opt.PDFInfoPath, reportPath, &report, opt.MaxOutputBytes); err != nil {
		report.Valid = false
		report.Reason = err.Error()
	}
	return report, nil
}

func normalizedStructuralOptions(opt StructuralOptions) StructuralOptions {
	d := DefaultStructuralOptions()
	if opt.Timeout <= 0 {
		opt.Timeout = d.Timeout
	}
	if opt.MaxPages <= 0 {
		opt.MaxPages = d.MaxPages
	}
	if opt.MaxOutputBytes <= 0 {
		opt.MaxOutputBytes = d.MaxOutputBytes
	}
	return opt
}

// RunStructuralWorker is the worker-side JSON entrypoint. It is intentionally
// the only function in this package that calls pdfcpu parsing APIs. The command
// dispatcher must wire WorkerArgument to this function before normal startup.
func RunStructuralWorker(in io.Reader, out io.Writer) error {
	limited := io.LimitReader(in, 16<<10+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read pdf worker request: %w", err)
	}
	if len(raw) > 16<<10 {
		return errors.New("pdf worker request exceeds 16KiB")
	}
	var req workerRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Errorf("decode pdf worker request: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("pdf worker request contains multiple values")
	}
	if req.Path == "" {
		return errors.New("pdf worker request has no path")
	}
	if req.MaxPages <= 0 {
		req.MaxPages = DefaultStructuralOptions().MaxPages
	}

	// A sanitize request re-inspects its own rewrite, so inspecting the source
	// as well would parse an untrusted file twice for one answer.
	report := StructuralReport{}
	if req.SanitizeTo == "" {
		report = inspectWithPDFCPU(req)
	} else {
		report = sanitizeWithPDFCPU(req)
	}
	enc := json.NewEncoder(out)
	return enc.Encode(report)
}

// WorkerMain is an IO-friendly alias for test binaries and executable dispatch.
func WorkerMain(in io.Reader, out io.Writer) error { return RunStructuralWorker(in, out) }

// sanitizeWithPDFCPU rewrites req.Path without its embedded files and reports
// on the REWRITE, so the parent never has to trust that the removal was
// complete: a surviving marker simply keeps the file quarantined. A failed
// rewrite leaves no destination file behind.
func sanitizeWithPDFCPU(req workerRequest) StructuralReport {
	conf := model.NewDefaultConfiguration()
	if err := api.RemoveAttachmentsFile(req.Path, req.SanitizeTo, nil, conf); err != nil {
		_ = os.Remove(req.SanitizeTo)
		// Same discipline as inspectWithPDFCPU: name the category, never the
		// upstream text, which embeds the absolute quarantine path.
		return StructuralReport{Reason: "strip embedded files failed"}
	}
	return inspectWithPDFCPU(workerRequest{Path: req.SanitizeTo, MaxPages: req.MaxPages})
}

func inspectWithPDFCPU(req workerRequest) StructuralReport {
	f, err := os.Open(req.Path)
	if err != nil {
		// The path is deliberately absent from the reason. This string is now
		// durable (validation-report/1) and reachable through an MCP tool, and
		// os.Open's error always embeds the absolute quarantine path — which
		// tells a reader nothing the job and candidate ids do not already say.
		// Same discipline as app.safeType: report the category, never the
		// upstream text.
		return StructuralReport{Reason: "open PDF failed"}
	}
	defer func() { _ = f.Close() }()

	conf := model.NewDefaultConfiguration()
	info, infoErr := api.PDFInfo(f, req.Path, nil, false, conf)
	if infoErr != nil {
		encrypted := strings.Contains(strings.ToLower(infoErr.Error()), "encrypt") || strings.Contains(strings.ToLower(infoErr.Error()), "password")
		reason := "pdfcpu inspection: " + infoErr.Error()
		if encrypted {
			reason = "encrypted PDF: " + infoErr.Error()
		}
		return StructuralReport{Encrypted: encrypted, Reason: reason}
	}
	report := StructuralReport{Pages: info.PageCount, Encrypted: info.Encrypted, HasEmbeddedFiles: len(info.Attachments) != 0}
	if report.Encrypted {
		report.Reason = "encrypted PDF"
		return report
	}
	if report.Pages < 1 {
		report.Reason = "PDF has no pages"
		return report
	}
	if report.Pages > req.MaxPages {
		report.Reason = "PDF page count " + strconv.Itoa(report.Pages) + " exceeds cap " + strconv.Itoa(req.MaxPages)
		return report
	}
	// ReadContext builds pdfcpu's validated object graph. Its Names cache is
	// the reliable location for document-level JavaScript and EmbeddedFiles.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		report.Reason = "rewind PDF: " + err.Error()
		return report
	}
	ctx, err := api.ReadContext(f, conf)
	if err != nil {
		report.Reason = "pdfcpu context: " + err.Error()
		return report
	}
	report.HasJavaScript = ctx.Names["JavaScript"] != nil || hasJavaScript(ctx)
	report.HasEmbeddedFiles = report.HasEmbeddedFiles || ctx.Names["EmbeddedFiles"] != nil || hasEmbeddedFile(ctx)
	if report.HasJavaScript {
		report.Reason = "PDF contains JavaScript"
		return report
	}
	if report.HasEmbeddedFiles {
		report.Reason = "PDF contains embedded files"
		return report
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		report.Reason = "rewind PDF: " + err.Error()
		return report
	}
	if err := api.Validate(f, conf); err != nil {
		report.Reason = "pdfcpu validation: " + err.Error()
		return report
	}
	report.Valid = true
	return report
}

// These checks traverse pdfcpu's parsed object table rather than scanning
// untrusted source bytes. JavaScript may appear as an action instead of a
// document-level name tree; embedded files are FileSpecs with an EF dictionary.
func hasJavaScript(ctx *model.Context) bool {
	for _, entry := range ctx.Table {
		if entry == nil {
			continue
		}
		if d, ok := objectDict(entry.Object); ok {
			if name := d.NameEntry("S"); name != nil && *name == "JavaScript" {
				return true
			}
			if _, ok := d["JS"]; ok {
				return true
			}
		}
	}
	return false
}

func hasEmbeddedFile(ctx *model.Context) bool {
	for _, entry := range ctx.Table {
		if entry == nil {
			continue
		}
		if d, ok := objectDict(entry.Object); ok {
			if _, ok := d["EF"]; ok {
				return true
			}
		}
	}
	return false
}

func objectDict(o types.Object) (types.Dict, bool) {
	switch v := o.(type) {
	case types.Dict:
		return v, true
	case types.StreamDict:
		return v.Dict, true
	case *types.StreamDict:
		return v.Dict, true
	default:
		return nil, false
	}
}

func validateWorkerReport(report *StructuralReport, maxPages int) error {
	if !report.Valid {
		return nil
	}
	if report.Encrypted {
		return errors.New("worker accepted encrypted PDF")
	}
	if report.HasJavaScript {
		return errors.New("worker accepted PDF containing JavaScript")
	}
	if report.HasEmbeddedFiles {
		return errors.New("worker accepted PDF containing embedded files")
	}
	if report.Pages < 1 {
		return errors.New("worker accepted PDF with no pages")
	}
	if report.Pages > maxPages {
		return fmt.Errorf("worker page count %d exceeds cap %d", report.Pages, maxPages)
	}
	return nil
}

func crossCheckPDFInfo(ctx context.Context, binary, path string, report *StructuralReport, limit int64) error {
	if binary == "" || !report.Valid {
		return nil
	}
	cmd := func() *exec.Cmd {
		cmd := func() *exec.Cmd { cmd := exec.CommandContext(ctx, binary, path); configureProcessTree(cmd); return cmd }()
		configureProcessTree(cmd)
		return cmd
	}()
	var out cappedBuffer
	out.limit = limit
	var stderr cappedBuffer
	stderr.limit = 8 << 10
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("pdfinfo timed out: %w", ctx.Err())
		}
		return fmt.Errorf("pdfinfo cross-check failed: %s", strings.TrimSpace(stderr.String()))
	}
	if out.exceeded {
		return errors.New("pdfinfo output exceeds cap")
	}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.SplitN(line, ":", 2)
		if len(fields) != 2 || !strings.EqualFold(strings.TrimSpace(fields[0]), "Pages") {
			continue
		}
		pages, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil || pages != report.Pages {
			return errors.New("pdfinfo page count disagrees with worker")
		}
		return nil
	}
	return errors.New("pdfinfo output did not contain page count")
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.limit <= 0 {
		b.limit = 1
	}
	remaining := b.limit + 1 - int64(b.Len())
	if remaining <= 0 {
		b.exceeded = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

// ReadFrom shadows bytes.Buffer.ReadFrom, which would otherwise be promoted
// through embedding and bypass Write (and therefore the output cap) in io.Copy.
func (b *cappedBuffer) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			_, _ = b.Write(buf[:n])
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
