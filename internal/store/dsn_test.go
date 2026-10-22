// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// URI-special characters in the data directory must neither redirect the
// database to a different file nor drop the pragmas.
func TestOpenDataDirWithURISpecialCharacters(t *testing.T) {
	for _, name := range []string{"Research#2026", "what?now", "100%25done", "a%20b#c?d&e=f"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, name, "papio")
			s, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			if _, err := os.Stat(filepath.Join(dir, "papio.db")); err != nil {
				t.Fatalf("database not created at the requested path: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("stray entries beside the data dir: %v", entries)
			}

			var fk int
			if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
				t.Fatal(err)
			}
			if fk != 1 {
				t.Fatalf("foreign_keys = %d, want 1 (pragmas dropped from the DSN)", fk)
			}
			var mode string
			if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
				t.Fatal(err)
			}
			if mode != "wal" {
				t.Fatalf("journal_mode = %q, want wal", mode)
			}
		})
	}
}

func TestFileDSNReadOnlyRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Research#2026?x")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ro, err := sql.Open("sqlite", FileDSN(s.Path(), "mode=ro&_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		t.Fatalf("read-only open of %s: %v", s.Path(), err)
	}
	if n == 0 {
		t.Fatal("read-only handle sees an empty database")
	}
	if _, err := ro.Exec("CREATE TABLE x(a)"); err == nil {
		t.Fatal("mode=ro handle accepted a write")
	}
}

func TestFileDSN(t *testing.T) {
	cases := []struct{ path, query, want string }{
		{"/tmp/a/papio.db", "", "file:/tmp/a/papio.db"},
		{"/tmp/a#b/p?q%c.db", "mode=ro", "file:/tmp/a%23b/p%3Fq%25c.db?mode=ro"},
		{"rel/papio.db", "x=1", "file:rel/papio.db?x=1"},
	}
	for _, c := range cases {
		if got := FileDSN(c.path, c.query); got != c.want {
			t.Errorf("FileDSN(%q, %q) = %q, want %q", c.path, c.query, got, c.want)
		}
	}
}
