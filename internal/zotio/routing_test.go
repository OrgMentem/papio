// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zotio

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/work"
)

// A SICI DOI carries ':' in its suffix. url.PathEscape keeps it, but a colon in
// a Windows path component is the NTFS stream separator, so the staged file
// could not be created. The name must hold no ':' and still decode to the DOI.
func TestImportStagingBasenameEncodesColon(t *testing.T) {
	const doi = "10.1002/(SICI)1097-0258(19980430)17:8<857::AID-SIM777>3.0.CO;2-E"
	got, err := importStagingBasename(work.Work{DOI: doi})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, ":<>\"|?*/\\") {
		t.Fatalf("staging name %q holds a character invalid on Windows", got)
	}
	decoded, err := url.PathUnescape(strings.TrimSuffix(got, ".pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != strings.ToLower(doi) {
		t.Fatalf("staging name %q decodes to %q, want %q", got, decoded, strings.ToLower(doi))
	}
}

// zotio scans every PDF in the directory it resolves. An earlier papio staged
// the DOI file under a different name directly in the job/artifact directory;
// a leftover there must not join the new staged file in one scan.
func TestResolveManifestIgnoresLeftoverFromEarlierStagingName(t *testing.T) {
	cli := &planCLI{
		manifest: `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`,
		preview:  `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Dir(cli.lastResolveAt)
	leftover := filepath.Join(artifactDir, "10.1002%2Fexample:old.pdf")
	if err := os.WriteFile(leftover, []byte("%PDF-leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.resolveManifest(context.Background(), plans[0], work.Work{DOI: "10.1002/example"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cli.lastResolveAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "10.1002%2Fexample.pdf" {
		t.Fatalf("resolve directory holds %v, want exactly 10.1002%%2Fexample.pdf", entries)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("earlier staged file was removed: %v", err)
	}
}
