// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package identitycorpus

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// backlogStore opens a store over an already-migrated data directory.
// storetest.DataDir is mandatory here: re-running the migrations costs ~0.6s
// per test against ~0.045s for opening a seeded file, and this package now
// builds a store in several tests.
func backlogStore(t *testing.T) (string, *job.Store, func()) {
	t.Helper()
	dir := storetest.DataDir(t)
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	closed := false
	closeStore := func() {
		if closed {
			return
		}
		closed = true
		if err := db.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	}
	t.Cleanup(closeStore)
	return dir, &job.Store{S: db}, closeStore
}

// seedEligible drives one job into exactly the state the auto-bind pool
// predicate selects on: awaiting_human with an open manual_download action.
// It goes through the real transitions rather than INSERTing rows so the test
// measures job.ListCandidateEligibleJobs against state the daemon can actually
// produce.
func seedEligible(t *testing.T, js *job.Store, requestID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	pol := job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "test", FetchMaxBytes: 1 << 20}
	id, err := js.CreateRequest(ctx, requestID, w, "", "", pol, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest %s: %v", requestID, err)
	}
	if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatalf("%s to resolving: %v", requestID, err)
	}
	if err := js.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatalf("%s to awaiting_human: %v", requestID, err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "please download", job.Access(false, "")); err != nil {
		t.Fatalf("%s open manual_download: %v", requestID, err)
	}
	return id
}

// seedIneligible creates a job that is awaiting a human for some other reason.
// It exists to prove the arm inherits the pool predicate rather than counting
// every pending job.
func seedIneligible(t *testing.T, js *job.Store, requestID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	pol := job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "test", FetchMaxBytes: 1 << 20}
	id, err := js.CreateRequest(ctx, requestID, w, "", "", pol, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest %s: %v", requestID, err)
	}
	if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatalf("%s to resolving: %v", requestID, err)
	}
	if err := js.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatalf("%s to awaiting_human: %v", requestID, err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "document_delivery", "ask a librarian", job.Access(false, "")); err != nil {
		t.Fatalf("%s open document_delivery: %v", requestID, err)
	}
	return id
}

// doiLessText is front matter with no DOI anywhere in it, so the document
// passes the production admission condition (FrontMatterDOIs empty).
func doiLessText(title string) string {
	return title + "\nA. Author, B. Author\nJournal of Measurement, 2024\n\nAbstract. Nothing here prints an identifier.\n"
}

func truthFor(t *testing.T, arm BacklogArm, docKey string) BacklogTruth {
	t.Helper()
	for _, tr := range arm.Truths {
		if tr.DocKey == docKey {
			return tr
		}
	}
	t.Fatalf("no truth recorded for %q; truths = %+v", docKey, arm.Truths)
	return BacklogTruth{}
}

func poolFor(t *testing.T, arm BacklogArm, docKey string) Pool {
	t.Helper()
	for _, p := range arm.Pools {
		if p.DocKey == docKey {
			return p
		}
	}
	t.Fatalf("no pool built for %q; pools = %+v", docKey, arm.Pools)
	return Pool{}
}

func hasPool(arm BacklogArm, docKey string) bool {
	for _, p := range arm.Pools {
		if p.DocKey == docKey {
			return true
		}
	}
	return false
}

