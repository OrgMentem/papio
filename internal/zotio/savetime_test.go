// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	saveTimePreview = `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`
	// newItemManifest is zotio's resolver answer for a DOI whose Crossref
	// record carries no abstract.
	newItemManifest = `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`
	// connectorApplied is zotio's answer once a stored connector create has
	// saved the item, filed it, attached the PDF and read both keys back.
	connectorApplied = `{"schema_version":1,"ok":true,"operation":"import.apply","mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"attempted":1,"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"op_id":"import.apply:001:create","key":"PA12RE34","status":"applied","reason":{"attachment_key":"AT56CH90","committed":true,"key":"PA12RE34","parent_key":"PA12RE34","via":"connector"}}]}}`
	// readingCollections is "zotio --agent collections list" for a library
	// with one collection named "Reading".
	readingCollections = `{"meta":{"source":"live"},"results":[{"key":"RD12NG34","data":{"key":"RD12NG34","name":"Reading","parentCollection":false,"version":3}},{"key":"OT56HR78","data":{"key":"OT56HR78","name":"Other","parentCollection":false,"version":4}}]}`
	// targetMissingApply is zotio's answer when the desktop cannot map the
	// collection key to a target, as for a collection the Web API created and
	// the desktop has not synced: nothing was saved.
	targetMissingApply = `{"schema_version":1,"ok":false,"operation":"import.apply","mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"attempted":1,"applied":0,"no_op":0,"conflicts":0,"failed":1},"items":[{"op_id":"import.apply:001:create","key":"","status":"failed","reason":"collection RD12NG34 maps to path \"Reading\", but no desktop connector target matched it; run 'zotio import targets --agent' and pass --connector-target C<n>"}]}}`
	// filingConflictApply is zotio's answer when the desktop saved the item
	// but could not file it: the save is committed and the PDF never went.
	filingConflictApply = `{"schema_version":1,"ok":false,"operation":"import.apply","mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"attempted":1,"applied":0,"no_op":0,"conflicts":1,"failed":0},"items":[{"op_id":"import.apply:001:create","key":"","status":"conflict","reason":{"via":"connector","committed":true,"session":"S1","connector_key":"C1","title":"Example Paper","message":"item \"Example Paper\" was saved but its collection filing failed"}}]}}`
	openAlexAbstract    = "An abstract from OpenAlex."
)

// errMutationIncomplete is the error line zotio exits with beside an
// incomplete mutation envelope.
var errMutationIncomplete = errors.New("zotio import: Error: mutation incomplete")

// saveTimeService is a ready new-item job whose policy files into "Reading"
// and asks for auto-import with auto-enrich, and whose discovery source knows
// the abstract that Crossref lacks.
func saveTimeService(t *testing.T, cli *planCLI) (*Service, string, *int) {
	t.Helper()
	service, jobID := readyPlanService(t, "", cli)
	service.AutoEnrich = true
	lookups := 0
	service.Abstracts = func(_ context.Context, doi string) (string, error) {
		lookups++
		if doi != "10.1002/example" {
			t.Errorf("abstract lookup for %q, want the job's DOI", doi)
		}
		return openAlexAbstract, nil
	}
	enablePlanAutoImport(t, service, jobID, "Reading")
	return service, jobID, &lookups
}

// onlyManifestItem returns the item of the one manifest papio holds.
func onlyManifestItem(t *testing.T, service *Service) map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(service.DataDir, "zotio", "manifests", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("manifests = %v, %v; want exactly one", paths, err)
	}
	return manifestItemAt(t, paths[0])
}

