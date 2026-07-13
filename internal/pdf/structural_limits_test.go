// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStructuralWorkerEnforcesPageLimit(t *testing.T) {
	path := writePDFWithPages(t, 2)
	req, err := json.Marshal(workerRequest{Path: path, MaxPages: 1})
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
	if report.Valid || report.Pages != 2 || !strings.Contains(report.Reason, "exceeds cap") {
		t.Fatalf("report=%+v", report)
	}
}

func writePDFWithPages(t *testing.T, pages int) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"", // filled after page object numbers are known
	}
	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		pageObject := 3 + i
		contentObject := 3 + pages + i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObject))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents %d 0 R >>", contentObject))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages)
	for range pages {
		objects = append(objects, "<< /Length 0 >>\nstream\n\nendstream")
	}
	offsets := make([]int, 0, len(objects))
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	path := filepath.Join(t.TempDir(), "pages.pdf")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
