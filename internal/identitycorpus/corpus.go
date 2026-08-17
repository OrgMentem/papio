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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"papio/internal/config"
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

	// Secondary reports that this attachment is NOT its parent's primary
	// PDF — the lowest-itemID one dedupOnePerParent keeps. It is always
	// false in the default one-per-parent mode, where every document is
	// its parent's primary by construction; only
	// LoadOptions.AllAttachments can produce a document with it set. The
	// composite arm reads it as a signal in its own right: a supplement,
	// an alternate scan or a publisher cover sheet is ordinarily filed as
	// a second attachment on the article's own Zotero item, so nothing in
	// the parent's curated metadata describes the bytes this document
	// actually holds.
	Secondary bool
}

// Skip records a candidate PDF attachment that Load excluded from the
// corpus, and why, so a caller can report how many of Zotero's attachments
// ended up measured versus filtered out and by which rule.
type Skip struct {
	Key    string
	Reason string
}

// SkipReasonDuplicateAttachment is the reason dedupOnePerParent assigns when
// a parent carries more than one PDF. cmd/identity-corpus classifies these
// skips when cross-referencing how many secondaries the composite arm would
// need LoadWithOptions{AllAttachments: true} to surface.
const SkipReasonDuplicateAttachment = "parent has another PDF attachment"

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
	// baseAttachmentPathPref matches Zotero's own prefs.js line for
	// extensions.zotero.baseAttachmentPath, e.g.
	//   user_pref("extensions.zotero.baseAttachmentPath", "/Users/x/Papers");
	// prefs.js stores the value as a JS string literal, single- or
	// double-quoted (Firefox itself always writes double quotes, but a
	// hand-edited or migrated prefs.js is not guaranteed to), with
	// backslash-escaping rather than URL- or shell-escaping, hence
	// unescapeJSString below. Go's RE2 engine has no backreferences, so
	// the two quote styles are two separate alternatives -- group 1 for a
	// double-quoted value, group 2 for a single-quoted one -- rather than
	// one alternative whose closing quote is required to match its
	// opening one; parsePrefsJS tells the two apart by which group's span
	// is present (via FindSubmatchIndex), not by which capture is
	// non-empty, since an empty base path is itself a legitimate value.
	// parsePrefsJS runs this per already comment-stripped line, so a
	// commented-out call is never seen here at all.
	baseAttachmentPathPref = regexp.MustCompile(`user_pref\(\s*["']extensions\.zotero\.baseAttachmentPath["']\s*,\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')\s*\)`)
	// attachmentKeyPattern is the fixed shape of a Zotero item key: 8
	// characters, uppercase letters and digits. resolveAttachmentPath
	// checks every attachment key against it before joining the key into
	// a filesystem path or a cache filename (see the F1 comment there).
	attachmentKeyPattern = regexp.MustCompile(`^[A-Z0-9]{8}$`)
	// configDataDirPattern is a narrow, line-anchored scan for config.toml's
	// top-level data_dir key. See dataDirIsExplicit for why this is
	// deliberately not a full TOML decode.
	configDataDirPattern = regexp.MustCompile(`(?m)^data_dir\s*=\s*\S`)
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

	// secondary is set by selectAttachments in all-attachments mode for
	// every attachment that is not its parent's lowest-itemID PDF, and
	// travels through prepared into Document.Secondary. In the default
	// mode nothing ever sets it: the one candidate kept per parent is the
	// primary.
	secondary bool
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
//
// One PDF per bibliographic parent, as it always has been: this is the
// corpus Measure's pairwise baseline is defined over, so its result must
// not move. LoadWithOptions is the way to ask for anything else.
func Load(ctx context.Context, zoteroDir, cacheDir string, workers int) ([]Document, []Skip, error) {
	return load(ctx, zoteroDir, cacheDir, workers, false)
}

// LoadOptions is the parameter form of load, carrying the one thing Load's
// positional signature cannot express without changing under every existing
// caller.
type LoadOptions struct {
	ZoteroDir string
	CacheDir  string
	Workers   int

	// AllAttachments keeps every PDF attachment of a parent item rather
	// than only the parent's primary (see dedupOnePerParent for why the
	// default drops the rest). It exists for the composite arm of the
	// candidate-binding measurement: errata, corrigenda, supplements,
	// cover sheets and journal expansions are precisely the attachments
	// dedupOnePerParent removes, so measuring that failure class over the
	// default corpus reports zero composites while the class sits
	// untouched in the library — an empty measurement that reads as a
	// clean one.
	//
	// The cost the default avoids is still real and is not fixed here: a
	// secondary attachment's own front matter is scored against metadata
	// describing its parent's primary PDF, so pairwise Measure must not
	// be run over this mode's output. Document.Secondary marks which
	// documents carry that caveat.
	AllAttachments bool
}

// LoadWithOptions is Load with the loader's opt-in modes exposed. Called with
// a zero AllAttachments it is Load exactly.
func LoadWithOptions(ctx context.Context, opts LoadOptions) ([]Document, []Skip, error) {
	return load(ctx, opts.ZoteroDir, opts.CacheDir, opts.Workers, opts.AllAttachments)
}

