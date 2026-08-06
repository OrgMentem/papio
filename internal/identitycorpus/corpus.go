// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package identitycorpus builds the identity-rule measurement corpus from the
// operator's live Zotero library: (PDF, curated metadata) pairs that
// pdf.MatchIdentity is scored against in measure.go. Zotero is the source
// rather than papio's own store because papio's store is both small and
// biased — 172 acquired PDFs whose metadata is whatever papio itself
// requested, frequently with author lists truncated to a single name, which
// is exactly the axis the author rules are measured on. Zotero holds a much
// larger, independently curated library with complete creator lists, so
// scoring against it measures the rules rather than papio's own requests.
package identitycorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"papio/internal/pdf"
	"papio/internal/work"
)

// Document is one PDF paired with the curated metadata Zotero holds for its
// parent bibliographic item, plus the text a caller feeds to
// pdf.MatchIdentity.
type Document struct {
	Key         string    // Zotero attachment item key
	ParentKey   string    // Zotero parent (bibliographic) item key
	Path        string    // absolute path to the PDF
	Work        work.Work // curated metadata
	Text        string    // pdf.TextReport.Excerpt — exactly what identity reads
	Chars       int64
	OCRUsed     bool
	NeedsReview bool
}

// Skip records a candidate PDF attachment that Load excluded from the
// corpus, and why, so a caller can report how many of Zotero's attachments
// ended up measured versus filtered out and by which rule.
type Skip struct {
	Key    string
	Reason string
}

// scholarlyItemTypes are the Zotero item types worth measuring identity
// against: everything that carries its own title and, ordinarily, an author
// list. An attachment whose parent has some other type (a note, a webpage
// snapshot, a standalone file with no bibliographic parent at all) was never
// a candidate for a (PDF, curated metadata) pair — there is no curated work
// to pair it with — so excluding it here is not tracked as a Skip; only a
// candidate that reaches Load's later checks and fails one is.
var scholarlyItemTypes = []string{
	"journalArticle", "conferencePaper", "preprint", "bookSection", "book",
	"report", "thesis", "magazineArticle", "newspaperArticle",
}

var (
	pmidExtraPattern   = regexp.MustCompile(`(?i)PMID:\s*(\d+)`)
	arxivExtraPattern  = regexp.MustCompile(`(?i)arXiv:\s*([^\s,;]+)`)
	leadingYearPattern = regexp.MustCompile(`^\d{4}`)
)

// candidate is one row of the query in queryCandidates: a PDF attachment
// whose parent is a scholarly item, before any of Load's filtering runs.
type candidate struct {
	attachmentKey string
	attachmentID  int64
	parentKey     string
	parentID      int64
	linkMode      int
	path          string
}

// prepared is a candidate that has cleared file resolution and metadata
// decidability, waiting only on text extraction — the one step Load runs
// concurrently, since it is the only step slow enough to matter.
type prepared struct {
	cand candidate
	path string
	work work.Work
	info os.FileInfo
}

// Load reads the operator's Zotero library at zoteroDir and returns curated
// (PDF, metadata) pairs suitable for scoring pdf.MatchIdentity, plus a
// record of every candidate that did not make it in. cacheDir, when
// non-empty, caches extracted text across runs; workers bounds extraction
// concurrency, defaulting to runtime.NumCPU() when <= 0.
func Load(ctx context.Context, zoteroDir, cacheDir string, workers int) ([]Document, []Skip, error) {
	capability := pdf.DetectCapability()
	if capability.PDFToText == "" {
		return nil, nil, errors.New("poppler's pdftotext is not installed; identity corpus extraction requires it")
	}

	copyDir, err := copyZoteroDatabase(zoteroDir)
	if err != nil {
		return nil, nil, err
	}
	// Zotero keeps zotero.sqlite open in WAL mode for as long as the
	// application runs. A WAL reader needs the -wal and -shm files that sit
	// beside it, and those only exist — and only stay mutually consistent —
	// while Zotero's own handle is open, so opening zoteroDir/zotero.sqlite
	// directly, even read-only, either blocks behind Zotero's writer lock or
	// risks a torn read mid-checkpoint. Copying all three files into a
	// private scratch directory first, then opening the copy, sidesteps
	// both: the copy is a point-in-time snapshot nothing else ever touches.
	// A future reader who "simplifies" this back into opening the live file
	// in place will reintroduce exactly the lock contention this exists to
	// avoid.
	defer os.RemoveAll(copyDir)

	dsn := "file:" + filepath.Join(copyDir, "zotero.sqlite") + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("opening copied zotero.sqlite: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("opening copied zotero.sqlite: %w", err)
	}

	candidates, err := queryCandidates(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	kept, skips := dedupOnePerParent(candidates)

	var prep []prepared
	for _, c := range kept {
		path, err := resolveAttachmentPath(zoteroDir, c)
		if err != nil {
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: err.Error()})
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: fmt.Sprintf("file not found: %v", err)})
			continue
		}
		w, err := buildWork(ctx, db, c.parentID)
		if err != nil {
			return nil, nil, err
		}
		// A parent with neither a title nor any strong identifier gives
		// pdf.MatchIdentity nothing to reach a verdict on — the rules can
		// only reject or park it, never because of anything they got
		// wrong. Scoring it would dilute the pass/review/reject counts
		// with a result that says nothing about the rules under test.
		if w.Title == "" && w.DOI == "" && w.PMID == "" && w.ArXiv == "" {
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: "no title or identifier"})
			continue
		}
		prep = append(prep, prepared{cand: c, path: path, work: w, info: info})
	}

	docs, extractSkips, err := extractAll(ctx, prep, capability, cacheDir, workers)
	if err != nil {
		return nil, nil, err
	}
	skips = append(skips, extractSkips...)

	sort.Slice(docs, func(i, j int) bool { return docs[i].Key < docs[j].Key })
	sort.Slice(skips, func(i, j int) bool { return skips[i].Key < skips[j].Key })
	return docs, skips, nil
}