// The pool must come from the daemon's own enumeration, carry every eligible
// job and nothing else, and be identical for every document — attemptAutoBind
// hands the selector the whole list.
func TestBacklogArmBuildsPoolsFromEligibilityEnumeration(t *testing.T) {
	_, js, _ := backlogStore(t)
	ctx := context.Background()

	wanted := seedEligible(t, js, "wr_backlog_wanted", work.Work{DOI: "10.1000/Wanted", Title: "Wanted paper", Year: 2024})
	other := seedEligible(t, js, "wr_backlog_other", work.Work{DOI: "10.1000/other", Title: "Other paper", Year: 2023})
	ignored := seedIneligible(t, js, "wr_backlog_ignored", work.Work{DOI: "10.1000/ignored", Title: "Ignored paper", Year: 2022})

	docs := []Document{
		// Identifier spelled as a doi.org URL with different case: raw
		// comparison misses this, ownership.NormalizeIdentifier does not.
		{Key: "DOCWANTED", Work: work.Work{DOI: "https://doi.org/10.1000/wanted", Title: "Wanted paper"}, Text: doiLessText("Wanted paper")},
		// Excluded by the production admission condition: a document whose
		// front matter prints a DOI never reaches candidate selection.
		{Key: "DOCFRONT", Work: work.Work{DOI: "10.1000/other"}, Text: "Other paper\nDOI: 10.1000/other published 2023\n"},
	}

	arm, err := BuildBacklogArm(ctx, js, docs)
	if err != nil {
		t.Fatalf("BuildBacklogArm: %v", err)
	}

	if arm.PendingRows != 2 {
		t.Fatalf("PendingRows = %d, want 2 (the ineligible job %s must not be enumerated)", arm.PendingRows, ignored)
	}
	if arm.LibraryDocuments != 2 || arm.DOILessDocuments != 1 {
		t.Fatalf("admission split = %d library / %d doi-less, want 2 / 1", arm.LibraryDocuments, arm.DOILessDocuments)
	}
	if hasPool(arm, "DOCFRONT") {
		t.Fatalf("built a pool for a document with a front-matter DOI; production never reaches selection for it")
	}

	pool := poolFor(t, arm, "DOCWANTED")
	if len(pool.Candidates) != 2 {
		t.Fatalf("pool candidates = %d, want the whole enumeration (2)", len(pool.Candidates))
	}
	gotKeys := []string{pool.Candidates[0].Key, pool.Candidates[1].Key}
	if gotKeys[0] != wanted || gotKeys[1] != other {
		t.Fatalf("candidate keys = %v, want enumeration order [%s %s]", gotKeys, wanted, other)
	}
	if len(pool.TrueKeys) != 1 || pool.TrueKeys[0] != wanted {
		t.Fatalf("TrueKeys = %v, want [%s]", pool.TrueKeys, wanted)
	}
	if pool.TargetAbsent {
		t.Fatalf("TargetAbsent = true for a document whose paper is in the pool")
	}
	if pool.Provenance != "identifier" {
		t.Fatalf("Provenance = %q, want %q", pool.Provenance, "identifier")
	}
	if arm.TargetPresent != 1 || arm.TargetAbsent != 0 || arm.Unestablished != 0 {
		t.Fatalf("split = %d present / %d absent / %d unestablished, want 1/0/0", arm.TargetPresent, arm.TargetAbsent, arm.Unestablished)
	}
	if got := arm.PoolSizes[2]; got != 1 {
		t.Fatalf("PoolSizes = %v, want one pool observed at size 2", arm.PoolSizes)
	}
	if basis := truthFor(t, arm, "DOCWANTED").Basis; basis != BasisCanonicalIdentifier {
		t.Fatalf("basis = %q, want %q", basis, BasisCanonicalIdentifier)
	}
}

// A document whose paper is genuinely not pending gets an EMPTY class with
// TargetAbsent set, which is a measurable trial (the correct outcome is
// abstain) and must not be confused with an unresolvable one.
func TestBacklogArmTargetAbsentWhenPaperNotInPool(t *testing.T) {
	_, js, _ := backlogStore(t)
	ctx := context.Background()

	seedEligible(t, js, "wr_absent_a", work.Work{DOI: "10.1000/pending-a", Title: "Pending A"})
	seedEligible(t, js, "wr_absent_b", work.Work{ArXiv: "arXiv:2401.00001v2", Title: "Pending B"})

	docs := []Document{
		{Key: "DOCELSEWHERE", Work: work.Work{DOI: "10.1000/not-pending", Title: "Some other paper"}, Text: doiLessText("Some other paper")},
		// arXiv v1 against a pending v2: the version-collapsing relation makes
		// this the SAME work, so it must not read as absent.
		{Key: "DOCARXIV", Work: work.Work{ArXiv: "2401.00001v1", Title: "Pending B"}, Text: doiLessText("Pending B")},
	}

	arm, err := BuildBacklogArm(ctx, js, docs)
	if err != nil {
		t.Fatalf("BuildBacklogArm: %v", err)
	}

	absent := poolFor(t, arm, "DOCELSEWHERE")
	if len(absent.TrueKeys) != 0 {
		t.Fatalf("TrueKeys = %v, want empty for an absent target", absent.TrueKeys)
	}
	if !absent.TargetAbsent {
		t.Fatalf("TargetAbsent = false; an empty class without this flag is read as unresolvable and dropped")
	}
	if basis := truthFor(t, arm, "DOCELSEWHERE").Basis; basis != BasisEstablishedAbsent {
		t.Fatalf("basis = %q, want %q", basis, BasisEstablishedAbsent)
	}

	matched := poolFor(t, arm, "DOCARXIV")
	if len(matched.TrueKeys) != 1 || matched.TargetAbsent {
		t.Fatalf("arXiv v1/v2 pair scored as %v absent=%v; NormalizeIdentifier collapses the version suffix", matched.TrueKeys, matched.TargetAbsent)
	}
	if arm.TargetAbsent != 1 || arm.TargetPresent != 1 {
		t.Fatalf("split = %d present / %d absent, want 1/1", arm.TargetPresent, arm.TargetAbsent)
	}
}