func load(ctx context.Context, zoteroDir, cacheDir string, workers int, allAttachments bool) ([]Document, []Skip, error) {
	capability := pdf.DetectCapability()
	if capability.PDFToText == "" {
		return nil, nil, errors.New("poppler's pdftotext is not installed; identity corpus extraction requires it")
	}

	copyDir, err := snapshotZoteroDatabase(ctx, zoteroDir)
	if err != nil {
		return nil, nil, err
	}
	// See snapshotZoteroDatabase for why this harness never opens
	// zoteroDir/zotero.sqlite directly: Zotero keeps it open in WAL mode
	// for as long as the application runs, and a private, verified
	// snapshot is what makes every query below see one consistent
	// point-in-time library rather than risking a read racing Zotero's own
	// writer.
	defer os.RemoveAll(copyDir)

	dsn := "file:" + filepath.Join(copyDir, "zotero.sqlite") + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("opening snapshot zotero.sqlite: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("opening snapshot zotero.sqlite: %w", err)
	}

	candidates, err := queryCandidates(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	kept, skips := selectAttachments(candidates, allAttachments)

	// VAL-2: an "attachments:"-prefixed linkMode 2 path is relative to
	// whatever the operator set as Zotero's Linked Attachment Base
	// Directory (Preferences > Advanced > Files and Folders), read here
	// once from prefs.js rather than re-read per candidate. It is simply
	// absent when the operator never set it, which resolveAttachmentPath
	// must tell apart from a file that used to exist and does not anymore.
	baseAttachmentPath, haveBaseAttachmentPath := linkedAttachmentBasePath(zoteroDir)

	// VAL-1: the whole reason this harness reads Zotero instead of
	// papio's own artifact store is independence from papio's own,
	// already-scored output (see the package comment). A linkMode 2
	// (linked_file) attachment can point anywhere on disk, including back
	// into papio's own data directory — an operator who lets zotio or the
	// browser extension file a paper straight into Zotero as a linked
	// file, pointed at the very artifact papio itself produced, ends up
	// with a document Zotero holds but papio built. Run against this
	// operator's real library, 48 of the 50 linkMode 2 candidates resolved
	// to absolute paths inside papio's data directory, and 9 of those were
	// artifacts papio had already delivered and already identity-scored:
	// scoring pdf.MatchIdentity against its own prior verdict is exactly
	// the circularity the Zotero corpus exists to avoid, so any candidate
	// that resolves inside papio's data directory is excluded here, not
	// silently measured as if it were independent evidence.
	papioRoot := papioDataDir()
	// VAL-1's exclusion is only as good as papioRoot: papioDataDir
	// deliberately always answers with the built-in default rather than
	// the operator's live config.toml (see its own comment for why —
	// config.Load's strict decode has no business aborting corpus
	// measurement over a config error unrelated to Zotero or papio's
	// artifact tree). The cost of that choice is real, though: an
	// operator who relocated data_dir gets the default answer here
	// regardless, and the exclusion above ends up checking a tree that
	// is not actually where their papio artifacts live, without anyone
	// being told. dataDirIsExplicit is the narrowest disclosure that
	// does not reintroduce the abort risk — it looks only for the
	// data_dir key's presence, never its value or the rest of the file —
	// so the operator at least learns the exclusion's answer is the
	// default, not a confirmed fact about their own configuration.
	if !dataDirIsExplicit() {
		fmt.Fprintln(os.Stderr, "identity-corpus: papio data directory resolved from the built-in default, not an explicit config.toml setting; the papio-owned exclusion may be checking the wrong location")
	}

	var prep []prepared
	for _, c := range kept {
		path, err := resolveAttachmentPath(zoteroDir, baseAttachmentPath, haveBaseAttachmentPath, c)
		if err != nil {
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: err.Error()})
			continue
		}
		if underDir(path, papioRoot) {
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: "papio-owned artifact"})
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			// PRIV-2: the resolved absolute path, and os.Stat's own error
			// text, both name the file (Zotero names stored files after
			// author, year and title) — neither belongs in a reason meant
			// to be pasted into a bug report. The attachment key already
			// in Skip.Key is enough for the operator to find the row in
			// their own library.
			skips = append(skips, Skip{Key: c.attachmentKey, Reason: "file missing"})
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

	// VAL-6 / PRIV-3: validate (and, on first run, create) the cache
	// directory once, before any worker touches it, rather than trusting
	// os.MkdirAll's silent adoption of whatever already sits at cacheDir.
	// See validateCacheDir for the threat this closes.
	if cacheDir != "" {
		if err := validateCacheDir(cacheDir); err != nil {
			return nil, nil, fmt.Errorf("identity corpus cache: %w", err)
		}
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

// snapshotZoteroDatabase produces a private, verified point-in-time copy of
// the operator's live zotero.sqlite into a fresh scratch directory (mode
// 0700, via os.MkdirTemp) and returns that directory; the caller owns
// removing it.
//
// There are two ways to get that copy, tried in order, because neither one
// alone covers this tool's actual operating conditions. VACUUM INTO is the
// atomic path PRIV-4 introduced: SQLite opens a single read transaction
// against the live database and streams a consistent snapshot straight
// into one fresh file, no -wal/-shm copies needed. But "safe to run while
// Zotero is open" is this tool's stated contract, and Zotero keeps
// zotero.sqlite in a locking mode that refuses even a second read-only
// connection while the application is running -- VACUUM INTO then fails
// with SQLITE_BUSY immediately, which is the expected case whenever the
// operator has Zotero open, not a bug in this tool or a reason to abort
// the run. copyZoteroDatabaseFallback is what the tool falls back to
// there: the OS-level byte copy PRIV-4 replaced, but no longer trusted
// blindly -- it now stats zotero.sqlite and its -wal sidecar before and
// after the copy and retries when they moved, so the fallback is still a
// snapshot this function can vouch for, not just one it hopes is whole.
// Either way, PRAGMA quick_check runs against the result before Load ever
// sees it, and which path was actually used is reported on stderr, since
// that changes what guarantee the operator is measuring under.
func snapshotZoteroDatabase(ctx context.Context, zoteroDir string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "papio-identitycorpus-*")
	if err != nil {
		return "", fmt.Errorf("creating scratch dir for zotero.sqlite snapshot: %w", err)
	}

	scratchPath := filepath.Join(tmpDir, "zotero.sqlite")
	method := "VACUUM INTO (atomic snapshot)"
	if vacuumErr := vacuumIntoSnapshot(ctx, zoteroDir, scratchPath); vacuumErr != nil {
		if fallbackErr := copyZoteroDatabaseFallback(ctx, zoteroDir, tmpDir); fallbackErr != nil {
			os.RemoveAll(tmpDir)
			return "", fmt.Errorf("zotero.sqlite snapshot failed: VACUUM INTO could not run (%w), and the fallback copy also failed: %w", vacuumErr, fallbackErr)
		}
		method = "byte-level copy (VACUUM INTO unavailable while Zotero holds the database open)"
	}

	if err := verifySnapshot(ctx, scratchPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	fmt.Fprintf(os.Stderr, "identity-corpus: zotero.sqlite snapshot method: %s\n", method)
	return tmpDir, nil
}

// vacuumIntoSnapshot is the atomic path: open the live database read-only
// and VACUUM INTO scratchPath. It fails whenever Zotero itself currently
// holds zotero.sqlite in a locking mode that refuses a second reader --
// SQLITE_BUSY -- which is routine while the operator has Zotero open, and
// the caller treats it as an expected fallback trigger, not a fatal error.
//
// F2: this never modifies zotero.sqlite's own data -- mode=ro plus a
// VACUUM INTO that only ever reads the source enforces that -- but SQLite
// still treats a WAL-mode database's -wal/-shm sidecars as part of
// opening it for read, not just for write: against a library Zotero has
// fully checkpointed and closed, where those sidecars are currently
// absent, this connection alone can recreate zotero.sqlite-wal and
// zotero.sqlite-shm beside zotero.sqlite and leave them there once
// closed. See dev/identity-corpus.md for the operator-facing version of
// this fact.
func vacuumIntoSnapshot(ctx context.Context, zoteroDir, scratchPath string) error {
	liveDSN := "file:" + filepath.Join(zoteroDir, "zotero.sqlite") + "?mode=ro&_pragma=busy_timeout(5000)"
	live, err := sql.Open("sqlite", liveDSN)
	if err != nil {
		return fmt.Errorf("opening live zotero.sqlite read-only: %w", err)
	}
	defer live.Close()

	if _, err := live.ExecContext(ctx, "VACUUM INTO ?", scratchPath); err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	return nil
}

// copyZoteroDatabaseFallback copies zotero.sqlite, and its -wal/-shm
// siblings when present, into tmpDir at the OS level -- the approach
// PRIV-4 replaced with VACUUM INTO, kept here as the fallback for the one
// case VACUUM INTO cannot handle: Zotero itself holding zotero.sqlite open
// in a locking mode that refuses a second reader. Three independent
// copyFile calls give up the single-transaction consistency VACUUM INTO
// has, so this instead stats zotero.sqlite and zotero.sqlite-wal (size and
// modification time) immediately before and after the copy: identical
// stats on both sides mean Zotero's own writer did not touch either file
// during the window this function had them open, which is the actual
// evidence the copy is whole. PRAGMA quick_check cannot be that evidence
// by itself -- a WAL truncated exactly on a page boundary mid-copy still
// parses as a valid, merely stale, database. Up to 3 attempts are made
// before giving up and naming the contention; each attempt clears
// whatever an earlier attempt left in tmpDir first (see the F3 comment
// inside the loop below), so a retry can never mix one attempt's main
// database with a leftover WAL from a different one.
func copyZoteroDatabaseFallback(ctx context.Context, zoteroDir, tmpDir string) error {
	const maxAttempts = 3
	mainPath := filepath.Join(zoteroDir, "zotero.sqlite")
	walPath := filepath.Join(zoteroDir, "zotero.sqlite-wal")

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// F3: without this, an earlier attempt's copy of
		// zotero.sqlite-wal or zotero.sqlite-shm survives into this one.
		// If Zotero checkpoints and deletes the WAL between attempt N and
		// attempt N+1, the os.Stat guard below correctly skips copying
		// it on N+1 -- there is nothing left at the source to copy -- but
		// that leaves attempt N's now-stale WAL copy sitting in tmpDir
		// beside attempt N+1's freshly copied main database. The
		// before/after stat signatures then agree (both see the WAL as
		// already gone on the source side), this function reports
		// success, and PRAGMA quick_check cannot catch it: a main
		// database and a WAL from two different points in Zotero's
		// history each parse as perfectly valid SQLite on their own.
		// Clearing every file this function can produce, at the top of
		// every attempt, makes an attempt's result entirely its own,
		// never half of a previous attempt's.
		for _, name := range []string{"zotero.sqlite", "zotero.sqlite-wal", "zotero.sqlite-shm"} {
			if err := os.Remove(filepath.Join(tmpDir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clearing a previous attempt's %s before retrying: %w", name, err)
			}
		}
		before := [2]fileSignature{statSignature(mainPath), statSignature(walPath)}

		if err := copyFile(ctx, mainPath, filepath.Join(tmpDir, "zotero.sqlite")); err != nil {
			return err
		}
		// zotero.sqlite-wal and zotero.sqlite-shm only exist while Zotero
		// holds the database open in WAL mode, so their absence here is
		// the normal case (Zotero not running, or fully checkpointed)
		// rather than an error.
		for _, name := range []string{"zotero.sqlite-wal", "zotero.sqlite-shm"} {
			src := filepath.Join(zoteroDir, name)
			if _, statErr := os.Stat(src); statErr != nil {
				continue
			}
			if err := copyFile(ctx, src, filepath.Join(tmpDir, name)); err != nil {
				return err
			}
		}

		after := [2]fileSignature{statSignature(mainPath), statSignature(walPath)}
		if before == after {
			return nil
		}
		lastErr = errors.New("zotero.sqlite or its WAL changed while it was being copied")
	}
	return fmt.Errorf("zotero.sqlite snapshot unstable after %d attempts (Zotero is writing faster than this tool can copy it): %w", maxAttempts, lastErr)
}

// fileSignature is the size/modification-time pair
// copyZoteroDatabaseFallback compares before and after a copy attempt. A
// missing file signs as its zero value, so a file that appears or
// disappears mid-copy -- Zotero starting or finishing a WAL checkpoint --
// also counts as a change and triggers a retry.
type fileSignature struct {
	size    int64
	modTime int64
}

func statSignature(path string) fileSignature {
	info, err := os.Stat(path)
	if err != nil {
		return fileSignature{}
	}
	return fileSignature{size: info.Size(), modTime: info.ModTime().UnixNano()}
}

// copyFile copies src to dst, honouring ctx during the copy itself rather
// than only between whole-file attempts. Before this, ctx was checked once
// at the top of copyZoteroDatabaseFallback's retry loop and then handed to
// a single io.Copy call that ran to completion regardless -- a Ctrl-C part
// way through zotero.sqlite's own copy, several hundred megabytes on a
// library this size, waited for that copy to finish before the caller's
// next ctx.Err() check ever ran. Reading through a fixed-size buffer
// instead of one unbroken io.Copy gives ctx a chance to be checked every
// chunk, without pulling in any dependency beyond the standard library.
func copyFile(ctx context.Context, src, dst string) (err error) {
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
	buf := make([]byte, 256*1024)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("copying %s to %s: %w", src, dst, writeErr)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("copying %s to %s: %w", src, dst, readErr)
		}
	}
}