// copyZoteroDatabase copies zotero.sqlite, and its -wal/-shm siblings when
// present, into a fresh temporary directory and returns that directory. The
// caller owns removing it; see the comment in Load on why the copy exists at
// all.
func copyZoteroDatabase(zoteroDir string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "papio-identitycorpus-*")
	if err != nil {
		return "", fmt.Errorf("creating scratch dir for zotero.sqlite copy: %w", err)
	}
	if err := copyFile(filepath.Join(zoteroDir, "zotero.sqlite"), filepath.Join(tmpDir, "zotero.sqlite")); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	// zotero.sqlite-wal and zotero.sqlite-shm only exist while Zotero holds
	// the database open in WAL mode, so their absence here is the normal
	// case (Zotero not running, or fully checkpointed) rather than an error.
	for _, name := range []string{"zotero.sqlite-wal", "zotero.sqlite-shm"} {
		src := filepath.Join(zoteroDir, name)
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		if err := copyFile(src, filepath.Join(tmpDir, name)); err != nil {
			os.RemoveAll(tmpDir)
			return "", err
		}
	}
	return tmpDir, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	return nil
}

// queryCandidates selects one row per PDF attachment whose parent is a
// scholarly item, excluding anything either the attachment or the parent has
// marked deleted. Everything this query filters out was never a candidate
// pairing to begin with, which is why none of it is recorded as a Skip —
// Skip is reserved for a row that made it this far and then failed a later,
// per-candidate check.
func queryCandidates(ctx context.Context, db *sql.DB) ([]candidate, error) {
	placeholders := make([]string, len(scholarlyItemTypes))
	args := make([]any, len(scholarlyItemTypes))
	for i, t := range scholarlyItemTypes {
		placeholders[i] = "?"
		args[i] = t
	}
	query := fmt.Sprintf(`
		SELECT att.key, att.itemID, par.key, par.itemID, ia.linkMode, ia.path
		FROM itemAttachments ia
		JOIN items att ON att.itemID = ia.itemID
		JOIN items par ON par.itemID = ia.parentItemID
		JOIN itemTypes it ON it.itemTypeID = par.itemTypeID
		WHERE ia.contentType = 'application/pdf'
		  AND ia.parentItemID IS NOT NULL
		  AND att.itemID NOT IN (SELECT itemID FROM deletedItems)
		  AND par.itemID NOT IN (SELECT itemID FROM deletedItems)
		  AND it.typeName IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying candidate attachments: %w", err)
	}
	defer rows.Close()

	var candidates []candidate
	for rows.Next() {
		var c candidate
		var path sql.NullString
		if err := rows.Scan(&c.attachmentKey, &c.attachmentID, &c.parentKey, &c.parentID, &c.linkMode, &path); err != nil {
			return nil, fmt.Errorf("scanning candidate attachment: %w", err)
		}
		c.path = path.String
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading candidate attachments: %w", err)
	}
	return candidates, nil
}

// dedupOnePerParent keeps exactly one PDF attachment per parent item. Seven
// parents in this library carry two — an article plus a supplement, or an
// alternate scan — and pairing the second against the parent's curated
// metadata would score a supplement's own front matter, which rarely
// repeats the paper's title or DOI, against metadata that describes the
// primary PDF instead. That is not a defect in pdf.MatchIdentity's rules; it
// is the harness asking a question the corpus was never meant to pose. The
// lowest attachment itemID is kept because Zotero assigns itemIDs on
// import, so the earliest-imported PDF is a deterministic, order-independent
// choice regardless of what order the rows arrive from SQLite in.
func dedupOnePerParent(candidates []candidate) ([]candidate, []Skip) {
	byParent := make(map[int64][]candidate, len(candidates))
	for _, c := range candidates {
		byParent[c.parentID] = append(byParent[c.parentID], c)
	}

	kept := make([]candidate, 0, len(candidates))
	var skips []Skip
	for _, group := range byParent {
		best := group[0]
		for _, c := range group[1:] {
			if c.attachmentID < best.attachmentID {
				best = c
			}
		}
		kept = append(kept, best)
		for _, c := range group {
			if c.attachmentID != best.attachmentID {
				skips = append(skips, Skip{Key: c.attachmentKey, Reason: "parent has another PDF attachment"})
			}
		}
	}
	return kept, skips
}

// resolveAttachmentPath turns a Zotero attachment's linkMode and stored path
// into a filesystem path. linkMode 3 (linked_url) points at a web page, not
// a local file, and any linkMode this library has not been observed to use
// is treated the same way — as a Skip — rather than guessed at.
func resolveAttachmentPath(zoteroDir string, c candidate) (string, error) {
	var path string
	switch c.linkMode {
	case 0, 1: // imported_file, imported_url: copied into Zotero's own storage.
		name := strings.TrimPrefix(c.path, "storage:")
		path = filepath.Join(zoteroDir, "storage", c.attachmentKey, name)
	case 2: // linked_file: relative to linked-attachments, or an absolute path.
		if rel, ok := strings.CutPrefix(c.path, "attachments:"); ok {
			path = filepath.Join(zoteroDir, "linked-attachments", rel)
		} else {
			path = c.path
		}
	default:
		return "", fmt.Errorf("linkMode %d has no local file", c.linkMode)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, nil
}

// buildWork assembles the curated work.Work a parent bibliographic item
// describes, from its itemData fields and its creator list.
func buildWork(ctx context.Context, db *sql.DB, parentID int64) (work.Work, error) {
	fields, err := itemFields(ctx, db, parentID)
	if err != nil {
		return work.Work{}, err
	}
	authors, err := itemAuthors(ctx, db, parentID)
	if err != nil {
		return work.Work{}, err
	}

	w := work.Work{
		Title:   fields["title"],
		DOI:     fields["DOI"],
		ISBN:    fields["ISBN"],
		Authors: authors,
	}
	if y := leadingYearPattern.FindString(fields["date"]); y != "" {
		if n, err := strconv.Atoi(y); err == nil {
			w.Year = n
		}
	}
	// PMID and arXiv ids are not their own Zotero fields; only 9 items in
	// this library carry either, and both live inside the free-text "extra"
	// field, so the only way to recover them is a case-insensitive scan.
	extra := fields["extra"]
	if m := pmidExtraPattern.FindStringSubmatch(extra); m != nil {
		w.PMID = m[1]
	}
	if m := arxivExtraPattern.FindStringSubmatch(extra); m != nil {
		w.ArXiv = strings.TrimSpace(m[1])
	}
	return w, nil
}

func itemFields(ctx context.Context, db *sql.DB, itemID int64) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT fields.fieldName, itemDataValues.value
		FROM itemData
		JOIN itemDataValues ON itemDataValues.valueID = itemData.valueID
		JOIN fields ON fields.fieldID = itemData.fieldID
		WHERE itemData.itemID = ?
	`, itemID)
	if err != nil {
		return nil, fmt.Errorf("querying item fields for item %d: %w", itemID, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("scanning item field for item %d: %w", itemID, err)
		}
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading item fields for item %d: %w", itemID, err)
	}
	return out, nil
}