func manifestItemAt(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Entries []struct {
			Item map[string]any `json:"item"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Entries) != 1 {
		t.Fatalf("manifest %s = %s (%v)", path, data, err)
	}
	return manifest.Entries[0].Item
}

// zotio reads the manifest from disk when it applies, and the confirmation
// digest covered only the manifest's path. A manifest rewritten between the
// preview and the apply was therefore applied under the old confirmation.
func TestApplyRefusesManifestChangedAfterPlanning(t *testing.T) {
	cli := &planCLI{manifest: newItemManifest, preview: saveTimePreview, apply: connectorApplied}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	confirmed, err := os.ReadFile(plan.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(confirmed), "Example Paper", "A Different Paper", 1)
	if rewritten == string(confirmed) {
		t.Fatalf("fixture manifest has no title to rewrite: %s", confirmed)
	}
	if err := os.WriteFile(plan.ManifestPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
	if err == nil {
		t.Fatal("applied a manifest rewritten after its plan was confirmed")
	}
	if cli.applyCalls != 0 {
		t.Fatalf("zotio apply ran %d times for a rewritten manifest, want 0", cli.applyCalls)
	}
	if class := ErrorInfoFrom(err).Class; class != ErrorClassPlanConfirmationMismatch {
		t.Fatalf("error class = %q (%v), want %q", class, err, ErrorClassPlanConfirmationMismatch)
	}
	var applies int
	if err := service.Store.DB().QueryRow(`SELECT count(*) FROM exports WHERE kind = 'zotio_apply'`).Scan(&applies); err != nil {
		t.Fatal(err)
	}
	if applies != 0 {
		t.Fatalf("zotio_apply rows = %d, want 0: a refused manifest must not reserve an apply", applies)
	}
}

// A new-item plan from before the binding carries no manifest hash, so its
// manifest cannot be proved, and its own digest still verifies. It must be
// refused rather than applied on trust.
func TestApplyRefusesNewItemPlanWithoutManifestBinding(t *testing.T) {
	cli := &planCLI{manifest: newItemManifest, preview: saveTimePreview, apply: connectorApplied}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	legacy := *plans[0]
	legacy.ManifestSHA256 = ""
	if legacy.ConfirmationSHA256, err = planDigest(&legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := service.writePlan(&legacy); err != nil {
		t.Fatal(err)
	}
	_, err = service.Apply(context.Background(), legacy.ID, legacy.ConfirmationSHA256)
	if err == nil || cli.applyCalls != 0 {
		t.Fatalf("unbound plan: err=%v zotio apply calls=%d, want refusal and 0", err, cli.applyCalls)
	}
}

// A new item that Zotero desktop saves already filed, with its DOI and
// abstract, leaves the Web API follow-ups nothing to do. Before, both ran
// seconds after the save, met a 404 because the desktop had not synced, and
// waited on FollowUpRetrier.
func TestSaveTimeFilingSkipsFollowUpsForNewItem(t *testing.T) {
	cli := &planCLI{manifest: newItemManifest, preview: saveTimePreview, apply: connectorApplied, collections: readingCollections}
	service, jobID, lookups := saveTimeService(t, cli)

	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" || attachmentKey != "AT56CH90" {
		t.Fatalf("import = (%q, %q, %q, %v), want applied PA12RE34/AT56CH90", status, parentKey, attachmentKey, err)
	}
	if cli.collectionCalls != 0 || cli.enrichCalls != 0 {
		t.Fatalf("follow-ups after a whole desktop save: add-to-collection=%d enrich=%d, want 0", cli.collectionCalls, cli.enrichCalls)
	}
	item := onlyManifestItem(t, service)
	if !slices.Equal(anyStrings(item["collections"]), []string{"RD12NG34"}) ||
		item["DOI"] != "10.1002/example" || item["abstractNote"] != openAlexAbstract || item["title"] != "Example Paper" {
		t.Fatalf("applied manifest item = %#v", item)
	}
	if *lookups != 1 {
		t.Fatalf("abstract lookups = %d, want 1", *lookups)
	}
	filing, _ := latestZotioEvent(t, service, jobID, followUpCollectionFiling)
	if filing["status"] != "applied" || filing["with_import"] != true || filing["collection"] != "Reading" || filing["collection_key"] != "RD12NG34" {
		t.Fatalf("collection filing event = %#v", filing)
	}
	enrich, _ := latestZotioEvent(t, service, jobID, followUpEnrich)
	if enrich["status"] != "applied" || enrich["with_import"] != true || enrich["parent_key"] != "PA12RE34" {
		t.Fatalf("enrich event = %#v", enrich)
	}

	// A replay answers from the ledger and must not file the item again.
	if _, _, _, err := service.PlanAndApply(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	if cli.applyCalls != 1 || cli.collectionCalls != 0 {
		t.Fatalf("replay: apply=%d add-to-collection=%d, want 1 and 0", cli.applyCalls, cli.collectionCalls)
	}
}

// papio fills only what the registry record left empty, and only for the
// item zotio built from the job's own DOI.
func TestDescribeNewItemFillsOnlyMissingFieldsOfTheJobsWork(t *testing.T) {
	for _, tc := range []struct {
		name, item, identifier string
		wantDOI, wantAbstract  string
		wantLookups            int
		wantSaves              bool
	}{
		{
			name:         "resolver values stay",
			identifier:   "10.1002/example",
			item:         `{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/EXAMPLE","abstractNote":"A Crossref abstract."}`,
			wantDOI:      "10.1002/EXAMPLE",
			wantAbstract: "A Crossref abstract.",
			wantSaves:    true,
		},
		{
			name:         "missing DOI comes from the job",
			identifier:   "10.1002/example",
			item:         `{"itemType":"journalArticle","title":"Example Paper"}`,
			wantDOI:      "10.1002/example",
			wantAbstract: openAlexAbstract,
			wantLookups:  1,
			wantSaves:    true,
		},
		{
			name:       "another work's item gets nothing from the job",
			identifier: "10.1002/other",
			item:       `{"itemType":"journalArticle","title":"Other Paper","DOI":"10.1002/other"}`,
			wantDOI:    "10.1002/other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := &planCLI{
				manifest:    `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"` + tc.identifier + `","status":"resolved","item":` + tc.item + `}]}`,
				preview:     saveTimePreview,
				collections: readingCollections,
			}
			service, jobID, lookups := saveTimeService(t, cli)
			plans, err := service.PlanJobs(context.Background(), []string{jobID})
			if err != nil {
				t.Fatal(err)
			}
			item := manifestItemAt(t, plans[0].ManifestPath)
			abstract, _ := item["abstractNote"].(string)
			if item["DOI"] != tc.wantDOI || abstract != tc.wantAbstract {
				t.Fatalf("manifest item = %#v, want DOI %q abstract %q", item, tc.wantDOI, tc.wantAbstract)
			}
			if *lookups != tc.wantLookups || plans[0].SavesDOIAndAbstract != tc.wantSaves {
				t.Fatalf("lookups = %d saves = %v, want %d and %v", *lookups, plans[0].SavesDOIAndAbstract, tc.wantLookups, tc.wantSaves)
			}
		})
	}
}