// verifySnapshot opens the finished snapshot at path and requires PRAGMA
// quick_check to report exactly "ok" before Load is allowed to use it --
// the check both snapshot paths share, and on the fallback path the last
// line of defense on top of copyZoteroDatabaseFallback's own stat-based
// consistency check.
func verifySnapshot(ctx context.Context, path string) error {
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening zotero.sqlite snapshot for verification: %w", err)
	}
	defer db.Close()

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("running quick_check on zotero.sqlite snapshot: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("zotero.sqlite snapshot failed integrity check: %s", result)
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
	// The only interpolation is a comma-joined run of "?" placeholders built
	// two lines up; every value rides args through the driver's binding.
	// #nosec G201
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
				skips = append(skips, Skip{Key: c.attachmentKey, Reason: SkipReasonDuplicateAttachment})
			}
		}
	}
	return kept, skips
}

// selectAttachments applies the loader's attachment-selection mode. The
// default is dedupOnePerParent, unchanged and byte-identical: same kept
// candidate, same Skip rows, same reasons.
//
// All-attachments mode keeps every candidate and skips none, marking every
// attachment that is not its parent's primary. The primary is chosen by the
// same rule dedupOnePerParent uses — the lowest attachment itemID, which
// Zotero assigns on import — so a document's Secondary flag means the same
// thing in both modes and does not depend on the order SQLite handed the
// rows over in. The result is sorted by attachment itemID for the same
// reason: it must not vary run to run, and the map walk above gives no
// order at all.
func selectAttachments(candidates []candidate, allAttachments bool) ([]candidate, []Skip) {
	if !allAttachments {
		return dedupOnePerParent(candidates)
	}

	primary := make(map[int64]int64, len(candidates))
	for _, c := range candidates {
		if id, ok := primary[c.parentID]; !ok || c.attachmentID < id {
			primary[c.parentID] = c.attachmentID
		}
	}

	kept := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		c.secondary = c.attachmentID != primary[c.parentID]
		kept = append(kept, c)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].attachmentID < kept[j].attachmentID })
	return kept, nil
}

