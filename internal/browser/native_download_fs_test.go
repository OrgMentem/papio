// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/config"
)

func nativeRootFixture(t *testing.T, before func(string)) (*nativeDownloadRoot, string) {
	t.Helper()
	source := t.TempDir()
	landing := filepath.Join(source, "papio")
	if err := os.Mkdir(landing, 0700); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(source)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Browser.AdoptionRoot = landing
	r, err := snapshotNativeDownloadRoot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r, source
}
func TestNativeDownloadBaselineNameSurvivesReplacement(t *testing.T) {
	r, source := nativeRootFixture(t, func(root string) { writeAdoptionProbeFile(t, filepath.Join(root, "old.pdf"), []byte("old")) })
	path := filepath.Join(source, "old.pdf")
	replacement := filepath.Join(source, "replacement.pdf")
	writeAdoptionProbeFile(t, replacement, []byte("new content"))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.stage(context.Background(), path, "native-test", 11, 100); !errors.Is(err, errNativeSource) {
		t.Fatalf("preexisting name imported after replacement: %v", err)
	}
}
func TestNativeDownloadSourceConfinement(t *testing.T) {
	for _, condition := range []string{"outside", "nested", "traversal", "symlink", "hardlink", "part suffix", "part sibling", "size mismatch", "size cap", "renamed baseline", "root replaced", "landing replaced"} {
		t.Run(condition, func(t *testing.T) {
			r, source := nativeRootFixture(t, func(root string) { writeAdoptionProbeFile(t, filepath.Join(root, "old.pdf"), []byte("old")) })
			path := filepath.Join(source, "fresh.pdf")
			writeAdoptionProbeFile(t, path, []byte("new"))
			size, max := int64(3), int64(100)
			switch condition {
			case "outside":
				path = filepath.Join(t.TempDir(), "fresh.pdf")
				writeAdoptionProbeFile(t, path, []byte("new"))
			case "nested":
				path = filepath.Join(source, "nested", "fresh.pdf")
				writeAdoptionProbeFile(t, path, []byte("new"))
			case "traversal":
				path = source + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(source) + string(filepath.Separator) + "fresh.pdf"
			case "symlink":
				target := path
				path = filepath.Join(source, "alias.pdf")
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(source, "link.pdf")); err != nil {
					t.Skipf("hardlink unavailable: %v", err)
				}
			case "part suffix":
				path = filepath.Join(source, "fresh.part")
				writeAdoptionProbeFile(t, path, []byte("new"))
			case "part sibling":
				writeAdoptionProbeFile(t, path+".part", []byte("new"))
			case "size mismatch":
				size = 4
			case "size cap":
				max = 2
			case "renamed baseline":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(source, "old.pdf"), path); err != nil {
					t.Fatal(err)
				}
			case "root replaced":
				moved := source + "-moved"
				if err := os.Rename(source, moved); err != nil {
					t.Skipf("open root rename unavailable: %v", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(moved) })
				writeAdoptionProbeFile(t, path, []byte("new"))
			case "landing replaced":
				if err := os.Rename(filepath.Join(source, "papio"), filepath.Join(source, "papio-old")); err != nil {
					t.Skipf("open root rename unavailable: %v", err)
				}
				if err := os.Mkdir(filepath.Join(source, "papio"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.stage(context.Background(), path, "native-test", size, max); !errors.Is(err, errNativeSource) {
				t.Fatalf("unconfined source imported: %v", err)
			}
		})
	}
}
func TestNativeDownloadPublishRejectsSymlinkJobAndExistingFile(t *testing.T) {
	for _, condition := range []string{"symlink job", "existing file", "stage changed"} {
		t.Run(condition, func(t *testing.T) {
			r, source := nativeRootFixture(t, nil)
			path := filepath.Join(source, "fresh.pdf")
			writeAdoptionProbeFile(t, path, []byte("new"))
			staged, err := r.stage(context.Background(), path, "native-test", 3, 100)
			if err != nil {
				t.Fatal(err)
			}
			jobDir := filepath.Join(source, "papio", "job_test")
			switch condition {
			case "symlink job":
				if err := os.Symlink(t.TempDir(), jobDir); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "existing file":
				writeAdoptionProbeFile(t, filepath.Join(jobDir, "native-test.pdf"), []byte("existing"))
			case "stage changed":
				writeAdoptionProbeFile(t, filepath.Join(source, "papio", staged.name), []byte("bad"))
			}
			if err := r.publish(context.Background(), "job_test", "native-test.pdf", staged); !errors.Is(err, errNativeSource) {
				t.Fatalf("publication escaped guard: %v", err)
			}
			if condition == "existing file" {
				got, err := os.ReadFile(filepath.Join(jobDir, "native-test.pdf"))
				if err != nil || string(got) != "existing" {
					t.Fatalf("existing file overwritten: %s %v", got, err)
				}
			}
		})
	}
}

// stubNativeDirSync replaces the directory flush for one test. observe, when
// non-nil, runs on every flush with the flushed directory's base name; fail,
// when non-nil, decides per flush whether it errors.
func stubNativeDirSync(t *testing.T, observe func(dir string), fail func(dir string) bool) {
	t.Helper()
	previous := syncNativeDir
	t.Cleanup(func() { syncNativeDir = previous })
	syncNativeDir = func(root *os.Root) error {
		dir := filepath.Base(root.Name())
		if observe != nil {
			observe(dir)
		}
		if fail != nil && fail(dir) {
			return errors.New("injected directory flush failure")
		}
		return nil
	}
}

// TestNativeDownloadPublishFlushesDirectoryEntries pins the durability order of
// publish: the job directory's entry in the landing directory is flushed before
// the file lands in it, and the job directory is flushed AFTER the final link
// and AFTER the partial name is gone. The link is a directory entry the file's
// own fsync does not cover, and a crash that kept both the final name and its
// .part twin would make scanAdoptionDir defer the directory for good. Admission
// and ingest are durable records that rely on that name surviving power loss
// (ledger finding papio-9d536edcf322d81a).
func TestNativeDownloadPublishFlushesDirectoryEntries(t *testing.T) {
	r, source := nativeRootFixture(t, nil)
	path := filepath.Join(source, "fresh.pdf")
	writeAdoptionProbeFile(t, path, []byte("new"))
	staged, err := r.stage(context.Background(), path, "native-test", 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(source, "papio", "job_test", "native-test.pdf")
	type call struct {
		dir                       string
		finalPresent, partPresent bool
	}
	exists := func(path string) bool { _, err := os.Lstat(path); return err == nil }
	var calls []call
	stubNativeDirSync(t, func(dir string) {
		calls = append(calls, call{dir, exists(final), exists(final + ".part")})
	}, nil)
	if err := r.publish(context.Background(), "job_test", "native-test.pdf", staged); err != nil {
		t.Fatal(err)
	}
	want := []call{{"papio", false, false}, {"job_test", true, false}}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("directory flushes = %+v, want %+v: the landing entry before the file, the job directory after the link and after the .part is removed", calls, want)
	}
}

// TestNativeDownloadPublishWithdrawsLinkWhenFlushFails pins the failure side:
// a name whose directory entry could not be made durable must not stay behind
// as a published file, and the retained staging must be republishable.
func TestNativeDownloadPublishWithdrawsLinkWhenFlushFails(t *testing.T) {
	for _, failing := range []string{"papio", "job_test"} {
		t.Run(failing, func(t *testing.T) {
			r, source := nativeRootFixture(t, nil)
			path := filepath.Join(source, "fresh.pdf")
			writeAdoptionProbeFile(t, path, []byte("new"))
			staged, err := r.stage(context.Background(), path, "native-test", 3, 100)
			if err != nil {
				t.Fatal(err)
			}
			jobDir := filepath.Join(source, "papio", "job_test")
			final := filepath.Join(jobDir, "native-test.pdf")
			stubNativeDirSync(t, nil, func(dir string) bool { return dir == failing })
			if err := r.publish(context.Background(), "job_test", "native-test.pdf", staged); !errors.Is(err, errNativeSource) {
				t.Fatalf("publish with a failing %s flush = %v, want errNativeSource", failing, err)
			}
			if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published name survived a failed flush: %v", err)
			}
			if _, err := os.Lstat(final + ".part"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial copy survived a failed publish: %v", err)
			}
			stubNativeDirSync(t, nil, nil)
			if err := r.publish(context.Background(), "job_test", "native-test.pdf", staged); err != nil {
				t.Fatalf("republish from retained staging: %v", err)
			}
			if got, err := os.ReadFile(final); err != nil || string(got) != "new" {
				t.Fatalf("republished file = %q, %v", got, err)
			}
		})
	}
}
