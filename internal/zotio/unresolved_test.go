// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/work"
)

// registryMissNote is zotio's note, and the tail of its error line, for a DOI
// that neither registry zotio asks holds. It is copied from the 2026-09-24
// production run, where the DOI was registered with mEDRA.
const registryMissNote = `resolving DOI metadata: fetching CrossRef metadata: HTTP 404: Resource not found.; DataCite: HTTP 404: {"errors":[{"status":"404","title":"The resource you are looking for doesn't exist."}]}`

// registryMissManifest is what "zotio --agent import resolve" prints on stdout
// for that paper, beside a non-zero exit: the entry is unresolved and its note
// names the cause.
func registryMissManifest(t *testing.T, identifier string) string {
	t.Helper()
	note, err := json.Marshal(registryMissNote)
	if err != nil {
		t.Fatal(err)
	}
	return `{"schema_version":2,"dir":"/staging","entries":[{"path":"/staging/paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"` +
		identifier + `","status":"unresolved","note":` + string(note) + `}]}`
}

// errRegistryMiss is the process error that accompanies that manifest.
var errRegistryMiss = errors.New("zotio import: Error: 1 fanout request(s) failed: /staging/paper.pdf: " + registryMissNote)

// unidentifiedManifest is zotio's answer for a PDF whose staged name and
// content carry no DOI or arXiv ID, as for a paper papio knows only by PMID.
const unidentifiedManifest = `{"schema_version":2,"dir":"/staging","entries":[{"path":"/staging/pmid-42389245.pdf","classification":"unidentified","action":"recognize","status":"unresolved","note":"no DOI or arXiv ID found in the filename or the PDF's content; import apply hands this file to Zotero's PDF recognizer"}]}`

// onlyManifestEntry returns the one entry of the one manifest papio holds.
func onlyManifestEntry(t *testing.T, service *Service) map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(service.DataDir, "zotio", "manifests", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("manifests = %v, %v; want exactly one", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Entries) != 1 {
		t.Fatalf("manifest = %s (%v)", data, err)
	}
	return manifest.Entries[0]
}

func TestRegistryMissImportsThePaperFromPapiosRecord(t *testing.T) {
	cli := &planCLI{
		manifest:   registryMissManifest(t, "10.4415/ann_23_04_05"),
		resolveErr: errRegistryMiss,
		preview:    saveTimePreview,
		apply:      connectorApplied,
	}
	service, jobID := readyPlanServiceWork(t, "", cli, work.Work{
		DOI: "10.4415/ann_23_04_05", PMID: "38088393",
		Title:   "Exploring the potential of ChatGPT for clinical reasoning and decision-making.",
		Authors: []string{"Scaioli G", "Lo Moro G"}, Year: 2023,
	})

	status, parent, attachment, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil {
		t.Fatalf("PlanAndApply: %v (class %q)", err, ErrorInfoFrom(err).Class)
	}
	if status != "applied" || parent != "PA12RE34" || attachment != "AT56CH90" {
		t.Fatalf("status=%q parent=%q attachment=%q", status, parent, attachment)
	}
	entry := onlyManifestEntry(t, service)
	if entry["status"] != "resolved" || entry["action"] != "create" || entry["classification"] != "new" {
		t.Fatalf("entry = %v, want a resolved create", entry)
	}
	if entry["identifier_type"] != "doi" || entry["identifier"] != "10.4415/ann_23_04_05" {
		t.Fatalf("entry identifier = %v %v, want zotio's DOI unchanged", entry["identifier_type"], entry["identifier"])
	}
	item, _ := entry["item"].(map[string]any)
	want := map[string]any{
		"itemType": "journalArticle",
		"title":    "Exploring the potential of ChatGPT for clinical reasoning and decision-making.",
		"DOI":      "10.4415/ann_23_04_05",
		"date":     "2023",
		"extra":    "PMID: 38088393",
	}
	for field, value := range want {
		if item[field] != value {
			t.Errorf("item[%q] = %v, want %v", field, item[field], value)
		}
	}
	creators, _ := item["creators"].([]any)
	if len(creators) != 2 {
		t.Fatalf("creators = %v, want both authors", item["creators"])
	}
	first, _ := creators[0].(map[string]any)
	if first["creatorType"] != "author" || first["name"] != "Scaioli G" || first["lastName"] != nil {
		t.Fatalf("first creator = %v, want the name as papio holds it", first)
	}
}

