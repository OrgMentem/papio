// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFolderNeverOverwritesConflictOrFollowsSymlink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source.pdf")
	body := []byte("%PDF-1.4\nverified fixture\n%%EOF")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	sha := hex.EncodeToString(h[:])
	folder := filepath.Join(root, "collection")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	key := Key("job_one", folder, sha)
	target := filepath.Join(folder, key+".pdf")
	other := []byte("unrelated owned document")
	if err := os.WriteFile(target, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, source, folder, key, sha); !errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("conflict = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(other) {
		t.Fatalf("conflict changed target: %q, %v", got, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Stage(ctx, source, folder, key, sha); !errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("symlink target = %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, target, folder, key, sha); !errors.Is(err, ErrArtifact) {
		t.Fatalf("missing source = %v", err)
	}
	link := filepath.Join(root, "source-link.pdf")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(ctx, link, folder, key, sha); !errors.Is(err, ErrArtifact) {
		t.Fatalf("symlink source = %v", err)
	}
}

func TestFolderReplayChecksExistingBytesAndDoesNotRepublish(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.pdf")
	body := []byte("%PDF-1.4\nreplay fixture\n%%EOF")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	sha := hex.EncodeToString(h[:])
	folder := filepath.Join(root, "collection")
	key := Key("job_replay", folder, sha)
	first, err := Stage(context.Background(), source, folder, key, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Cleanup()
	before, err := os.Stat(first.Target)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Stage(context.Background(), source, folder, key, sha)
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != "" || second.Existing == nil {
		t.Fatalf("replay stages new bytes: %+v", second)
	}
	if err := second.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(first.Target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("replay replaces inode: %v", err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) != 1 || entries[0].Name() != key+".pdf" {
		t.Fatalf("replay artifacts = %v, %v", entries, err)
	}
}

func stageFixture(t *testing.T, body []byte, sha string) (source, folder, key string) {
	t.Helper()
	root := t.TempDir()
	source = filepath.Join(root, "source.pdf")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	folder = filepath.Join(root, "collection")
	return source, folder, Key("job_pin", folder, sha)
}

func TestStageRejectsSourceBytesThatAreNotTheValidatedArtifact(t *testing.T) {
	good := []byte("%PDF-1.4\nvalidated artifact\n%%EOF")
	sum := sha256.Sum256(good)
	sha := hex.EncodeToString(sum[:])
	empty := sha256.Sum256(nil)
	for name, tc := range map[string]struct {
		body []byte
		sha  string
	}{
		"corrupted or swapped bytes": {[]byte("%PDF-1.4\nnot the validated artifact\n%%EOF"), sha},
		"empty source":               {nil, sha},
		// The hash of nothing matches an empty source, so only the zero-length
		// guard refuses it.
		"empty source with the empty hash": {nil, hex.EncodeToString(empty[:])},
	} {
		t.Run(name, func(t *testing.T) {
			source, folder, key := stageFixture(t, tc.body, tc.sha)
			if _, err := Stage(context.Background(), source, folder, key, tc.sha); !errors.Is(err, ErrArtifact) {
				t.Fatalf("Stage = %v, want ErrArtifact", err)
			}
			if entries, err := os.ReadDir(folder); err != nil || len(entries) != 0 {
				t.Fatalf("rejected Stage left %v, %v in the folder", entries, err)
			}
		})
	}
}

// Publish must refuse a replay whose verified target changed after Stage. Each
// case changes exactly one property the guard compares.
func TestPublishRefusesReplayTargetChangedAfterStage(t *testing.T) {
	body := []byte("%PDF-1.4\nreplay guard fixture\n%%EOF")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	for name, mutate := range map[string]func(t *testing.T, target string, original os.FileInfo){
		"new inode, same bytes": func(t *testing.T, target string, original os.FileInfo) {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, original.ModTime(), original.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"same inode, new size": func(t *testing.T, target string, original os.FileInfo) {
			if err := os.Chmod(target, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("x"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, original.ModTime(), original.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"same inode and size, new mtime": func(t *testing.T, target string, original os.FileInfo) {
			later := original.ModTime().Add(time.Hour)
			if err := os.Chtimes(target, later, later); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			source, folder, key := stageFixture(t, body, sha)
			first, err := Stage(ctx, source, folder, key, sha)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Publish(ctx); err != nil {
				t.Fatal(err)
			}
			first.Cleanup()
			replay, err := Stage(ctx, source, folder, key, sha)
			if err != nil || replay.Existing == nil {
				t.Fatalf("replay Stage = %+v, %v", replay, err)
			}
			mutate(t, replay.Target, replay.Existing)
			if err := replay.Publish(ctx); !errors.Is(err, ErrDestinationConflict) {
				t.Fatalf("Publish over a changed target = %v, want ErrDestinationConflict", err)
			}
		})
	}
}

func TestCleanupStagesRemovesOnlyThisReceiptsPartials(t *testing.T) {
	folder := t.TempDir()
	key, other := "keyaaaa", "keybbbb"
	mine := filepath.Join(folder, ".papio-"+key+"-x.partial")
	files := map[string]bool{
		mine: false,
		filepath.Join(folder, ".papio-"+other+"-y.partial"): true,
		filepath.Join(folder, key+".pdf"):                   true,
	}
	for path := range files {
		if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink matching the glob is not a regular file and must be skipped.
	link := filepath.Join(folder, ".papio-"+key+"-link.partial")
	linked := filepath.Join(folder, "linked-target.pdf")
	if err := os.WriteFile(linked, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	files[link], files[linked] = true, true

	if err := CleanupStages(context.Background(), folder, key); err != nil {
		t.Fatal(err)
	}
	for path, survives := range files {
		_, err := os.Lstat(path)
		if survives && err != nil {
			t.Fatalf("CleanupStages removed %s: %v", filepath.Base(path), err)
		}
		if !survives && !os.IsNotExist(err) {
			t.Fatalf("CleanupStages left %s: %v", filepath.Base(path), err)
		}
	}
}
