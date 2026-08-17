// Command composite-label walks the unlabelled rows of a composite review file
// and records one human judgement per row.
//
// The composite arm of the candidate-binding measurement cannot report anything
// until a human labels those rows: a proposal nobody reviewed is neither a
// confirmed composite nor a reviewed rejection, and the audit sample exists
// solely to bound what the proposer missed. That labelling is the one step no
// amount of engineering removes, so this command exists to make it fast rather
// than to make it optional.
//
// It writes through identitycorpus.WriteCompositeReview and only ever assigns a
// CompositeClass constant, so a label recorded here cannot be a class the
// measurement does not know, and the file it produces is the same file the
// measurement loads. Labels already present are left alone unless -relabel is
// passed, so a session can be abandoned and resumed.
//
// The evidence shown per row is Document.Text — pdf.TextReport.Excerpt, exactly
// what the identity rules read. Showing the rendered PDF instead would let a
// reviewer label something the rule cannot see, which is how ground truth
// acquires invented precision. Press o to open the PDF when the excerpt is
// genuinely not enough.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"papio/internal/identitycorpus"
)

// labelKeys maps one keystroke to the class it assigns. The letters follow the
// class names rather than the review file's print order, so the prompt can be
// read without a legend after the first few rows.
var labelKeys = []struct {
	key   string
	class identitycorpus.CompositeClass
	help  string
}{
	{"n", identitycorpus.ClassNotComposite, "not composite — this IS the paper itself"},
	{"e", identitycorpus.ClassErratum, "erratum"},
	{"c", identitycorpus.ClassCorrigendum, "corrigendum"},
	{"r", identitycorpus.ClassRetraction, "retraction notice"},
	{"m", identitycorpus.ClassComment, "comment or reply"},
	{"s", identitycorpus.ClassSupplement, "supplement / appendix / secondary file"},
	{"v", identitycorpus.ClassCoverSheet, "cover sheet / citation card"},
	{"x", identitycorpus.ClassExpansion, "journal expansion of an earlier work"},
	{"k", identitycorpus.ClassComposite, "composite, kind unclear — a confirmed composite is still confirmed"},
}

// cacheDefault mirrors identity-corpus's default cache location so a labelling
// session reuses the extraction that command already paid for. Diverging here
// would silently re-extract the whole library on every run.
func cacheDefault() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "papio", "identity-corpus")
	}
	return filepath.Join(os.TempDir(), "papio-identity-corpus")
}

