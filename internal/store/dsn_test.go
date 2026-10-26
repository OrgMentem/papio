// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func seedSpecialDir(t *testing.T, dir string) {
	t.Helper()
	seedOnce.Do(buildSeed)
	if seedErr != nil {
		t.Fatalf("building migrated template: %v", seedErr)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "papio.db"), seedTemplate, 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dir, err)
	}
}

func osFileInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// URI-special characters in the data directory must neither redirect the
// database to a different file nor drop the pragmas.
func TestOpenDataDirWithURISpecialCharacters(t *testing.T) {
	for _, name := range []string{"Research#2026", "what?now", "100%25done", "a%20b#c?d&e=f"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent, err := os.MkdirTemp(t.TempDir(), "p")
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(parent, name, "papio")
			seedSpecialDir(t, dir)
			s, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			if _, err := os.Stat(filepath.Join(dir, "papio.db")); err != nil {
				t.Fatalf("database not created at the requested path: %v", err)
			}
			// A truncated path can also stay inside the data dir itself: a
			// '#' drops the tail of the filename, so "Research#2026" opens
			// "Research" next to the seeded directory. That fallback file is
			// empty, so migrations still run and the schema probes pass —
			// only the stray-file check and the identity check below fail.
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != name || !entries[0].IsDir() {
				t.Fatalf("stray entries beside the data dir: %v", entries)
			}
			// PRAGMA database_list reports the file the handle actually
			// opened: a mis-escaped DSN that lands on a stray or truncated
			// fallback fails here even when migrations built it a full
			// schema, which a table probe cannot tell apart. Compare by
			// identity, not text: the handle reports the canonical path
			// (/private/var/... on macOS) while s.Path keeps the symlink
			// form (/var/...).
			var seq int
			var mainName, opened string
			if err := s.DB().QueryRow("PRAGMA database_list").Scan(&seq, &mainName, &opened); err != nil {
				t.Fatalf("PRAGMA database_list: %v", err)
			}
			if !os.SameFile(osFileInfo(t, opened), osFileInfo(t, s.Path())) {
				t.Fatalf("opened %q, want the seeded database %q", opened, s.Path())
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
	seedSpecialDir(t, dir)
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
	var roSeeded string
	if err := ro.QueryRow("SELECT name FROM sqlite_master WHERE name='jobs'").Scan(&roSeeded); err != nil {
		t.Fatalf("read-only handle does not see the seeded schema: %v", err)
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