// Correspondence that cannot be established is excluded and counted, never
// guessed into either arm. Two independent ways it fails, and the second is
// the subtle one: an eligible job with no canonicalizable identifier could be
// ANY paper, so absence cannot be concluded for any document.
func TestBacklogArmExcludesUnestablishedCorrespondence(t *testing.T) {
	_, js, _ := backlogStore(t)
	ctx := context.Background()

	seedEligible(t, js, "wr_unest_named", work.Work{DOI: "10.1000/named", Title: "Named paper"})
	// ISBN passes work.HasIdentifier but ownership.NormalizeIdentifier refuses
	// it, so this job is opaque to the only admissible evidence.
	seedEligible(t, js, "wr_unest_opaque", work.Work{ISBN: "9780000000001", Title: "Opaque book chapter"})

	docs := []Document{
		{Key: "DOCNOID", Work: work.Work{Title: "A document with no strong identifier"}, Text: doiLessText("A document with no strong identifier")},
		{Key: "DOCUNMATCHED", Work: work.Work{DOI: "10.1000/unmatched", Title: "Unmatched"}, Text: doiLessText("Unmatched")},
		{Key: "DOCNAMED", Work: work.Work{DOI: "10.1000/named", Title: "Named paper"}, Text: doiLessText("Named paper")},
	}

	arm, err := BuildBacklogArm(ctx, js, docs)
	if err != nil {
		t.Fatalf("BuildBacklogArm: %v", err)
	}

	if hasPool(arm, "DOCNOID") {
		t.Fatalf("built a pool for a document with no canonicalizable identifier")
	}
	if hasPool(arm, "DOCUNMATCHED") {
		t.Fatalf("built a pool for a document whose absence cannot be established while an opaque job is pending")
	}
	if arm.Unestablished != 2 {
		t.Fatalf("Unestablished = %d, want 2", arm.Unestablished)
	}
	if arm.TargetAbsent != 0 {
		t.Fatalf("TargetAbsent = %d, want 0; an unresolvable document must never be guessed into the absent arm", arm.TargetAbsent)
	}
	if basis := truthFor(t, arm, "DOCNOID").Basis; basis != BasisUnestablishedNoDocID {
		t.Fatalf("DOCNOID basis = %q, want %q", basis, BasisUnestablishedNoDocID)
	}
	if basis := truthFor(t, arm, "DOCUNMATCHED").Basis; basis != BasisUnestablishedOpaqueJob {
		t.Fatalf("DOCUNMATCHED basis = %q, want %q", basis, BasisUnestablishedOpaqueJob)
	}
	// The one resolvable document still scores, so exclusion is per document
	// and not a whole-arm bail-out.
	if !hasPool(arm, "DOCNAMED") || arm.TargetPresent != 1 {
		t.Fatalf("resolvable document dropped: pools = %+v", arm.Pools)
	}

	rendered := arm.Render()
	for _, want := range []string{string(BasisUnestablishedNoDocID), string(BasisUnestablishedOpaqueJob)} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render omits the exclusion reason %q:\n%s", want, rendered)
		}
	}
}

