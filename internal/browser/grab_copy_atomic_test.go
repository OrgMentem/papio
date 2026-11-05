// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Every grab copy lands under an adoption name a scanner may read at any
// moment, so copyGrabFile writes a dot-prefixed temporary and publishes it by
// hard link. These tests pin what makes a kill mid-copy survivable — an
// abandoned temporary is invisible to adoption rather than ambiguous, and a
// completed copy leaves nothing behind but the file itself — and that
// publication never writes over a destination.

// stagingTempNames lists the dot-prefixed staging temporaries in dir.
func stagingTempNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// A daemon killed mid-copy leaves its partially written temporary behind. It
// must not be adoptable (a truncated PDF still passes the five-byte header
// probe, so as a visible file it would be adopted, fail validation, and park
// the job asking a human to re-supply a file papio already holds) and it must
// not make the directory ambiguous for the retry that follows.
func TestAbandonedGrabStagingTempIsInvisibleToAdoption(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_staging_stump", handoffWork())
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stump := filepath.Join(dir, ".papio-stage-123456.tmp")
	if err := os.WriteFile(stump, []byte("%PDF-1.4\ntruncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if name, ok := b.scanAdoptionDir(ctx, id); ok {
		t.Fatalf("adoption scan selected the abandoned staging temp %q", name)
	}

	// The retry stages the full bytes beside the stump and must still produce
	// exactly one adoptable file.
	src := filepath.Join(cfg.DataDir, "quarantine", id, "grab.pdf")
	writeFixturePDF(t, src)
	name, created, _, err := b.stageGrabCopy(src, dir, "paper.pdf")
	if err != nil {
		t.Fatalf("stageGrabCopy: %v", err)
	}
	if !created || name != "paper.pdf" {
		t.Fatalf("stageGrabCopy = %q, created %v; want paper.pdf, true (the stump must not be reused)", name, created)
	}
	got, ok := b.scanAdoptionDir(ctx, id)
	if !ok || got != "paper.pdf" {
		t.Fatalf("adoption scan = %q, %v; want paper.pdf, true (a dot-prefixed stump must not make the directory ambiguous)", got, ok)
	}
}

// A completed copy leaves the destination and nothing else: no temporary for a
// later sweep to carry, and no second copy of the bytes.
func TestCopyGrabFileLeavesNoStagingTemp(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	src := filepath.Join(cfg.DataDir, "src", "grab.pdf")
	writeFixturePDF(t, src)
	dir := filepath.Join(cfg.DataDir, "dest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "paper.pdf")

	if err := b.copyGrabFile(src, dst); err != nil {
		t.Fatalf("copyGrabFile: %v", err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination missing after a successful copy: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination holds %d bytes, want the source's %d", len(got), len(want))
	}
	if leftover := stagingTempNames(t, dir); len(leftover) != 0 {
		t.Fatalf("staging temporaries left behind: %v", leftover)
	}
}

// linkUnsupported reports hard links unsupported, as link(2) does on FAT/exFAT
// and some network shares. copyGrabFile then publishes with the platform's
// real exclusive rename on the test's own (APFS/ext4/tmpfs) volume; macOS
// exFAT refuses that rename too, which is the fail-closed case.
func linkUnsupported(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EOPNOTSUPP}
}

// Publication keeps O_EXCL's promise never to write over a destination,
// including one created after uniqueAdoptionDest chose the name and while the
// copy was already under way: the refusal must leave that file untouched and
// no temporary behind. A rename, or a check followed by a rename, replaces the
// file raced in at the moment of publication. The WithoutHardLinks cases run
// the real exclusive rename (renameNoReplace), not a stand-in.
func TestCopyGrabFileNeverOverwritesADestination(t *testing.T) {
	const theirs = "a file somebody else owns"
	// raceIn creates dst at the publication instant, then publishes with next.
	raceIn := func(t *testing.T, dst string, next func(string, string) error) func(string, string) error {
		return func(oldname, newname string) error {
			if err := os.WriteFile(dst, []byte(theirs), 0o600); err != nil {
				t.Fatal(err)
			}
			return next(oldname, newname)
		}
	}
	cases := []struct {
		name     string
		existing bool // dst exists before the copy starts
		link     func(*testing.T, string) func(string, string) error
	}{
		{name: "ExistsBeforeCopy", existing: true},
		{name: "CreatedAtPublication", link: func(t *testing.T, dst string) func(string, string) error {
			return raceIn(t, dst, os.Link)
		}},
		{name: "ExistsWithoutHardLinks", existing: true, link: func(*testing.T, string) func(string, string) error {
			return linkUnsupported
		}},
		{name: "CreatedAtPublicationWithoutHardLinks", link: func(t *testing.T, dst string) func(string, string) error {
			return raceIn(t, dst, linkUnsupported)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, cfg, _ := newBridge(t)
			src := filepath.Join(cfg.DataDir, "src", "grab.pdf")
			writeFixturePDF(t, src)
			dir := filepath.Join(cfg.DataDir, "dest")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "paper.pdf")
			if tc.existing {
				if err := os.WriteFile(dst, []byte(theirs), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.link != nil {
				b.linkFile = tc.link(t, dst)
			}

			err := b.copyGrabFile(src, dst)
			if !errors.Is(err, os.ErrExist) {
				t.Fatalf("copyGrabFile over an existing destination = %v, want os.ErrExist", err)
			}
			got, readErr := os.ReadFile(dst)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != theirs {
				t.Fatalf("existing destination was overwritten: %q", got)
			}
			if leftover := stagingTempNames(t, dir); len(leftover) != 0 {
				t.Fatalf("refused copy left staging temporaries behind: %v", leftover)
			}
		})
	}
}

// A volume without hard links still receives the grab: the real exclusive
// rename publishes the complete bytes and leaves no temporary behind.
func TestCopyGrabFileWithoutHardLinksStillCopies(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	b.linkFile = linkUnsupported
	src := filepath.Join(cfg.DataDir, "src", "grab.pdf")
	writeFixturePDF(t, src)
	dir := filepath.Join(cfg.DataDir, "dest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "paper.pdf")

	if err := b.copyGrabFile(src, dst); err != nil {
		t.Fatalf("copyGrabFile without hard links: %v", err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination missing after the exclusive rename: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination holds %d bytes, want the source's %d", len(got), len(want))
	}
	if leftover := stagingTempNames(t, dir); len(leftover) != 0 {
		t.Fatalf("exclusive rename left staging temporaries behind: %v", leftover)
	}
}

// A volume with neither hard links nor an exclusive rename gets no file at the
// adoption name at all: writing it in place is the partial file a kill leaves
// behind. The copy fails, the source bytes are untouched for a retry, and no
// temporary is left.
func TestCopyGrabFileFailsClosedWithoutANoReplacePrimitive(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	b.linkFile = linkUnsupported
	b.renameNoReplaceFile = func(oldname, newname string) error {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: errors.ErrUnsupported}
	}
	src := filepath.Join(cfg.DataDir, "src", "grab.pdf")
	writeFixturePDF(t, src)
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataDir, "dest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "paper.pdf")

	err = b.copyGrabFile(src, dst)
	if err == nil || errors.Is(err, os.ErrExist) {
		t.Fatalf("copyGrabFile without any no-replace primitive = %v, want a non-ErrExist failure", err)
	}
	if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("destination created without a no-replace primitive: %v", statErr)
	}
	if got, readErr := os.ReadFile(src); readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("source bytes changed by a failed copy: %d bytes, %v", len(got), readErr)
	}
	if leftover := stagingTempNames(t, dir); len(leftover) != 0 {
		t.Fatalf("failed copy left staging temporaries behind: %v", leftover)
	}
}