func TestUnidentifiedPMIDOnlyPaperImportsFromPapiosRecord(t *testing.T) {
	cli := &planCLI{
		manifest: unidentifiedManifest,
		preview:  saveTimePreview,
		apply:    connectorApplied,
	}
	service, jobID := readyPlanServiceWork(t, "", cli, work.Work{
		PMID:    "42389245",
		Title:   "MedHopQA: A Disease-Centered Multi-Hop Reasoning Benchmark.",
		Authors: []string{"Islamaj R", "Lu Z."}, Year: 2026,
	})

	status, parent, _, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil {
		t.Fatalf("PlanAndApply: %v (class %q)", err, ErrorInfoFrom(err).Class)
	}
	if status != "applied" || parent != "PA12RE34" {
		t.Fatalf("status=%q parent=%q", status, parent)
	}
	entry := onlyManifestEntry(t, service)
	if entry["status"] != "resolved" || entry["action"] != "create" || entry["classification"] != "new" ||
		entry["identifier_type"] != "pmid" || entry["identifier"] != "42389245" {
		t.Fatalf("entry = %v, want a resolved create named by the PMID", entry)
	}
	item, _ := entry["item"].(map[string]any)
	if item["title"] != "MedHopQA: A Disease-Centered Multi-Hop Reasoning Benchmark." || item["extra"] != "PMID: 42389245" || item["DOI"] != nil {
		t.Fatalf("item = %v", item)
	}
}

// A paper zotio could not identify has had no library check by identifier:
// zotio matches the library by DOI only. papio checks its own identifiers, so
// an item that already holds the PMID gets the PDF rather than a twin.
func TestUnidentifiedPaperAlreadyInLibraryAttachesInsteadOfCreating(t *testing.T) {
	cli := &planCLI{
		manifest: unidentifiedManifest,
		preview:  saveTimePreview,
		found:    `[{"key":"PM12ID34","data":{"key":"PM12ID34"}}]`,
		missing: func() ([]MissingPDFItem, error) {
			return []MissingPDFItem{{Key: "PM12ID34"}}, nil
		},
	}
	service, jobID := readyPlanServiceWork(t, "", cli, work.Work{
		PMID: "42389245", Title: "MedHopQA", Authors: []string{"Islamaj R"}, Year: 2026,
	})

	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatalf("PlanJobs: %v", err)
	}
	if plans[0].Route != "manifest_attach" || plans[0].ExpectedParentKey != "PM12ID34" {
		t.Fatalf("route=%q parent=%q, want an attach to the item that holds the PMID", plans[0].Route, plans[0].ExpectedParentKey)
	}
	entry := onlyManifestEntry(t, service)
	if entry["action"] != "attach" || entry["matched_key"] != "PM12ID34" || entry["item"] != nil {
		t.Fatalf("entry = %v, want an attach with no new item", entry)
	}
}

// The DOI in an unresolved entry may be one zotio read from the PDF's text
// rather than the job's. papio's record describes the job's paper, not that
// one, so it describes nothing and says why.
func TestUnresolvedEntryForAnotherDOIIsRefused(t *testing.T) {
	cli := &planCLI{
		manifest:   registryMissManifest(t, "10.9999/cited.reference"),
		resolveErr: errRegistryMiss,
		preview:    saveTimePreview,
	}
	service, jobID := readyPlanServiceWork(t, "", cli, work.Work{
		PMID: "42389245", Title: "MedHopQA", Authors: []string{"Islamaj R"}, Year: 2026,
	})

	_, err := service.PlanJobs(context.Background(), []string{jobID})
	if err == nil {
		t.Fatal("PlanJobs succeeded, want a refusal")
	}
	info := ErrorInfoFrom(err)
	if info.Class != ErrorClassMetadataUnresolved || info.HTTPStatus != 0 {
		t.Fatalf("error info = %+v, want %q without an HTTP status", info, ErrorClassMetadataUnresolved)
	}
	if message := SanitizeErrorHint(err.Error()); !strings.Contains(message, "not the job's DOI") {
		t.Fatalf("recorded message = %q, want the reason inside the bound", message)
	}
	if cli.previewCalls != 0 || cli.applyCalls != 0 {
		t.Fatalf("previewCalls=%d applyCalls=%d, want no mutation", cli.previewCalls, cli.applyCalls)
	}
}