// A desktop that saved the item but could not file it answers "conflict" with
// committed evidence. The item exists, so a later pass must never create it
// again. The mirror has not seen it yet, so zotio's resolver would still call
// the paper new, and a second create would put a duplicate paper in the
// library. papio refuses every create for the job until it can name the
// committed item, then attaches the PDF to that item and runs the follow-ups.
func TestCommittedConflictAttachesToTheCommittedItemAndNeverCreatesAgain(t *testing.T) {
	cli := &planCLI{
		manifest: newItemManifest, preview: saveTimePreview, collections: readingCollections,
		apply: filingConflictApply, applyErr: errMutationIncomplete,
	}
	service, jobID, _ := saveTimeService(t, cli)
	if status, _, _, err := service.PlanAndApply(context.Background(), jobID); err == nil || status != "failed" {
		t.Fatalf("conflicted import = (%q, %v), want a failure", status, err)
	}
	if cli.applyCalls != 1 || cli.collectionCalls != 0 || cli.enrichCalls != 0 {
		t.Fatalf("after conflict: apply=%d add-to-collection=%d enrich=%d, want 1, 0, 0", cli.applyCalls, cli.collectionCalls, cli.enrichCalls)
	}
	events, err := service.Bundle.Jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if detail["with_import"] == true || detail["status"] == filingDeferred {
			t.Fatalf("a conflict recorded %s %#v", event["kind"], detail)
		}
	}

	// The desktop has the item, but the mirror does not show it yet, so the
	// resolver still answers "new". Nothing may be created.
	cli.apply, cli.applyErr = connectorApplied, nil
	for pass := 0; pass < 2; pass++ {
		if status, _, _, err := service.PlanAndApply(context.Background(), jobID); err == nil || status != "failed" {
			t.Fatalf("pass %d before the item is visible = (%q, %v), want a failure", pass, status, err)
		}
	}
	if cli.applyCalls != 1 {
		t.Fatalf("zotio apply ran %d times, want 1: an unreconciled committed create was created again", cli.applyCalls)
	}

	// Once the mirror shows the committed item, the PDF goes to that item.
	cli.found = `[{"key":"PA12RE34","data":{"key":"PA12RE34","itemType":"journalArticle"}}]`
	cli.apply = `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"PA12RE34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`
	status, parentKey, _, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" {
		t.Fatalf("attach after conflict = (%q, %q, %v)", status, parentKey, err)
	}
	if cli.applyCalls != 2 || !slices.Contains(cli.applyArgs, "attachments") || !slices.Contains(cli.applyArgs, "PA12RE34") {
		t.Fatalf("apply calls = %d, last apply = %v, want the PDF attached to PA12RE34", cli.applyCalls, cli.applyArgs)
	}
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("follow-ups after attaching to the committed parent: add-to-collection=%d enrich=%d, want 1 each", cli.collectionCalls, cli.enrichCalls)
	}
	if filing, _ := latestZotioEvent(t, service, jobID, followUpCollectionFiling); filing["status"] != "applied" || filing["with_import"] != nil {
		t.Fatalf("collection filing event = %#v, want the follow-up's", filing)
	}
}

