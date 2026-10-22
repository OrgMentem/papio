// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SemanticOptions limits untrusted external converters. A zero value is filled
// from DefaultSemanticOptions.
type SemanticOptions struct {
	Timeout        time.Duration
	OCRTimeout     time.Duration
	MinChars       int
	MaxOutputBytes int64
	MaxExcerpt     int
	OCRPages       int
	OCRMaxOutput   int64
	MaxRasterBytes int64
}

func DefaultSemanticOptions() SemanticOptions {
	return SemanticOptions{
		Timeout:        30 * time.Second,
		OCRTimeout:     60 * time.Second,
		MinChars:       1_000,
		MaxOutputBytes: 1 << 20,
		MaxExcerpt:     16 << 10,
		OCRPages:       3,
		OCRMaxOutput:   1 << 20,
		MaxRasterBytes: 64 << 20,
	}
}

// toolSearchPath lists the package-manager prefixes a GUI-launched process does
// not inherit. macOS hands a launchd child PATH=/usr/local/bin:/usr/bin:/bin:
// /usr/sbin:/sbin, so a daemon autostarted by the browser's native-messaging
// host cannot see Homebrew at all. Detection that trusted PATH alone therefore
// made text extraction depend on WHO started the daemon: from a shell it works,
// from the browser every PDF fails semantic extraction and is staged for human
// review. PATH still wins; these are only consulted when it comes up empty.
var toolSearchPath = []string{
	"/opt/homebrew/bin", // Homebrew on Apple silicon
	"/usr/local/bin",    // Homebrew on Intel macOS, common manual installs
	"/opt/local/bin",    // MacPorts
	"/home/linuxbrew/.linuxbrew/bin",
	"/usr/bin",
	"/snap/bin",
}

// DetectCapability finds optional local helpers. It makes no network calls.
func DetectCapability() Capability {
	c := Capability{PDFCPU: true}
	c.PDFInfo = lookTool("pdfinfo")
	c.PDFDetach = lookTool("pdfdetach")
	c.PDFToText = lookTool("pdftotext")
	c.PDFToPPM = lookTool("pdftoppm")
	c.Tesseract = lookTool("tesseract")
	return c
}

func lookTool(name string) string {
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	for _, dir := range toolSearchPath {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// ExtractText invokes pdftotext in a bounded subprocess, then uses a bounded
// raster/OCR fallback only when regular text is too sparse. Tool absence is an
// explicit review signal, never a successful extraction.
func ExtractText(ctx context.Context, path string, cap Capability, opt SemanticOptions) (TextReport, error) {
	opt = normalizedSemanticOptions(opt)
	if cap.PDFToText == "" {
		return TextReport{NeedsReview: true, Evidence: []string{"capability: pdftotext unavailable"}}, nil
	}
	text, err := runTextTool(ctx, opt.Timeout, opt.MaxOutputBytes, cap.PDFToText, path, "-")
	if err != nil {
		// Cancellation is not a semantic verdict. runTextTool returns ctx.Err()
		// when the parent context is done, and the caller
		// (Service.validateCandidate) settles a cancelled attempt only when
		// Validate reports an error — so swallowing this leaves an open
		// attempt row and drives database writes on a dead context. Same rule
		// as metadata.go's extraction path.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return TextReport{}, ctxErr
		}
		return TextReport{NeedsReview: true, Evidence: []string{"pdftotext failed: " + err.Error()}}, nil
	}
	report := textReport(text, false, opt)
	report.Evidence = append(report.Evidence, "pdftotext extracted text")
	unique := uniqueTextChars(text)
	if report.Chars >= int64(opt.MinChars) && unique >= int64(opt.MinChars) {
		return report, nil
	}
	if report.Chars >= int64(opt.MinChars) {
		// Enough raw characters, but nearly all of them are the same repeated
		// line(s) — the ProQuest scan case: a per-page "reproduced with
		// permission" watermark is the only text layer. Character count is
		// volume, not signal; distinct content is what identity needs.
		report.Evidence = append(report.Evidence,
			fmt.Sprintf("text is repeated boilerplate (%d chars, %d distinct)", report.Chars, unique))
	} else {
		report.Evidence = append(report.Evidence, "text is sparse or image-only")
	}
	if !cap.OCR() {
		report.NeedsReview = true
		report.Evidence = append(report.Evidence, "capability: pdftoppm and/or tesseract unavailable")
		return report, nil
	}
	ocr, err := extractOCR(ctx, path, cap, opt)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return TextReport{}, ctxErr
		}
		report.NeedsReview = true
		report.Evidence = append(report.Evidence, "OCR unavailable: "+err.Error())
		return report, nil
	}
	report = textReport(ocr, true, opt)
	report.Evidence = append(report.Evidence, "OCR fallback used")
	if report.Chars < int64(opt.MinChars) {
		report.NeedsReview = true
		report.Evidence = append(report.Evidence, "OCR text remains below minimum character threshold")
	}
	return report, nil
}

