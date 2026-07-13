// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPromoteIsAtomicIdempotentAndVerifiable(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	temp := filepath.Join(q, "download.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := HashFile(temp)
	if err != nil || size != int64(len(body)) {
		t.Fatalf("hash: %v size=%d", err, size)
	}

	dest, err := s.Promote(temp, sha)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temp file survived promotion")
	}
	if err := s.Verify(sha); err != nil {
		t.Fatalf("verify: %v", err)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("artifact mode = %v, want read-only 0400", info.Mode().Perm())
	}

	// Second promotion of identical content is a no-op (crash-recovery re-fetch).
	temp2 := filepath.Join(q, "download2.tmp")
	if err := os.WriteFile(temp2, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dest2, err := s.Promote(temp2, sha)
	if err != nil || dest2 != dest {
		t.Fatalf("re-promote = %q, %v; want same path, no error", dest2, err)
	}

	// Wrong-hash promotion is refused.
	temp3 := filepath.Join(q, "download3.tmp")
	_ = os.WriteFile(temp3, []byte("different"), 0o600)
	if _, err := s.Promote(temp3, sha); err == nil {
		t.Fatal("promoted mismatched content")
	}
}

func TestConfineRegularFile(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a.pdf")
	_ = os.WriteFile(inside, []byte("x"), 0o600)
	if err := ConfineRegularFile(root, inside); err != nil {
		t.Fatalf("regular file inside root rejected: %v", err)
	}
	if err := ConfineRegularFile(root, filepath.Join(root, "..", "escape.pdf")); err == nil {
		t.Fatal("escaping path accepted")
	}
	link := filepath.Join(root, "link.pdf")
	if err := os.Symlink(inside, link); err == nil {
		if err := ConfineRegularFile(root, link); err == nil {
			t.Fatal("symlink accepted")
		}
	}
	if err := ConfineRegularFile(root, root); err == nil {
		t.Fatal("directory accepted")
	}
}