// A collection list that ignores --start answers every page with the first
// page. The repeated page adds no key, which used to read as the end of the
// list, so a name duplicated on a later page looked unique and the item could
// be filed into the wrong collection.
func TestCollectionKeyByNameGivesUpWhenPagesRepeat(t *testing.T) {
	rows := make([]string, 0, collectionPageSize)
	rows = append(rows, `{"key":"RD12NG34","data":{"key":"RD12NG34","name":"Reading"}}`)
	for i := range collectionPageSize - 1 {
		key := fmt.Sprintf("OT%06d", i+1)
		rows = append(rows, `{"key":"`+key+`","data":{"key":"`+key+`","name":"Other `+key+`"}}`)
	}
	cli := &planCLI{collections: `{"meta":{"source":"live"},"results":[` + strings.Join(rows, ",") + `]}`}
	service := &Service{CLI: cli}
	if key := service.collectionKeyByName(context.Background(), "Reading"); key != "" {
		t.Fatalf("collection key = %q from a list whose pages repeat, want none", key)
	}
}

// A collection that the Web API has but Zotero desktop has not synced yet has
// no desktop target, so zotio refuses the save before writing anything. papio
// records that filing moves to after the import, plans again without the
// collection, and imports the paper in the same pass.
func TestSaveTimeFilingFallsBackWhenDesktopLacksCollection(t *testing.T) {
	cli := &planCLI{manifest: newItemManifest, preview: saveTimePreview, collections: readingCollections}
	cli.applyFn = func(context.Context) (json.RawMessage, error) {
		if cli.applyCalls == 1 {
			return json.RawMessage(targetMissingApply), errMutationIncomplete
		}
		return json.RawMessage(connectorApplied), nil
	}
	service, jobID := readyPlanService(t, "", cli)
	enablePlanAutoImport(t, service, jobID, "Reading")

	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" || attachmentKey != "AT56CH90" {
		t.Fatalf("import = (%q, %q, %q, %v), want applied after the fallback", status, parentKey, attachmentKey, err)
	}
	if cli.applyCalls != 2 || cli.resolveCalls != 2 || cli.collectionLists != 1 {
		t.Fatalf("apply=%d resolve=%d collection lists=%d, want 2, 2, 1", cli.applyCalls, cli.resolveCalls, cli.collectionLists)
	}
	if _, ok := onlyManifestItem(t, service)["collections"]; ok {
		t.Fatal("the fallback manifest still names the collection the desktop lacks")
	}
	if cli.collectionCalls != 1 || !slices.Contains(cli.collectionArgs, "Reading") {
		t.Fatalf("follow-up filing calls = %d args = %v, want one for Reading", cli.collectionCalls, cli.collectionArgs)
	}
	deferred := zotioEventDetail(t, service, jobID, followUpCollectionFiling)
	if deferred["status"] != filingDeferred || deferred["reason"] != desktopTargetMissingReason ||
		deferred["collection"] != "Reading" || deferred["collection_key"] != "RD12NG34" {
		t.Fatalf("first filing event = %#v, want the recorded deferral", deferred)
	}
	if filing, _ := latestZotioEvent(t, service, jobID, followUpCollectionFiling); filing["status"] != "applied" || filing["with_import"] != nil {
		t.Fatalf("latest filing event = %#v, want the follow-up's", filing)
	}
}