func normalizedSemanticOptions(o SemanticOptions) SemanticOptions {
	d := DefaultSemanticOptions()
	if o.Timeout <= 0 {
		o.Timeout = d.Timeout
	}
	if o.OCRTimeout <= 0 {
		o.OCRTimeout = d.OCRTimeout
	}
	if o.MinChars <= 0 {
		o.MinChars = d.MinChars
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = d.MaxOutputBytes
	}
	if o.MaxExcerpt <= 0 {
		o.MaxExcerpt = d.MaxExcerpt
	}
	if o.OCRPages <= 0 {
		o.OCRPages = d.OCRPages
	}
	if o.OCRMaxOutput <= 0 {
		o.OCRMaxOutput = d.OCRMaxOutput
	}
	if o.MaxRasterBytes <= 0 {
		o.MaxRasterBytes = d.MaxRasterBytes
	}
	return o
}

func runTextTool(parent context.Context, timeout time.Duration, limit int64, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, args...)
		configureProcessTree(cmd)
		return cmd
	}()
	var stdout cappedBuffer
	stdout.limit = limit
	var stderr cappedBuffer
	stderr.limit = 8 << 10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if stdout.exceeded {
		return "", fmt.Errorf("tool output exceeds %d bytes", limit)
	}
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", errors.New(detail)
		}
		return "", err
	}
	return stdout.String(), nil
}

// MaxTextBytes is the most extracted text one page-range read returns. It is
// sized from JSON's worst case, not from the typical page: the text is
// untrusted, and a byte that escapes (a control character, an invalid UTF-8
// byte, <, > or &) costs up to six bytes on the wire. 128 KiB of such bytes is
// 768 KiB, which leaves headroom for the envelope under ipc.MaxResultBytes
// (1 MiB). A page of prose is a few KiB, so one read still spans dozens of
// pages; the page range is the paging mechanism.
const MaxTextBytes = 128 << 10

// pageTextToolLimit bounds what pdftotext may emit for one read before the
// output is cut to the requested byte cap. It is the memory bound; the byte
// cap is the response bound.
const pageTextToolLimit = 8 << 20

var (
	// ErrToolUnavailable means pdftotext is not installed. It is a missing
	// capability, never an empty extraction.
	ErrToolUnavailable = errors.New("pdftotext is not installed")
	// ErrInvalidPageRange means the requested pages are not inside the document.
	ErrInvalidPageRange = errors.New("invalid page range")
	// ErrTextTooLarge means pdftotext produced more than the tool limit; a
	// narrower page range avoids it.
	ErrTextTooLarge = errors.New("extracted text is too large; request a narrower page range")
)

// PageTextRequest selects pages and a byte cap for ExtractPageText. Zero
// FirstPage means page 1, zero LastPage means "to the end", and zero MaxBytes
// means MaxTextBytes. PagesTotal is the document's page count when known (0
// when unknown); it validates and clamps the range.
type PageTextRequest struct {
	FirstPage  int
	LastPage   int
	PagesTotal int
	MaxBytes   int
}

// PageText is one bounded page-range read. OCR is never used.
type PageText struct {
	Text       string
	FirstPage  int
	LastPage   int
	PagesTotal int
	Chars      int64
	Truncated  bool
}

// ExtractPageText runs pdftotext over a page range through runTextTool, so it
// keeps the same timeout and process-tree kill as validation. When the text
// exceeds the byte cap it is cut on a rune boundary, Truncated is set, and
// LastPage names the page the cut fell in so the caller can resume there.
func ExtractPageText(ctx context.Context, path string, cap Capability, opt SemanticOptions, req PageTextRequest) (PageText, error) {
	if cap.PDFToText == "" {
		return PageText{}, ErrToolUnavailable
	}
	opt = normalizedSemanticOptions(opt)
	first, last := req.FirstPage, req.LastPage
	if first < 0 || last < 0 || (last != 0 && first > last) {
		return PageText{}, fmt.Errorf("%w: pages %d-%d", ErrInvalidPageRange, first, last)
	}
	if first == 0 {
		first = 1
	}
	if req.PagesTotal > 0 {
		if first > req.PagesTotal {
			return PageText{}, fmt.Errorf("%w: page %d is past the last page (%d)", ErrInvalidPageRange, first, req.PagesTotal)
		}
		if last == 0 || last > req.PagesTotal {
			last = req.PagesTotal
		}
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 || maxBytes > MaxTextBytes {
		maxBytes = MaxTextBytes
	}
	args := []string{}
	if first > 1 || last != 0 {
		args = append(args, "-f", strconv.Itoa(first))
	}
	if last != 0 {
		args = append(args, "-l", strconv.Itoa(last))
	}
	args = append(args, path, "-")
	text, err := runTextTool(ctx, opt.Timeout, max(opt.MaxOutputBytes, pageTextToolLimit), cap.PDFToText, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, context.DeadlineExceeded) {
			return PageText{}, ctxErr
		}
		if strings.HasPrefix(err.Error(), "tool output exceeds") {
			return PageText{}, ErrTextTooLarge
		}
		return PageText{}, err
	}
	result := PageText{FirstPage: first, PagesTotal: req.PagesTotal}
	if len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		result.Truncated = true
		result.LastPage = first + strings.Count(text, "\f")
	} else {
		pages := strings.Count(text, "\f")
		if text != "" && !strings.HasSuffix(text, "\f") {
			pages++
		}
		result.LastPage = last
		if result.LastPage == 0 {
			result.LastPage = first + max(pages, 1) - 1
		}
		if result.PagesTotal == 0 && first == 1 && req.LastPage == 0 {
			result.PagesTotal = result.LastPage
		}
	}
	result.Text = text
	result.Chars = int64(utf8.RuneCountInString(text))
	return result, nil
}

