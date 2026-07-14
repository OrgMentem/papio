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

// DetectCapability finds optional local helpers. It makes no network calls.
func DetectCapability() Capability {
	c := Capability{PDFCPU: true}
	c.PDFInfo, _ = exec.LookPath("pdfinfo")
	c.PDFToText, _ = exec.LookPath("pdftotext")
	c.PDFToPPM, _ = exec.LookPath("pdftoppm")
	c.Tesseract, _ = exec.LookPath("tesseract")
	return c
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

// SemanticExtract is an alias for ExtractText.
func SemanticExtract(ctx context.Context, path string, cap Capability, opt SemanticOptions) (TextReport, error) {
	return ExtractText(ctx, path, cap, opt)
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
		cmd := exec.CommandContext(ctx, cap.PDFToPPM, "-f", "1", "-l", fmt.Sprint(opt.OCRPages), "-scale-to", "2000", "-png", path, prefix)
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
		all.WriteString(text)
	}
	return all.String(), nil
}
