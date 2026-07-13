// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package pdf

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePayloadFileMatchesByteGate(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"valid-with-eof", append(append([]byte("%PDF-1.7\n"), make([]byte, MinimumPayloadBytes)...), []byte("%%EOF")...)},
		{"valid-without-eof", append([]byte("%PDF-1.7\n"), make([]byte, MinimumPayloadBytes)...)},
		{"short", []byte("%PDF-1.7\nshort")},
		{"html", append([]byte("<html>"), make([]byte, MinimumPayloadBytes)...)},
		{"early-eof", append(append([]byte("%PDF-1.7\n%%EOF"), make([]byte, eofSearchBytes+100)...), []byte("trailer")...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.pdf")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			fileReport, err := ValidatePayloadFile(path, "application/pdf")
			if err != nil {
				t.Fatal(err)
			}
			byteReport := ValidatePayload(tc.body, "application/pdf")
			if fileReport.OK != byteReport.OK || fileReport.HasHeader != byteReport.HasHeader ||
				fileReport.HasEOF != byteReport.HasEOF || fileReport.SizeBytes != byteReport.SizeBytes || fileReport.Reason != byteReport.Reason {
				t.Fatalf("file report %+v != byte report %+v", fileReport, byteReport)
			}
		})
	}
}

func TestValidateUsesPathGateWithoutWorkerForInvalidPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-pdf.pdf")
	if err := os.WriteFile(path, append([]byte("<html>"), make([]byte, MinimumPayloadBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(context.Background(), ValidationInput{Path: path, DeclaredMIME: "application/pdf"}, ValidationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Payload.OK || report.Structural.Valid {
		t.Fatalf("invalid file reached structural worker: %+v", report)
	}
}

func TestValidatePayloadFileRejectsDirectories(t *testing.T) {
	if _, err := ValidatePayloadFile(t.TempDir(), "application/pdf"); err == nil {
		t.Fatal("directory accepted as PDF payload")
	}
}