func main() {
	home, homeErr := os.UserHomeDir()
	defaultZotero := ""
	if homeErr == nil {
		defaultZotero = filepath.Join(home, "Zotero")
	}

	labels := flag.String("labels", "", "path to the composite review file to label (required; produced by identity-corpus -composite-labels)")
	zotero := flag.String("zotero", defaultZotero, "Zotero data directory (containing zotero.sqlite and storage/)")
	cache := flag.String("cache", cacheDefault(), `extracted-text cache directory ("" disables caching); defaults to the cache identity-corpus fills, so labelling reuses its extraction`)
	workers := flag.Int("workers", 0, "extraction concurrency (0 = runtime.NumCPU())")
	excerpt := flag.Int("excerpt", 700, "bytes of extracted text to show per row")
	only := flag.String("only", "", `restrict to one section: "proposals" or "audit"`)
	relabel := flag.Bool("relabel", false, "revisit rows that already carry a label")
	flag.Parse()

	if *labels == "" {
		fail("-labels is required; generate the file with: go run ./cmd/identity-corpus -candidates -composite-labels <path>")
	}
	if *zotero == "" {
		fail(fmt.Sprint("-zotero not set and could not resolve home directory: ", homeErr))
	}
	switch *only {
	case "", "proposals", "audit":
	default:
		fail(`-only must be "proposals" or "audit"`)
	}

	review, err := identitycorpus.LoadCompositeReview(*labels)
	if err != nil {
		fail(err.Error())
	}

	workerCount := *workers
	if workerCount == 0 {
		workerCount = runtime.NumCPU()
	}

	// AllAttachments mirrors identity-corpus's composite-arm load. The default
	// one-per-parent corpus drops exactly the supplements and cover sheets this
	// file is about, so loading it here would leave half these rows without any
	// document to show.
	fmt.Fprintln(os.Stderr, "loading corpus (extraction is cached; the first run is the slow one)...")
	docs, _, err := identitycorpus.LoadWithOptions(context.Background(), identitycorpus.LoadOptions{
		ZoteroDir:      *zotero,
		CacheDir:       *cache,
		Workers:        workerCount,
		AllAttachments: true,
	})
	if err != nil {
		fail(err.Error())
	}
	byKey := make(map[string]identitycorpus.Document, len(docs))
	for _, d := range docs {
		byKey[d.Key] = d
	}

	type target struct {
		section string
		entry   *identitycorpus.CompositeEntry
	}
	var queue []target
	if *only != "audit" {
		for i := range review.Proposals {
			queue = append(queue, target{"proposal", &review.Proposals[i]})
		}
	}
	if *only != "proposals" {
		for i := range review.AuditSample {
			queue = append(queue, target{"audit", &review.AuditSample[i]})
		}
	}

	pending := 0
	for _, t := range queue {
		if *relabel || !t.entry.Reviewed {
			pending++
		}
	}
	if pending == 0 {
		fmt.Println("every row in scope is already labelled; pass -relabel to revisit them.")
		printTally(review)
		return
	}

	fmt.Printf("\n%d row(s) to label. One key per row; Enter alone skips a row and leaves it unlabelled.\n", pending)
	fmt.Println("Keys:")
	for _, l := range labelKeys {
		fmt.Printf("  %s  %s\n", l.key, l.help)
	}
	fmt.Println("  o  open the PDF, then ask again")
	fmt.Println("  ?  show this legend")
	fmt.Println("  q  save and quit")

	in := bufio.NewReader(os.Stdin)
	done := 0
	for _, t := range queue {
		if !*relabel && t.entry.Reviewed {
			continue
		}
		done++
		doc, haveDoc := byKey[t.entry.Key]

		for {
			printRow(t.section, done, pending, *t.entry, doc, haveDoc, *excerpt)
			fmt.Print("label> ")
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				fmt.Println()
				save(*labels, review)
				return
			}
			answer := strings.ToLower(strings.TrimSpace(line))

			switch answer {
			case "":
				fmt.Println("  (skipped, still unlabelled)")
			case "q":
				save(*labels, review)
				printTally(review)
				return
			case "?":
				for _, l := range labelKeys {
					fmt.Printf("  %s  %s\n", l.key, l.help)
				}
				continue
			case "o":
				if !haveDoc {
					fmt.Println("  no document loaded for this key; cannot open")
					continue
				}
				if err := open(doc.Path); err != nil {
					fmt.Println("  could not open:", err)
				}
				continue
			}

			if answer != "" {
				class, ok := classFor(answer)
				if !ok {
					fmt.Println("  unrecognised key; ? for the legend")
					continue
				}
				t.entry.Reviewed = true
				t.entry.Class = class
				// A confirmed composite refers to the work it is about. The
				// proposer's guess is the starting point rather than the answer,
				// but an unreviewed guess must not become ground truth, so it is
				// only inherited once a human has confirmed the class.
				if class.IsComposite() && len(t.entry.RefersTo) == 0 {
					t.entry.RefersTo = append([]string(nil), t.entry.ProposedRefersTo...)
				}
				fmt.Printf("  recorded: %s\n", class)
				// Save after every row: an hour of judgement must not depend on
				// this process exiting cleanly.
				save(*labels, review)
			}
			break
		}
	}
	printTally(review)
}

