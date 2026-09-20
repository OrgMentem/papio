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
