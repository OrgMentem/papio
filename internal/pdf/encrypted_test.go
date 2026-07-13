// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func TestStructuralWorkerRejectsEncryptedPDF(t *testing.T) {
	plain := writePDFWithPages(t, 1)
	encrypted := filepath.Join(t.TempDir(), "encrypted.pdf")
	if err := api.EncryptFile(plain, encrypted, model.NewAESConfiguration("user", "owner", 128)); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(workerRequest{Path: encrypted, MaxPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunStructuralWorker(bytes.NewReader(req), &out); err != nil {
		t.Fatal(err)
	}
	var report StructuralReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Valid || !report.Encrypted {
		t.Fatalf("encrypted PDF accepted: %+v", report)
	}
}
