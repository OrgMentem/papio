// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A config written before open_books existed has no [open_books] table, and
// must load with the shipped repositories rather than with none: the source
// ships enabled, so an empty list would silently turn it into a no-op.
func TestOpenBooksDefaultsApplyToAnOlderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := OpenBooks{DSpaceServers: []string{DefaultDSpaceServer}}
	if !reflect.DeepEqual(cfg.OpenBooks, want) {
		t.Fatalf("open_books = %+v, want %+v", cfg.OpenBooks, want)
	}
	if !cfg.SourcePolicy(SourceOpenBooks).Enabled {
		t.Fatal("open_books must ship enabled")
	}
}

func TestOpenBooksListIsReplacedNotMergedAndMayBeEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "access_mode='conservative'\n[open_books]\ndspace_servers = ['https://repo.example.org/server']\noapen = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := OpenBooks{DSpaceServers: []string{"https://repo.example.org/server"}, OAPEN: false}
	if !reflect.DeepEqual(cfg.OpenBooks, want) {
		t.Fatalf("open_books = %+v, want %+v", cfg.OpenBooks, want)
	}

	// An operator who wants OAPEN only writes an empty list; it must survive a
	// load, which an omitempty tag would turn back into the default.
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[open_books]\ndspace_servers = []\noapen = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenBooks.DSpaceServers) != 0 || !cfg.OpenBooks.OAPEN {
		t.Fatalf("open_books = %+v, want an empty server list and OAPEN on", cfg.OpenBooks)
	}
}

func TestOpenBooksValidation(t *testing.T) {
	many := make([]string, MaxDSpaceServers+1)
	for i := range many {
		many[i] = "https://r" + string(rune('a'+i)) + ".example.org/server"
	}
	tests := []struct {
		name    string
		servers []string
		wantErr string
	}{
		{"default", []string{DefaultDSpaceServer}, ""},
		{"loopback http for tests", []string{"http://127.0.0.1:8080/server"}, ""},
		{"plain http to a public host", []string{"http://repo.example.org/server"}, "must use https"},
		{"credentials in the URL", []string{"https://user:pass@repo.example.org/server"}, "not a plain URL"},
		{"query string", []string{"https://repo.example.org/server?x=1"}, "not a plain URL"},
		{"fragment", []string{"https://repo.example.org/server#x"}, "not a plain URL"},
		{"no host", []string{"https:///server"}, "not a plain URL"},
		{"not a URL", []string{"repo"}, "not a plain URL"},
		{"duplicate after a trailing slash", []string{"https://repo.example.org/server", "https://REPO.example.org/server/"}, "twice"},
		{"too many", many, "maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.AccessMode = ModeConservative
			cfg.OpenBooks.DSpaceServers = tt.servers
			err := cfg.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// Default() hands out a private server list: validate() and clone() must not
// let a caller's edit reach the next Default().
func TestOpenBooksDefaultsAreNotShared(t *testing.T) {
	first := Default()
	first.OpenBooks.DSpaceServers[0] = "https://elsewhere.example.org/server"
	if got := Default().OpenBooks.DSpaceServers[0]; got != DefaultDSpaceServer {
		t.Fatalf("mutating one Default() changed the next: %q", got)
	}
	cloned := Default().clone()
	cloned.OpenBooks.DSpaceServers[0] = "https://elsewhere.example.org/server"
	original := Default()
	if got := original.clone().OpenBooks.DSpaceServers[0]; got != DefaultDSpaceServer {
		t.Fatalf("clone shares its slice: %q", got)
	}
}
