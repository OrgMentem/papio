// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package pdf validates artifacts: payload magic checks, structural parsing in
// a bounded re-exec'd worker (hostile inputs never parse in the daemon
// process), external Poppler semantics, Tesseract OCR fallback, and
// deterministic identity matching. This file pins the contract; the
// implementations live in the sibling files.
package pdf

// PayloadReport is the cheap pre-parse gate on downloaded bytes, following the
// instsci behavioral reference: minimum plausible size, %PDF- header (with a
// small scan window for BOM/garbage prefixes), and an %%EOF marker searched
// within a bounded tail window (some real PDFs append data after %%EOF).
type PayloadReport struct {
	OK          bool
	SizeBytes   int64
	HasHeader   bool
	HasEOF      bool
	SniffedMIME string
	Reason      string // set when !OK
}

// StructuralReport comes from the isolated pdfcpu worker.
type StructuralReport struct {
	Valid            bool
	Pages            int
	Encrypted        bool
	HasJavaScript    bool
	HasEmbeddedFiles bool
	Reason           string // set when !Valid
}

// TextReport is the semantic extraction result (pdftotext, or OCR fallback).
type TextReport struct {
	Chars   int64
	Excerpt string // bounded head of extracted text for identity matching
	OCRUsed bool
}

// Capability reports which external tools are available (doctor surface).
type Capability struct {
	PDFCPU    bool   // in-binary structural worker (always true once built)
	PDFInfo   string // absolute path or ""
	PDFToText string
	PDFToPPM  string
	Tesseract string
}

// Semantic reports whether text extraction is possible at all.
func (c Capability) Semantic() bool { return c.PDFToText != "" }

// OCR reports whether the raster+OCR fallback is possible.
func (c Capability) OCR() bool { return c.PDFToPPM != "" && c.Tesseract != "" }

// Identity decision results.
const (
	IdentityPass   = "pass"
	IdentityReject = "reject"
	IdentityReview = "review"
)

// IdentityDecision explains whether the artifact is the requested work.
type IdentityDecision struct {
	Result   string
	Evidence []string
}