// resolveAttachmentPath turns a Zotero attachment's linkMode and stored
// path into a filesystem path. linkMode 3 (linked_url) points at a web
// page, not a local file, and any linkMode this library has not been
// observed to use is treated the same way — as a Skip — rather than
// guessed at.
//
// linkMode 2 (linked_file) is the one case where the stored path can be
// relative: an "attachments:"-prefixed path resolves against whatever the
// operator set as Zotero's Linked Attachment Base Directory, passed in as
// baseAttachmentPath/haveBaseAttachmentPath (see linkedAttachmentBasePath).
// VAL-2 found the previous behaviour — joining the relative path onto
// zoteroDir/linked-attachments — resolved against a directory Zotero
// itself never reads or writes; it was inert on a library where the pref
// happens to be unset (every linkMode 2 row here is already absolute) and
// silently wrong on any library where the operator actually configured
// it. When the pref is unset and a relative path shows up anyway, that is
// reported as its own condition — the base directory is unknown — rather
// than folded into "file not found", which would tell the operator their
// file is missing when the true fact is that this harness does not know
// where to look for it.
//
// F1: the attachment key is joined into the storage path below (case 0,
// 1) and, later, into extractOne's cache filename — and it arrives over
// sync from whoever added the item, exactly like the attachment name two
// lines down that PRIV-5 already refuses when it escapes storage/<KEY>/.
// Validating the name and leaving the key it is joined with untouched is
// not validating the join: a hostile key composed the same way as a
// hostile name resolves outside storage/ entirely, and separately
// composes a cache filename outside cacheDir. Zotero item keys are a
// fixed shape — 8 characters, uppercase letters and digits — so a key
// outside that shape is refused once, here, before either use.
func resolveAttachmentPath(zoteroDir, baseAttachmentPath string, haveBaseAttachmentPath bool, c candidate) (string, error) {
	if !attachmentKeyPattern.MatchString(c.attachmentKey) {
		return "", errors.New("file missing: attachment key has an unexpected shape")
	}
	var path string
	switch c.linkMode {
	case 0, 1: // imported_file, imported_url: copied into Zotero's own storage.
		name := strings.TrimPrefix(c.path, "storage:")
		// PRIV-5: this path column is DB-supplied, and for a group
		// library it arrives over sync from whoever added the item, not
		// necessarily this operator. filepath.Join below would silently
		// clean a ".." component into an escape from storage/<KEY>/, so a
		// name that tries to leave that directory — or is exactly ".." —
		// is rejected outright instead of resolved. Ten rows in this
		// library already contain "..", and none of them also contains a
		// path separator, which is the only reason none currently
		// escapes.
		if name == ".." || strings.ContainsAny(name, "/\\") {
			return "", errors.New("storage attachment name escapes its own directory")
		}
		path = filepath.Join(zoteroDir, "storage", c.attachmentKey, name)
	case 2: // linked_file: resolved against the operator's configured base directory, or already absolute.
		if rel, ok := strings.CutPrefix(c.path, "attachments:"); ok {
			if !haveBaseAttachmentPath {
				return "", errors.New("linked attachment base path not configured: set extensions.zotero.baseAttachmentPath in Zotero (Advanced > Files and Folders > Linked Attachment Base Directory)")
			}
			path = filepath.Join(baseAttachmentPath, rel)
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

// zoteroProfileDir locates Zotero's Firefox-style profile directory by
// parsing profiles.ini, the file every Firefox-family application writes
// to record where its actual per-profile state -- including prefs.js --
// lives. This matters because zoteroDir, the argument Load already takes,
// is Zotero's DATA directory (~/Zotero by default, holding zotero.sqlite
// and the storage/ tree), and the PROFILE directory prefs.js actually
// lives in is a second, independently located tree that shares nothing
// with zoteroDir but the word "Zotero" -- on a typical macOS install it is
// ~/Library/Application Support/Zotero/Profiles/<random>.default. The
// previous code read <zoteroDir>/prefs.js directly, which does not exist
// on any real Zotero install; haveBaseAttachmentPath was therefore always
// false, and every "attachments:"-relative linked file was refused as
// unconfigured even when the operator had genuinely set the pref, which is
// the one case this whole mechanism exists to handle. There is no
// shortcut around profiles.ini: it is the same file every other tool that
// needs a Firefox-family preference -- Firefox itself, sync clients,
// zotero-cli -- has to parse for exactly this reason, since the profile
// directory's name is randomly generated per install and not derivable
// from anything else on disk.
func zoteroProfileDir() (string, bool) {
	for _, root := range zoteroAppSupportDirs() {
		data, err := os.ReadFile(filepath.Join(root, "profiles.ini"))
		if err != nil {
			continue
		}
		if dir, ok := parseProfilesIni(root, data); ok {
			return dir, true
		}
	}
	return "", false
}

// zoteroAppSupportDirs lists, in order, the directories that might hold
// Zotero's profiles.ini on this OS. Linux gets two candidates because
// Zotero has shipped profiles.ini under both ~/.zotero/zotero (current)
// and the older bare ~/.zotero at different points in its history, and
// nothing on disk announces which one a given install used.
func zoteroAppSupportDirs() []string {
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return []string{filepath.Join(appData, "Zotero", "Zotero")}
		}
		return nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		return []string{filepath.Join(home, "Library", "Application Support", "Zotero")}
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		return []string{filepath.Join(home, ".zotero", "zotero"), filepath.Join(home, ".zotero")}
	}
}

// iniSection is one [name] block of profiles.ini's Windows-style INI
// format, as an ordered key/value map. Firefox writes keys and values
// without quoting, so unlike prefs.js there is no escaping to reverse
// here.
type iniSection struct {
	name string
	kv   map[string]string
}

// parseIni does the minimum profiles.ini needs: split on [section] headers
// and key=value lines, skipping blank lines and ;/# comments. It is not a
// general INI parser -- profiles.ini never nests, quotes, or continues a
// line -- so nothing beyond that is attempted.
func parseIni(data []byte) []iniSection {
	var sections []iniSection
	var current *iniSection
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sections = append(sections, iniSection{name: line[1 : len(line)-1], kv: map[string]string{}})
			current = &sections[len(sections)-1]
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		current.kv[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return sections
}

// parseProfilesIni resolves the profile directory profiles.ini (rooted at
// root, the directory profiles.ini itself lives in) points at: the
// [ProfileN] section whose own Default key reads "1", or [Profile0] when
// none is marked default -- the same fallback order Firefox's own profile
// manager uses, since Zotero's profiles.ini is exactly that format. A
// profile's Path is relative to root unless its IsRelative key reads "0",
// and profiles.ini always writes Path with forward slashes regardless of
// platform, hence filepath.FromSlash before joining.
func parseProfilesIni(root string, data []byte) (string, bool) {
	var fallback map[string]string
	for _, section := range parseIni(data) {
		if !strings.HasPrefix(section.name, "Profile") {
			continue
		}
		if section.name == "Profile0" {
			fallback = section.kv
		}
		if section.kv["Default"] == "1" {
			if dir, ok := profileSectionDir(root, section.kv); ok {
				return dir, true
			}
		}
	}
	if fallback != nil {
		return profileSectionDir(root, fallback)
	}
	return "", false
}

// profileSectionDir turns one [ProfileN] section's Path/IsRelative pair
// into an absolute directory, per the rule documented on parseProfilesIni.
func profileSectionDir(root string, kv map[string]string) (string, bool) {
	path := kv["Path"]
	if path == "" {
		return "", false
	}
	if kv["IsRelative"] == "0" {
		return filepath.Clean(path), true
	}
	return filepath.Join(root, filepath.FromSlash(path)), true
}

// linkedAttachmentBasePath reads extensions.zotero.baseAttachmentPath --
// the directory Zotero itself resolves a linkMode 2 attachment's
// "attachments:"-prefixed relative path against -- from prefs.js in
// Zotero's profile directory (see zoteroProfileDir for why that is not
// zoteroDir). <zoteroDir>/prefs.js is tried as a fallback, for an operator
// who happens to keep the two together, and both attempts failing is
// simply "unset". ok is false in that case, which callers must treat as
// "unknown", not as any kind of file-not-found: there is nothing to
// resolve against, a different fact from a resolved path not existing.
func linkedAttachmentBasePath(zoteroDir string) (string, bool) {
	if profileDir, ok := zoteroProfileDir(); ok {
		if value, ok := readBaseAttachmentPathPref(filepath.Join(profileDir, "prefs.js")); ok {
			return value, true
		}
	}
	return readBaseAttachmentPathPref(filepath.Join(zoteroDir, "prefs.js"))
}

// readBaseAttachmentPathPref reads and parses one prefs.js candidate path,
// treating a missing file the same as a present one with no matching
// pref: both mean "try the next candidate", not an error.
func readBaseAttachmentPathPref(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parsePrefsJS(data)
}

// parsePrefsJS extracts extensions.zotero.baseAttachmentPath from a
// Firefox-style prefs.js file's raw content, honouring the two things
// that make it unsafe to regex the whole file in one pass. First, a line
// commented out with a leading "//", or sitting inside a "/* */" block --
// both of which real prefs.js files carry, since Firefox itself opens
// every one with a "// Mozilla User Preferences" line and a "/* Do not
// edit this file... */" block -- is not a setting; it is a note to a
// human, or dead history from a previous edit, and must never be read as
// live configuration. Second, prefs.js is last-write-wins: Firefox
// rewrites the whole file on every change, but a hand-edited or migrated
// copy can carry more than one user_pref call for the same key, and
// whichever one sorts last in the file is authoritative -- not whichever a
// first-match regex happens to hit first -- so every surviving line is
// scanned and the last match wins.
func parsePrefsJS(data []byte) (string, bool) {
	cleaned := stripPrefsComments(data)
	value, found := "", false
	for _, line := range bytes.Split(cleaned, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("//")) {
			continue
		}
		loc := baseAttachmentPathPref.FindSubmatchIndex(line)
		if loc == nil {
			continue
		}
		var raw []byte
		if loc[2] != -1 {
			raw = line[loc[2]:loc[3]] // double-quoted value
		} else {
			raw = line[loc[4]:loc[5]] // single-quoted value
		}
		value, found = unescapeJSString(string(raw)), true
	}
	return value, found
}

// stripPrefsComments blanks out every "/* ... */" block comment in data,
// preserving line breaks so the line-based "//" check in parsePrefsJS, and
// the byte offsets baseAttachmentPathPref reports, both still line up with
// the original file. An unterminated block comment -- malformed input, not
// something a well-formed prefs.js ever produces -- blanks the rest of the
// file rather than looping forever looking for a close that is not there.
func stripPrefsComments(data []byte) []byte {
	out := append([]byte(nil), data...)
	for {
		start := bytes.Index(out, []byte("/*"))
		if start < 0 {
			return out
		}
		rel := bytes.Index(out[start+2:], []byte("*/"))
		end := len(out)
		if rel >= 0 {
			end = start + 2 + rel + 2
		}
		for i := start; i < end; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
		if rel < 0 {
			return out
		}
	}
}

// unescapeJSString reverses the backslash-escaping a JS string literal
// uses: a backslash makes the following character literal, whatever that
// character is. Firefox's own preference writer only ever produces \\ and
// \" this way, but a hand-edited prefs.js can escape a single quote (\')
// inside a single-quoted value too, so the reversal is generic rather than
// hardcoded to the two sequences Firefox itself happens to emit.
func unescapeJSString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// papioDataDir returns the root of papio's own data directory, so
// candidates that resolve inside it can be excluded (see the VAL-1 comment
// in Load). It calls config.Default().DataDir — the same baseline
// defaultDataDir() logic every other component in this module falls back
// to (~/.local/share/papio, or %LOCALAPPDATA%\papio on Windows) — rather
// than config.Load(""), deliberately: Load parses and strictly validates
// the operator's live config.toml (rejecting unknown fields, requiring a
// valid access_mode, and every other setting besides), none of which this
// harness has any use for, and a config error that has nothing to do with
// Zotero or papio's artifact tree would then abort corpus measurement for
// a reason unrelated to either. Default() reaches exactly the tree an
// operator who has not overridden data_dir is actually using, which is the
// case VAL-1 needs to catch.
func papioDataDir() string {
	dir := config.Default().DataDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// dataDirIsExplicit reports whether the operator's own config.toml sets
// data_dir, as distinct from papioDataDir falling back to the built-in
// default. It is a narrow, line-anchored text scan for the top-level key
// -- not a full TOML decode -- deliberately: config.Load's strict decoder
// rejects unknown fields and requires a valid access_mode, and papioDataDir
// already decided (see its own comment) that a config error unrelated to
// data_dir must never abort corpus measurement. data_dir is Config's only
// field of that name, so a line-anchored match against the raw file text
// is unambiguous without a real parser, and any read failure -- including
// "no config.toml at all" -- is reported as "not explicit": this function
// only ever adds a caveat to Load's stderr output, so erring toward that
// caveat is always safe, while erring toward silence on a real override is
// the exact defect the exclusion needs disclosed, not hidden.
func dataDirIsExplicit() bool {
	data, err := os.ReadFile(filepath.Join(config.Dir(), "config.toml"))
	if err != nil {
		return false
	}
	return configDataDirPattern.Match(data)
}

// underDir reports whether path lies at or beneath dir, comparing cleaned
// absolute paths lexically. Both papioDataDir and resolveAttachmentPath
// already run their results through filepath.Abs, and a symlink escape
// into or out of papio's data directory is not the threat model VAL-1
// addresses — it only needs to catch a Zotero attachment path that was
// written pointing at papio's own tree.
func underDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
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

// validateCacheDir creates cacheDir if needed and refuses to use it when a
// co-tenant could have staged it first. os.MkdirAll silently adopts a
// pre-existing directory regardless of who owns it or how permissive its
// mode is, and the default cache location is $TMPDIR — per-user on macOS,
// but shared /tmp on Linux, where any other local user can plant a
// directory at the exact path this harness is about to write hundreds of
// files of front matter into. A symlinked cacheDir is refused for the same
// reason a planted symlink at an individual cache file matters (see
// writeCacheEntry): a directory another user owns, or that group or other
// can write into, hands that user the ability to enumerate every
// attachment key this run touches (each cache filename is
// key-size-mtime.txt) or to poison an entry a later run would then treat
// as already-extracted text.
//
// F4: the leaf's own mode is masked against 0o077, not 0o022. A directory
// the operator owns at the ordinary 0o755 passed the old 0o022 mask —
// only a write bit failed it — even though group or other can still list
// its contents, which is the enumeration this comment already names as
// the threat; every bit but the owner's own is refused now, not just
// write. That still says nothing about who can rename or remove the leaf
// itself, though: that authority belongs to whoever can write the
// *parent* directory, not to whoever owns the leaf, so a leaf created
// under a world-writable parent passed every check above and still let a
// co-tenant swap it out from under this run between validation and the
// writes extraction performs afterward. An explicitly chosen -cache
// directory therefore also has its ancestry checked (checkCacheDirParents
// below). os.UserCacheDir()'s default is exempt from that walk: it
// resolves to a fixed, OS-defined per-user directory (~/Library/Caches,
// $XDG_CACHE_HOME or ~/.cache, %LocalAppData%) whose ancestry is the
// platform's own user-profile tree, not a path this tool or the operator
// chose — walking it adds no coverage a -cache override doesn't already
// need on its own, and walking every path all the way to the filesystem
// root regardless of where it started would fail on the root-owned
// system directories every path eventually passes through.
func validateCacheDir(cacheDir string) error {
	start := time.Now()
	if abs, err := filepath.Abs(cacheDir); err == nil {
		cacheDir = abs
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}
	info, err := os.Lstat(cacheDir)
	if err != nil {
		return fmt.Errorf("checking cache directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("cache directory is a symlink; refusing to use it")
	}
	if !info.IsDir() {
		return errors.New("cache path exists and is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("cache directory is writable by group or other; refusing to use it")
	}
	if uid, ok := fileOwnerUID(info); ok && int64(uid) != int64(os.Getuid()) {
		return errors.New("cache directory is not owned by the current user; refusing to use it")
	}
	if userCacheRoot, ucErr := os.UserCacheDir(); ucErr != nil || !underDir(cacheDir, userCacheRoot) {
		if err := checkCacheDirParents(cacheDir); err != nil {
			return err
		}
	}
	// F5: sweep once validateCacheDir has confirmed cacheDir itself is
	// safe to touch at all — never before that, and never on a directory
	// this call is about to refuse. See sweepStaleCacheTemps for what it
	// removes and why.
	sweepStaleCacheTemps(cacheDir, start)
	return nil
}

// checkCacheDirParents walks the ancestors of an explicitly chosen cache
// directory (see the F4 comment on validateCacheDir), refusing one a
// co-tenant could write into. Only the write bit is tested here (0o022,
// not the leaf's 0o077): listing a parent's own entries exposes nothing
// the leaf's contents shield, but write access to a parent is exactly the
// authority needed to rename or remove the leaf this run already
// validated and put something else in its place before extraction
// starts. Root ownership is accepted at any level — root already has
// unrestricted access regardless of any check an unprivileged process can
// perform, and every OS's directory tree above a user's home
// (/, /Users, /home, ...) is ordinarily root-owned, so refusing those
// would fail every explicit -cache path on an ordinary machine, not just
// a hostile one.
func checkCacheDirParents(cacheDir string) error {
	dir := filepath.Dir(cacheDir)
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("checking cache directory's parent: %w", err)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("a directory containing the cache directory is writable by group or other; refusing to use it")
		}
		if uid, ok := fileOwnerUID(info); ok && int64(uid) != int64(os.Getuid()) && uid != 0 {
			return errors.New("a directory containing the cache directory is not owned by the current user; refusing to use it")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// fileOwnerUID returns info's owning user id, on the platforms whose
// os.FileInfo.Sys() carries one. It goes through reflect rather than a
// syscall.Stat_t type assertion so this file keeps compiling everywhere —
// syscall.Stat_t does not exist on Windows, whose Sys() returns something
// else entirely — instead of needing a second, build-tagged file just for
// this one check. ok is false wherever the field is absent, and
// validateCacheDir simply skips the ownership half of its check there:
// Windows has no POSIX owner/group-writable model for the other half to
// complain about either.
func fileOwnerUID(info os.FileInfo) (uid uint32, ok bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("Uid")
	if !f.IsValid() || f.Kind() != reflect.Uint32 {
		return 0, false
	}
	u := f.Uint()
	if u > math.MaxUint32 {
		return 0, false
	}
	return uint32(u), true
}

// cacheTempPattern is writeCacheEntry's os.CreateTemp pattern for its
// in-progress temp file, and sweepStaleCacheTemps's glob for finding one
// left behind — the same shape in both places, so a leftover from a
// killed run is always recognizable as one of these, not mistaken for a
// finished entry (those are named key-size-mtime.txt, never matching
// this pattern) or swept before it is even written.
const cacheTempPattern = "identity-corpus-*.tmp"

// writeCacheEntry writes text to cachePath by creating a same-directory
// temp file with O_EXCL — refusing to follow or overwrite anything already
// there — and renaming it into place. VAL-6/PRIV-3b found the previous
// os.WriteFile(cachePath, ...) left two openings: a process killed
// mid-write left a truncated file the read path's `len(cached) > 0` check
// accepted as a complete (and silently wrong) extraction result, and
// WriteFile follows symlinks, so a planted symlink at a cache filename
// could redirect the write anywhere this process can write.
// validateCacheDir already keeps a co-tenant from placing anything inside
// cacheDir at all; this is the other half, for the write itself.
//
// F5: the temp name used to be cachePath + ".tmp." + the process id,
// which is unique only until two runs happen to draw the same pid — at
// which point the second run's O_EXCL fails on the first run's leftover
// and extractOne silently discards that error by design, so the
// document is quietly re-extracted every run instead of ever caching.
// os.CreateTemp picks its own random suffix, so no two runs, however
// their pids land, can ever collide on the same temp name; the killed
// run's own leftover is instead reclaimed by sweepStaleCacheTemps at the
// start of the next run that validates this cacheDir.
func writeCacheEntry(cachePath string, text []byte) error {
	f, err := os.CreateTemp(filepath.Dir(cachePath), cacheTempPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// sweepStaleCacheTemps removes writeCacheEntry's own temp files that
// predate start, once per Load call, right after validateCacheDir has
// confirmed cacheDir is safe to operate on at all. A run killed between
// os.CreateTemp and the rename that finishes writeCacheEntry leaves its
// temp file sitting in cacheDir forever — one probe left 470 MB of
// extracted front matter behind this way — and because it is never
// readable at the final cache path, every later run pays the extraction
// cost for that document again, silently, with no error surfaced anywhere
// a caller would see it (extractOne treats a cache write failure as
// tolerable; see extractOne). A temp file can only predate start if an
// earlier run made it and never finished: this run's own temp files are
// all created after start, so none of them are ever swept, even while a
// worker is still writing one concurrently with this call.
func sweepStaleCacheTemps(cacheDir string, start time.Time) {
	matches, err := filepath.Glob(filepath.Join(cacheDir, cacheTempPattern))
	if err != nil {
		return
	}
	for _, path := range matches {
		info, err := os.Lstat(path)
		if err != nil || !info.ModTime().Before(start) {
			continue
		}
		os.Remove(path)
	}
}

// classifyExtractionFailure reduces pdf.ExtractText's evidence trail — in
// several cases poppler's own stderr, which routinely names the file it
// was reading — to one of the report's coarse, path-free classes. PRIV-2
// found the previous behaviour, joining every evidence line verbatim into
// the Skip reason, printed the resolved absolute path once and then
// poppler's own filename-bearing message a second time inside it; a Skip
// reason has to survive being pasted into a bug report unread, so nothing
// the tool chose to say about this particular file leaves this function,
// only which of a fixed, small set of things went wrong.
func classifyExtractionFailure(evidence []string) string {
	joined := strings.Join(evidence, "; ")
	switch {
	case strings.Contains(joined, "exceeds") && strings.Contains(joined, "bytes"):
		// pdftotext's own output-cap guard (see runTextTool in
		// internal/pdf/semantic.go). VAL-3 found this one condition
		// accounts for 38 of 77 skips, and hits 44% of book candidates
		// against 1.7% of journal articles, which is exactly the kind of
		// silent thinning the report's summary needs to surface as one
		// number instead of 38 identical stderr lines.
		return "output cap"
	case strings.Contains(joined, "deadline") || strings.Contains(joined, "context canceled"):
		return "extraction failed: timed out"
	case strings.Contains(joined, "capability:"):
		return "extraction failed: required tool unavailable"
	case strings.Contains(joined, "OCR"):
		return "extraction failed: OCR did not recover usable text"
	case joined == "":
		return "extraction failed: no text extracted"
	default:
		return "extraction failed: pdftotext reported an error"
	}
}

// extractOne extracts (or reuses cached) text for one prepared candidate. A
// bad or unreadable PDF here becomes a Skip, never an error returned up to
// Load: over roughly 679 PDFs some are always going to be encrypted,
// corrupt, or scanned with no usable text layer, and one such file must not
// cost the harness the rest.
func extractOne(ctx context.Context, p prepared, capability pdf.Capability, opts pdf.SemanticOptions, cacheDir string) (Document, Skip, bool) {
	base := Document{Key: p.cand.attachmentKey, ParentKey: p.cand.parentKey, Path: p.path, Work: p.work, Secondary: p.cand.secondary}

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
		return Document{}, Skip{Key: p.cand.attachmentKey, Reason: classifyExtractionFailure([]string{err.Error()})}, false
	}
	if report.Excerpt == "" {
		return Document{}, Skip{Key: p.cand.attachmentKey, Reason: classifyExtractionFailure(report.Evidence)}, false
	}

	if cachePath != "" {
		// Load's validateCacheDir already confirmed cacheDir is a
		// private, current-user-owned directory and created it, so the
		// only thing left to get right here is not leaving a torn entry
		// behind if this process is killed mid-write; see writeCacheEntry.
		// A write failure is still silently tolerated — a full re-run is
		// always correct, just slower — but it never fails the document.
		_ = writeCacheEntry(cachePath, []byte(report.Excerpt))
	}

	base.Text = report.Excerpt
	base.Chars = report.Chars
	base.OCRUsed = report.OCRUsed
	base.NeedsReview = report.NeedsReview
	return base, Skip{}, true
}