func textReport(text string, ocr bool, opt SemanticOptions) TextReport {
	excerpt := text
	if len(excerpt) > opt.MaxExcerpt {
		excerpt = excerpt[:opt.MaxExcerpt]
	}
	return TextReport{Chars: int64(utf8.RuneCountInString(text)), Excerpt: excerpt, OCRUsed: ocr}
}

// uniqueTextChars measures distinct content: rune count over the set of
// normalized (trimmed, lowercased) unique lines. A per-page watermark repeated
// thirteen times contributes its length once. Identity matching needs distinct
// signal, not volume.
func uniqueTextChars(text string) int64 {
	seen := map[string]struct{}{}
	var n int64
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(strings.Join(strings.Fields(line), " "))
		if l == "" {
			continue
		}
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		n += int64(utf8.RuneCountInString(l))
	}
	return n
}

func extractOCR(parent context.Context, path string, cap Capability, opt SemanticOptions) (string, error) {
	dir, err := os.MkdirTemp("", "papio-ocr-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	prefix := filepath.Join(dir, "page")
	// scale-to bounds raster dimensions in addition to page count and timeout.
	ctx, cancel := context.WithTimeout(parent, opt.OCRTimeout)
	defer cancel()
	cmd := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, cap.PDFToPPM, "-f", "1", "-l", fmt.Sprint(opt.OCRPages), "-scale-to", "2000", "-png", path, prefix) //nolint:gosec // G204: PDFToPPM is a discovered trusted binary path; path/prefix are internal quarantine paths, not user shell input.
		configureProcessTree(cmd)
		return cmd
	}()
	var stderr cappedBuffer
	stderr.limit = 8 << 10
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("pdftoppm: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	pages, err := filepath.Glob(prefix + "-*.png")
	if err != nil {
		return "", err
	}
	sort.Strings(pages)
	if len(pages) == 0 {
		return "", errors.New("pdftoppm produced no pages")
	}
	if len(pages) > opt.OCRPages {
		pages = pages[:opt.OCRPages]
	}
	var all strings.Builder
	var rasterBytes int64
	for _, page := range pages {
		info, err := os.Stat(page)
		if err != nil {
			return "", err
		}
		rasterBytes += info.Size()
		if rasterBytes > opt.MaxRasterBytes {
			return "", errors.New("OCR raster output exceeds cap")
		}
		remaining := opt.OCRMaxOutput - int64(all.Len())
		if remaining <= 0 {
			return "", errors.New("OCR output exceeds cap")
		}
		text, err := runTextTool(ctx, opt.OCRTimeout, remaining, cap.Tesseract, page, "stdout")
		if err != nil {
			return "", fmt.Errorf("tesseract: %w", err)
		}
		appendOCRPage(&all, text)
	}
	return all.String(), nil
}

// appendOCRPage joins one page of OCR text to the document, separating pages
// with a form feed.
//
// The separator is load-bearing, not cosmetic. pdftotext emits a form feed at
// every page break and identityWindow derives "page one" by cutting on the
// first one, so text with no form feed makes every front-matter rule read the
// first N bytes of the WHOLE document as page one. Tesseract is invoked once
// per page and its output carries no page break of its own, so concatenating
// the pages plainly — which this did — handed the identity rules a page one
// spanning as many pages as OCR produced.
//
// The blind path is where that mattered most: FrontMatterDOIs takes the
// conclusive DOI set from the 1 KiB window, so a scanned page one with little
// text let a DOI printed on page two fall inside that window, and a targetless
// capture could be named as whatever work page two happened to cite.
func appendOCRPage(all *strings.Builder, text string) {
	if all.Len() > 0 {
		all.WriteString("\f")
	}
	all.WriteString(text)
}
