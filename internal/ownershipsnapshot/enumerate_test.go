// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"papio/internal/config"
)

func fileSource(path, format string) config.LibrarySource {
	return config.LibrarySource{Name: "export", Kind: config.LibraryKindFile, Path: path, Format: format, Claim: config.LibraryClaimRecordPresent}
}

func writeExport(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Retraction scope reads the library through this loader, so it must return
// exactly the DOI-bearing records of every supported export, with or without
// a configured format, and drop records that carry no DOI.
func TestEnumerateLibraryRecordsReturnsDOIRecords(t *testing.T) {
	const bibtex = `@article{a, title = {Alpha}, doi = {10.1000/alpha}}
@book{b, title = {No DOI Book}}
`
	const ris = "TY  - JOUR\nTI  - Beta\nDO  - 10.1000/beta\nER  - \nTY  - BOOK\nTI  - Undigitized\nER  - \n"
	const csl = `[{"type":"article-journal","title":"Gamma","DOI":"10.1000/gamma"},{"type":"book","title":"Plain"}]`
	for _, tc := range []struct {
		name, file, format, body string
		want                     []LibraryRecord
	}{
		{"bibtex by extension", "lib.bib", "", bibtex, []LibraryRecord{{DOI: "10.1000/alpha", Title: "Alpha"}}},
		{"bibtex by content", "export", "", bibtex, []LibraryRecord{{DOI: "10.1000/alpha", Title: "Alpha"}}},
		{"ris configured", "export.txt", "ris", ris, []LibraryRecord{{DOI: "10.1000/beta", Title: "Beta"}}},
		{"csl-json by extension", "lib.json", "", csl, []LibraryRecord{{DOI: "10.1000/gamma", Title: "Gamma"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, err := EnumerateLibraryRecords(context.Background(), fileSource(writeExport(t, tc.file, tc.body), tc.format))
			if err != nil {
				t.Fatalf("EnumerateLibraryRecords: %v", err)
			}
			if !reflect.DeepEqual(records, tc.want) {
				t.Fatalf("records = %+v, want %+v", records, tc.want)
			}
		})
	}
}

// An export with no entries is an empty library, not a failure.
func TestEnumerateLibraryRecordsEmptyExportIsEmpty(t *testing.T) {
	records, err := EnumerateLibraryRecords(context.Background(), fileSource(writeExport(t, "lib.bib", "\n"), ""))
	if err != nil || len(records) != 0 {
		t.Fatalf("records = %+v, err = %v; want an empty library", records, err)
	}
}

// Every way the loader cannot read the whole library is an error naming the
// source, never an empty or partial library.
func TestEnumerateLibraryRecordsRefusesWhatItCannotReadWhole(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), "huge.bib")
	if err := os.WriteFile(oversized, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sparse: no real disk, but a read past the cap.
	if err := os.Truncate(oversized, defaultMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source config.LibrarySource
		is     error
		text   string
	}{
		{name: "blank name", source: config.LibrarySource{Name: "  ", Kind: config.LibraryKindFile, Path: "x.bib"}, text: "name is required"},
		{name: "blank path", source: fileSource("  ", ""), text: `"export": path is required`},
		{name: "unsupported kind", source: config.LibrarySource{Name: "export", Kind: "url", Path: "x"}, text: `kind "url" is not supported`},
		{name: "missing file", source: fileSource(filepath.Join(t.TempDir(), "absent.bib"), ""), is: os.ErrNotExist, text: `"export"`},
		{name: "oversized file", source: fileSource(oversized, ""), text: "exceeds"},
		{name: "malformed bibtex", source: fileSource(writeExport(t, "bad.bib", "@article{a, title = {Unclosed"), ""), text: `"export"`},
		{name: "unsupported format", source: fileSource(writeExport(t, "lib.bib", "@article{a, doi = {10.1000/a}}"), "marc"), text: "unsupported bibliographic format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, err := EnumerateLibraryRecords(context.Background(), tc.source)
			if err == nil {
				t.Fatalf("records = %+v, want an error", records)
			}
			if records != nil {
				t.Fatalf("records = %+v alongside error %v, want none", records, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error = %v, want it to wrap %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.text)
			}
		})
	}
}

// A cancelled caller gets its own cancellation back, before any read.
func TestEnumerateLibraryRecordsHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := writeExport(t, "lib.bib", "@article{a, doi = {10.1000/a}}")
	if records, err := EnumerateLibraryRecords(ctx, fileSource(path, "")); !errors.Is(err, context.Canceled) || records != nil {
		t.Fatalf("records = %+v, err = %v; want context.Canceled and nothing", records, err)
	}
}
