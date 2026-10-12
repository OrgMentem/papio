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
	if err := first.Publish(); err != nil {
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
	if err := second.Publish(); err != nil {
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