// The operator's live database is read, never written: papio.db itself is
// byte-identical afterwards, a write attempted through the arm's own handle is
// refused by the driver, and the only files the run leaves behind are the
// empty -wal/-shm sidecars SQLite recreates in order to READ a WAL-mode
// database (see OpenBacklogEligibility). That last part is pinned rather than
// forbidden because it is what actually happens and an operator deserves to
// find it asserted somewhere rather than discovered.
func TestBacklogEligibilityDoesNotWriteTheStore(t *testing.T) {
	dir, js, closeStore := backlogStore(t)
	ctx := context.Background()
	seedEligible(t, js, "wr_ro_one", work.Work{DOI: "10.1000/read-only", Title: "Read only"})
	closeStore()

	dbPath := filepath.Join(dir, "papio.db")
	before := dirDigest(t, dir)

	elig, err := OpenBacklogEligibility(dir)
	if err != nil {
		t.Fatalf("OpenBacklogEligibility: %v", err)
	}
	defer func() { _ = elig.Close() }()

	arm, err := BuildBacklogArm(ctx, elig, []Document{
		{Key: "DOCRO", Work: work.Work{DOI: "10.1000/read-only"}, Text: doiLessText("Read only")},
	})
	if err != nil {
		t.Fatalf("BuildBacklogArm over the read-only handle: %v", err)
	}
	if arm.PendingRows != 1 || len(arm.Pools) != 1 {
		t.Fatalf("read-only enumeration returned %d rows / %d pools, want 1 / 1", arm.PendingRows, len(arm.Pools))
	}
	if arm.StorePath != dbPath {
		t.Fatalf("StorePath = %q, want %q", arm.StorePath, dbPath)
	}

	// A write through the very handle the arm used must be refused by the
	// driver, not merely avoided by this package's own discipline.
	if _, err := elig.db.ExecContext(ctx, `UPDATE jobs SET state = 'ready'`); err == nil {
		t.Fatalf("UPDATE through the read-only handle succeeded; mode=ro is not in force")
	} else if !strings.Contains(strings.ToLower(err.Error()), "readonly") && !strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Fatalf("UPDATE failed with %v, want a read-only refusal", err)
	}

	after := dirDigest(t, dir)
	if after["papio.db"] != before["papio.db"] {
		t.Fatalf("papio.db changed under a read-only run")
	}
	delete(after, "papio.db")
	delete(before, "papio.db")
	if len(before) != 0 {
		t.Fatalf("precondition: the closed store left %v behind, so the sidecar assertion below is not measuring this run", before)
	}
	// sha256 of zero bytes: the -wal SQLite creates to read the database holds
	// no frames, which is the evidence nothing was written through it.
	const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for name, digest := range after {
		switch name {
		case "papio.db-wal":
			if digest != emptyDigest {
				t.Fatalf("read-only run left a non-empty %s: the handle wrote frames", name)
			}
		case "papio.db-shm":
			// Shared-memory index, rebuilt from the database on every open.
		default:
			t.Fatalf("read-only run created %s in the operator's data directory", name)
		}
	}
}

// dirDigest snapshots every file in dir by content, so a new sidecar, a
// removed one, or one byte rewritten anywhere all show up as a difference.
func dirDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return out
}