func classFor(key string) (identitycorpus.CompositeClass, bool) {
	for _, l := range labelKeys {
		if l.key == key {
			return l.class, true
		}
	}
	return identitycorpus.ClassUnlabelled, false
}

func printRow(section string, n, total int, e identitycorpus.CompositeEntry, doc identitycorpus.Document, haveDoc bool, excerpt int) {
	fmt.Printf("\n─── %s %d/%d ───────────────────────────────────────────\n", section, n, total)
	fmt.Printf("title    %s\n", e.Title)
	if section == "proposal" {
		fmt.Printf("proposed %s\n", e.Proposed)
		names := make([]string, 0, len(e.Signals))
		for _, s := range e.Signals {
			names = append(names, s.Name)
		}
		if len(names) > 0 {
			fmt.Printf("signals  %s\n", strings.Join(names, ", "))
		}
		for _, s := range e.Signals {
			if s.Detail != "" {
				fmt.Printf("         %s: %s\n", s.Name, s.Detail)
			}
		}
	} else {
		fmt.Println("proposed (not flagged — you are checking whether the proposer missed one)")
	}
	flags := []string{}
	if e.Secondary {
		flags = append(flags, "not its parent's primary PDF")
	}
	if e.DOILess {
		flags = append(flags, "no DOI")
	}
	if haveDoc && doc.OCRUsed {
		flags = append(flags, "text came from OCR")
	}
	if len(flags) > 0 {
		fmt.Printf("note     %s\n", strings.Join(flags, "; "))
	}
	if e.Reviewed {
		fmt.Printf("existing %s\n", e.Class)
	}
	if !haveDoc {
		fmt.Println("text     (no document loaded for this key)")
		return
	}
	fmt.Println("text     ── first bytes of exactly what the identity rules read ──")
	for _, line := range firstLines(doc.Text, excerpt) {
		fmt.Printf("         %s\n", line)
	}
}

// firstLines returns the excerpt's leading bytes as trimmed, non-empty lines.
// Blank runs are dropped because pdftotext emits many and they would push the
// only informative lines off the screen.
func firstLines(text string, budget int) []string {
	if budget > len(text) {
		budget = len(text)
	}
	var out []string
	for _, raw := range strings.Split(text[:budget], "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if len(line) > 100 {
			line = line[:100] + "…"
		}
		out = append(out, line)
	}
	return out
}

func open(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	default:
		return errors.New("opening files is not wired up for " + runtime.GOOS)
	}
	return cmd.Start()
}

func save(path string, review identitycorpus.CompositeReview) {
	if err := identitycorpus.WriteCompositeReview(path, review); err != nil {
		fmt.Fprintln(os.Stderr, "composite-label: could not write labels:", err)
		os.Exit(1)
	}
}

func printTally(review identitycorpus.CompositeReview) {
	counts := map[identitycorpus.CompositeClass]int{}
	unlabelled := 0
	confirmedAudit := 0
	for _, e := range review.Proposals {
		if !e.Reviewed {
			unlabelled++
			continue
		}
		counts[e.Class]++
	}
	for _, e := range review.AuditSample {
		if !e.Reviewed {
			unlabelled++
			continue
		}
		counts[e.Class]++
		if e.Class.IsComposite() {
			confirmedAudit++
		}
	}
	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, string(c))
	}
	sort.Strings(classes)

	fmt.Println("\n─── tally ───")
	for _, c := range classes {
		fmt.Printf("  %-20s %d\n", c, counts[identitycorpus.CompositeClass(c)])
	}
	fmt.Printf("  %-20s %d\n", "still unlabelled", unlabelled)
	if confirmedAudit > 0 {
		fmt.Printf("\n%d audit row(s) are composites the proposer missed — that is the recall bound the arm needs.\n", confirmedAudit)
	}
	fmt.Println("\nRe-run the measurement to consume these labels:")
	fmt.Println("  go run ./cmd/identity-corpus -candidates -composite-labels <path>")
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "composite-label:", msg)
	os.Exit(2)
}