// itemAuthors renders itemID's byline in Zotero's own creator order. It
// prefers creators typed "author" and only falls back to every creator, in
// order, when none carries that type — edited books route their whole
// byline through "editor" instead, and a name on the cover is a name in the
// byline whatever Zotero's creatorType enum happens to call its role.
func itemAuthors(ctx context.Context, db *sql.DB, itemID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT creators.firstName, creators.lastName, creators.fieldMode, creatorTypes.creatorType
		FROM itemCreators
		JOIN creators ON creators.creatorID = itemCreators.creatorID
		JOIN creatorTypes ON creatorTypes.creatorTypeID = itemCreators.creatorTypeID
		WHERE itemCreators.itemID = ?
		ORDER BY itemCreators.orderIndex
	`, itemID)
	if err != nil {
		return nil, fmt.Errorf("querying creators for item %d: %w", itemID, err)
	}
	defer rows.Close()

	type creator struct {
		first, last, kind string
		fieldMode         int
	}
	var all []creator
	for rows.Next() {
		var c creator
		var first, last sql.NullString
		if err := rows.Scan(&first, &last, &c.fieldMode, &c.kind); err != nil {
			return nil, fmt.Errorf("scanning creator for item %d: %w", itemID, err)
		}
		c.first, c.last = first.String, last.String
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading creators for item %d: %w", itemID, err)
	}

	byline := make([]creator, 0, len(all))
	for _, c := range all {
		if c.kind == "author" {
			byline = append(byline, c)
		}
	}
	if len(byline) == 0 {
		byline = all
	}

	names := make([]string, 0, len(byline))
	for _, c := range byline {
		// fieldMode 1 means Zotero stored the whole name as lastName with no
		// separate firstName (organisations, or a name entered as a single
		// field); fieldMode 0 is the ordinary first/last split.
		if c.fieldMode == 1 {
			names = append(names, c.last)
		} else {
			names = append(names, strings.TrimSpace(c.first+" "+c.last))
		}
	}
	return names, nil
}

// extractAll runs text extraction over every prepared candidate using
// workers goroutines (defaulting to runtime.NumCPU() when workers <= 0),
// respects ctx cancellation between candidates — extraction of any single
// document is already cancelled mid-flight because pdf.ExtractText is handed
// the same ctx — and returns documents and skips in candidate order (the
// caller sorts by Key for a stable final order).
func extractAll(ctx context.Context, prep []prepared, capability pdf.Capability, cacheDir string, workers int) ([]Document, []Skip, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	opts := pdf.DefaultSemanticOptions()

	docs := make([]*Document, len(prep))
	skips := make([]*Skip, len(prep))

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				p := prep[idx]
				doc, skip, ok := extractOne(ctx, p, capability, opts, cacheDir)
				if ok {
					docs[idx] = &doc
				} else {
					skips[idx] = &skip
				}
			}
		}()
	}

feed:
	for i := range prep {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	outDocs := make([]Document, 0, len(prep))
	outSkips := make([]Skip, 0, len(prep))
	for i := range prep {
		if docs[i] != nil {
			outDocs = append(outDocs, *docs[i])
		} else if skips[i] != nil {
			outSkips = append(outSkips, *skips[i])
		}
	}
	return outDocs, outSkips, nil
}

// extractOne extracts (or reuses cached) text for one prepared candidate. A
// bad or unreadable PDF here becomes a Skip, never an error returned up to
// Load: over roughly 789 PDFs some are always going to be encrypted,
// corrupt, or scanned with no usable text layer, and one such file must not
// cost the harness the other 780.
func extractOne(ctx context.Context, p prepared, capability pdf.Capability, opts pdf.SemanticOptions, cacheDir string) (Document, Skip, bool) {
	base := Document{Key: p.cand.attachmentKey, ParentKey: p.cand.parentKey, Path: p.path, Work: p.work}

	// The cache key folds in size and modification time, not just the
	// attachment key, so a PDF replaced or re-exported by Zotero misses the
	// stale entry instead of silently reusing another file's text.
	var cachePath string
	if cacheDir != "" {
		cachePath = filepath.Join(cacheDir, fmt.Sprintf("%s-%d-%d.txt", p.cand.attachmentKey, p.info.Size(), p.info.ModTime().Unix()))
		if cached, err := os.ReadFile(cachePath); err == nil && len(cached) > 0 {
			text := string(cached)
			base.Text = text
			base.Chars = int64(len(text))
			return base, Skip{}, true
		}
	}

	report, err := pdf.ExtractText(ctx, p.path, capability, opts)
	if err != nil {
		return Document{}, Skip{Key: p.cand.attachmentKey, Reason: fmt.Sprintf("extraction failed: %v", err)}, false
	}
	if report.Excerpt == "" {
		reason := "empty extracted text"
		if len(report.Evidence) > 0 {
			reason = strings.Join(report.Evidence, "; ")
		}
		return Document{}, Skip{Key: p.cand.attachmentKey, Reason: reason}, false
	}

	if cachePath != "" {
		// Extraction over ~789 PDFs runs in minutes, and this harness is run
		// twice per rule change (before and after); caching the excerpt is
		// what keeps the second run seconds instead of minutes. Cache
		// misses are silently tolerated — a full re-run is always correct,
		// just slower — but a write failure never fails the document.
		if err := os.MkdirAll(cacheDir, 0o700); err == nil {
			_ = os.WriteFile(cachePath, []byte(report.Excerpt), 0o600)
		}
	}

	base.Text = report.Excerpt
	base.Chars = report.Chars
	base.OCRUsed = report.OCRUsed
	base.NeedsReview = report.NeedsReview
	return base, Skip{}, true
}