// A name that matches no collection, or more than one, leaves filing to the
// follow-up, which creates a missing collection and has always picked among
// duplicates by itself.
func TestSaveTimeFilingLeavesUnresolvedNamesToFollowUp(t *testing.T) {
	for name, collections := range map[string]string{
		"absent":    `{"meta":{"source":"live"},"results":[{"key":"OT56HR78","data":{"key":"OT56HR78","name":"Other"}}]}`,
		"ambiguous": `{"meta":{"source":"live"},"results":[{"key":"RD12NG34","data":{"key":"RD12NG34","name":"Reading"}},{"key":"RD56NG78","data":{"key":"RD56NG78","name":"Reading","parentCollection":"OT56HR78"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			cli := &planCLI{manifest: newItemManifest, preview: saveTimePreview, apply: connectorApplied, collections: collections}
			service, jobID := readyPlanService(t, "", cli)
			enablePlanAutoImport(t, service, jobID, "Reading")
			if status, _, _, err := service.PlanAndApply(context.Background(), jobID); err != nil || status != "applied" {
				t.Fatalf("import = (%q, %v)", status, err)
			}
			if _, ok := onlyManifestItem(t, service)["collections"]; ok {
				t.Fatal("manifest names a collection the lookup could not pin down")
			}
			if cli.collectionCalls != 1 {
				t.Fatalf("follow-up filing calls = %d, want 1", cli.collectionCalls)
			}
		})
	}
}

// Only a target zotio could not resolve moves filing after the import. A
// closed desktop is Zotero-not-running, which waits instead, and a conflict
// has already written.
func TestDesktopTargetMissingNamesOnlyTargetRefusals(t *testing.T) {
	failed := func(reason string) string {
		encoded, _ := json.Marshal(reason)
		return `{"ok":false,"mode":"apply","result":{"summary":{"applied":0,"conflicts":0,"failed":1},"items":[{"status":"failed","reason":` + string(encoded) + `}]}}`
	}
	for name, tc := range map[string]struct {
		out  string
		want bool
	}{
		"no target matched":  {targetMissingApply, true},
		"not on the desktop": {failed("collection key RD12NG34 was not found in the live Zotero collection list"), true},
		"ambiguous target":   {failed(`collection RD12NG34 maps to ambiguous connector path "Reading" (C1, C2); pass --connector-target C<n>`), true},
		"desktop closed":     {failed("--via connector set but desktop Zotero is not reachable on :23119: connection refused"), false},
		"committed conflict": {filingConflictApply, false},
		"no envelope":        {"", false},
	} {
		if got := desktopTargetMissing(json.RawMessage(tc.out)); got != tc.want {
			t.Errorf("%s: desktopTargetMissing = %v, want %v", name, got, tc.want)
		}
	}
}

func anyStrings(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, v := range values {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}