// The rendered section must be unreadable as a production rate: the caveat is
// present, every count row is marked, the observed distribution is reported
// with its degeneracy stated, and nothing derives a rate.
func TestBacklogArmRenderCarriesDescriptiveCaveatAndObservedDistribution(t *testing.T) {
	_, js, _ := backlogStore(t)
	ctx := context.Background()

	seedEligible(t, js, "wr_render_a", work.Work{DOI: "10.1000/render-a", Title: "Render A"})
	seedEligible(t, js, "wr_render_b", work.Work{DOI: "10.1000/render-b", Title: "Render B"})
	seedEligible(t, js, "wr_render_c", work.Work{DOI: "10.1000/render-c", Title: "Render C"})

	arm, err := BuildBacklogArm(ctx, js, []Document{
		{Key: "DOCRA", Work: work.Work{DOI: "10.1000/render-a"}, Text: doiLessText("Render A")},
		{Key: "DOCRB", Work: work.Work{DOI: "10.1000/render-b"}, Text: doiLessText("Render B")},
	})
	if err != nil {
		t.Fatalf("BuildBacklogArm: %v", err)
	}
	if arm.PendingRows != 3 {
		t.Fatalf("PendingRows = %d, want 3", arm.PendingRows)
	}
	if len(arm.PoolSizes) != 1 || arm.PoolSizes[3] != 2 {
		t.Fatalf("PoolSizes = %v, want {3: 2} — every pool is the whole enumeration", arm.PoolSizes)
	}

	rendered := arm.Render()
	if !strings.Contains(rendered, BacklogCaveat) {
		t.Fatalf("render omits BacklogCaveat:\n%s", rendered)
	}
	if !strings.Contains(rendered, "observed pool-size distribution") {
		t.Fatalf("render omits the observed pool-size distribution:\n%s", rendered)
	}
	if !strings.Contains(rendered, "pending rows returned by the eligibility enumeration") {
		t.Fatalf("render omits the pending-row count:\n%s", rendered)
	}
	if !strings.Contains(rendered, "not measured at the {2,5,10,25} pool sizes") {
		t.Fatalf("render does not state that this arm is exempt from the pool-size sweep:\n%s", rendered)
	}
	// A reader comparing this section with the measurement loop's table meets
	// two different quantities both called "unestablished". The render must
	// say so, or one of the two sections looks wrong.
	if !strings.Contains(rendered, "different quantity from ArmResult.Unestablished") {
		t.Fatalf("render does not distinguish this arm's exclusion tally from the loop's column:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Do not add the two.") {
		t.Fatalf("render does not forbid summing the two unestablished counts:\n%s", rendered)
	}
	// Every count-bearing row carries the marker, so one line copied out of
	// context still says what it is. tabwriter has already replaced the tabs
	// with padding by this point, so the rows are matched by label and by
	// trailing marker rather than by column.
	marked := 0
	var lines []string
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimRight(line, " ")
		if strings.HasSuffix(trimmed, backlogRowMarker) {
			marked++
		}
		lines = append(lines, trimmed)
	}
	for _, label := range []string{
		"pending rows returned by the eligibility enumeration",
		"library documents loaded",
		"doi-less documents admitted",
		"pools built",
		"target present (equivalence class established)",
		"target absent (absence established, empty class)",
		"unestablished (excluded, never guessed into an arm)",
	} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, label) {
				found = true
				if !strings.HasSuffix(line, backlogRowMarker) {
					t.Fatalf("row %q does not carry %q", line, backlogRowMarker)
				}
			}
		}
		if !found {
			t.Fatalf("render omits the row %q:\n%s", label, rendered)
		}
	}
	// Seven count rows plus the one observed pool-size row.
	if marked != 8 {
		t.Fatalf("marked rows = %d, want 8:\n%s", marked, rendered)
	}

	// No rate is derived anywhere. The caveat itself names the rate it forbids,
	// so it is removed before the check, and the absence of any percentage is
	// asserted structurally.
	body := strings.ReplaceAll(rendered, BacklogCaveat, "")
	if strings.Contains(body, "wrong-bind rate") || strings.Contains(body, "rate") {
		t.Fatalf("render states a rate for a descriptive arm:\n%s", rendered)
	}
	if strings.Contains(body, "%") {
		t.Fatalf("render prints a percentage for a descriptive arm:\n%s", rendered)
	}
}

// An empty queue produces no pools rather than a pool of size zero per
// document, which would fill the arm with correct-abstain rows that test
// nothing.
func TestBacklogArmEmptyQueueBuildsNoPools(t *testing.T) {
	_, js, _ := backlogStore(t)

	arm, err := BuildBacklogArm(context.Background(), js, []Document{
		{Key: "DOCX", Work: work.Work{DOI: "10.1000/x"}, Text: doiLessText("X")},
	})
	if err != nil {
		t.Fatalf("BuildBacklogArm: %v", err)
	}
	if arm.PendingRows != 0 || len(arm.Pools) != 0 {
		t.Fatalf("empty queue produced %d rows / %d pools, want 0 / 0", arm.PendingRows, len(arm.Pools))
	}
	if !strings.Contains(arm.Render(), "An empty queue is not a clean result") {
		t.Fatalf("render lets an empty queue read as a clean result:\n%s", arm.Render())
	}
}

// The corpus extracts with pdf.DefaultSemanticOptions while the daemon runs
// config.Default().PDF. That divergence must be reported, not assumed away.
func TestBacklogExtractionNoteReportsDaemonDivergence(t *testing.T) {
	note := backlogExtractionNote()
	for _, want := range []string{"EXTRACTION DIVERGES", "MinChars 1000", "min_text_chars 400", "OCRPages 3", "max_ocr_pages 4", "ocr_enabled true"} {
		if !strings.Contains(note, want) {
			t.Fatalf("extraction note missing %q:\n%s", want, note)
		}
	}
}
