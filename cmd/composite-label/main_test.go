package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"papio/internal/identitycorpus"
)

// TestLaunchReapsExitedChild pins that opening a file does not leave a zombie
// per keypress: the child launched without blocking the review loop must still
// be waited on, which is what records its ProcessState.
func TestLaunchReapsExitedChild(t *testing.T) {
	if os.Getenv("COMPOSITE_LABEL_HELPER") == "1" {
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLaunchReapsExitedChild$")
	cmd.Env = append(os.Environ(), "COMPOSITE_LABEL_HELPER=1")
	done, err := launch(cmd)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("exited child was never reaped")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("ProcessState = %v, want a reaped exit", cmd.ProcessState)
	}
}

// runTestSession drives the session with the real save path against a review
// file on disk, then reloads that file so assertions see what a later resume
// would see rather than the in-memory entries.
func runTestSession(t *testing.T, input string, entries []identitycorpus.CompositeEntry, docs map[string]identitycorpus.Document, relabel bool, openFn func(string) error) identitycorpus.CompositeReview {
	t.Helper()
	path := filepath.Join(t.TempDir(), "labels.json")
	review := identitycorpus.CompositeReview{Proposals: entries}
	if err := identitycorpus.WriteCompositeReview(path, review); err != nil {
		t.Fatalf("seeding review: %v", err)
	}
	var queue []target
	for i := range review.Proposals {
		queue = append(queue, target{"proposal", &review.Proposals[i]})
	}
	runSession(bufio.NewReader(strings.NewReader(input)), queue, docs, path, &review, relabel, 700, len(queue), save, openFn)
	reloaded, err := identitycorpus.LoadCompositeReview(path)
	if err != nil {
		t.Fatalf("reloading review: %v", err)
	}
	return reloaded
}

func TestLabelSessionRecordsClassesAndInheritsReferent(t *testing.T) {
	entries := []identitycorpus.CompositeEntry{
		{Key: "A", ProposedRefersTo: []string{"PAPER1"}},
		{Key: "B", ProposedRefersTo: []string{"PAPER2"}},
		{Key: "C"},
	}
	// A: invalid key then erratum. B: skipped. C: not composite.
	review := runTestSession(t, "zz\nE\n\nn\n", entries, nil, false, nil)
	a, b, c := review.Proposals[0], review.Proposals[1], review.Proposals[2]
	if !a.Reviewed || a.Class != identitycorpus.ClassErratum || !reflect.DeepEqual(a.RefersTo, []string{"PAPER1"}) {
		t.Fatalf("A = %+v, want reviewed erratum inheriting PAPER1", a)
	}
	if b.Reviewed || b.Class != identitycorpus.ClassUnlabelled || len(b.RefersTo) != 0 {
		t.Fatalf("B = %+v, want skipped row untouched (proposal guess must not become ground truth)", b)
	}
	if !c.Reviewed || c.Class != identitycorpus.ClassNotComposite || len(c.RefersTo) != 0 {
		t.Fatalf("C = %+v, want reviewed not_composite with no referent", c)
	}
}

func TestLabelSessionNotCompositeDoesNotInheritReferent(t *testing.T) {
	entries := []identitycorpus.CompositeEntry{{Key: "A", ProposedRefersTo: []string{"PAPER1"}}}
	review := runTestSession(t, "n\n", entries, nil, false, nil)
	if got := review.Proposals[0]; !got.Reviewed || len(got.RefersTo) != 0 {
		t.Fatalf("A = %+v, want reviewed with no inherited referent", got)
	}
}

func TestLabelSessionKeepsExistingReferentAndResumes(t *testing.T) {
	entries := []identitycorpus.CompositeEntry{
		{Key: "A", Reviewed: true, Class: identitycorpus.ClassErratum, RefersTo: []string{"X"}},
		{Key: "B", ProposedRefersTo: []string{"P"}, RefersTo: []string{"HUMAN"}},
	}
	// Without relabel the reviewed row is not asked.
	review := runTestSession(t, "c\nq\n", entries, nil, false, nil)
	if a := review.Proposals[0]; a.Class != identitycorpus.ClassErratum {
		t.Fatalf("reviewed row was revisited without -relabel: %+v", a)
	}
	if b := review.Proposals[1]; b.Class != identitycorpus.ClassCorrigendum || !reflect.DeepEqual(b.RefersTo, []string{"HUMAN"}) {
		t.Fatalf("B = %+v, want corrigendum keeping the human referent", b)
	}
	// q before any label still persists and leaves rows unlabelled.
	review = runTestSession(t, "q\n", []identitycorpus.CompositeEntry{{Key: "Z"}}, nil, false, nil)
	if review.Proposals[0].Reviewed {
		t.Fatal("q labelled a row")
	}
	// With relabel the reviewed row is asked again.
	review = runTestSession(t, "r\n", []identitycorpus.CompositeEntry{{Key: "A", Reviewed: true, Class: identitycorpus.ClassErratum}}, nil, true, nil)
	if review.Proposals[0].Class != identitycorpus.ClassRetraction {
		t.Fatalf("relabel left class %q", review.Proposals[0].Class)
	}
}

// Input that ends with rows still pending must leave the labels recorded so
// far on disk and the remaining rows unlabelled, so the session can resume.
func TestLabelSessionEOFWithRowsPendingPersistsAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.json")
	review := identitycorpus.CompositeReview{Proposals: []identitycorpus.CompositeEntry{
		{Key: "A", ProposedRefersTo: []string{"P"}}, {Key: "B"}, {Key: "C"},
	}}
	if err := identitycorpus.WriteCompositeReview(path, review); err != nil {
		t.Fatal(err)
	}
	var queue []target
	for i := range review.Proposals {
		queue = append(queue, target{"proposal", &review.Proposals[i]})
	}
	runSession(bufio.NewReader(strings.NewReader("k\n")), queue, nil, path, &review, false, 700, 3, save, nil)
	first, err := identitycorpus.LoadCompositeReview(path)
	if err != nil {
		t.Fatal(err)
	}
	if a := first.Proposals[0]; !a.Reviewed || a.Class != identitycorpus.ClassComposite || !reflect.DeepEqual(a.RefersTo, []string{"P"}) {
		t.Fatalf("reloaded A = %+v, want the saved composite label with its referent", a)
	}
	if first.Proposals[1].Reviewed || first.Proposals[2].Reviewed {
		t.Fatal("rows past EOF were labelled")
	}
	// Resume from the reloaded file: only the unlabelled rows are asked.
	queue = queue[:0]
	for i := range first.Proposals {
		queue = append(queue, target{"proposal", &first.Proposals[i]})
	}
	runSession(bufio.NewReader(strings.NewReader("n\nn\n")), queue, nil, path, &first, false, 700, 2, save, nil)
	second, err := identitycorpus.LoadCompositeReview(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Proposals[0].Class != identitycorpus.ClassComposite || !second.Proposals[1].Reviewed || !second.Proposals[2].Reviewed {
		t.Fatalf("resumed review = %+v", second.Proposals)
	}
}

func TestLabelSessionOpenHandling(t *testing.T) {
	opened := 0
	openFn := func(path string) error {
		opened++
		if path != "/doc.pdf" {
			t.Fatalf("opened %q", path)
		}
		return errors.New("no opener")
	}
	docs := map[string]identitycorpus.Document{"A": {Key: "A", Path: "/doc.pdf", Text: "x"}}
	// o on a row with no document never calls the opener; o on a loaded row
	// calls it, survives its error, and asks again.
	review := runTestSession(t, "o\ns\no\n?\nv\n", []identitycorpus.CompositeEntry{{Key: "NODOC"}, {Key: "A"}}, docs, false, openFn)
	if opened != 1 {
		t.Fatalf("opener calls = %d, want 1", opened)
	}
	if review.Proposals[0].Class != identitycorpus.ClassSupplement || review.Proposals[1].Class != identitycorpus.ClassCoverSheet {
		t.Fatalf("classes = %q, %q", review.Proposals[0].Class, review.Proposals[1].Class)
	}
}

func TestClassFor(t *testing.T) {
	for _, l := range labelKeys {
		if got, ok := classFor(l.key); !ok || got != l.class {
			t.Fatalf("classFor(%q) = %q, %v", l.key, got, ok)
		}
	}
	for _, key := range []string{"", "z", "nn", "N"} {
		if got, ok := classFor(key); ok || got != identitycorpus.ClassUnlabelled {
			t.Fatalf("classFor(%q) = %q, %v, want unlabelled/false", key, got, ok)
		}
	}
}

func TestFirstLines(t *testing.T) {
	long := strings.Repeat("a", 120)
	cases := []struct {
		name   string
		text   string
		budget int
		want   []string
	}{
		{"blank runs dropped and trimmed", "  one \n\n\n two\n", 100, []string{"one", "two"}},
		{"budget cuts the text", "abc\ndef\nghi", 5, []string{"abc", "d"}},
		{"budget larger than text", "abc", 999, []string{"abc"}},
		{"zero budget", "abc", 0, nil},
		{"long line truncated", long, 500, []string{strings.Repeat("a", 100) + "…"}},
		{"exactly 100 kept", strings.Repeat("b", 100), 500, []string{strings.Repeat("b", 100)}},
	}
	for _, tc := range cases {
		if got := firstLines(tc.text, tc.budget); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
