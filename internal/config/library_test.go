// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validLibraryConfig = `access_mode = "conservative"
email = "researcher@example.test"

[[library.sources]]
name = "owned-pdfs"
kind = "file"
path = "~/library/with-pdfs.bib"
format = "bibtex"
claim = "pdf_present"

[[library.sources]]
name = "reading-list"
kind = "file"
path = "/tmp/reading.ris"
format = "ris"
claim = "record_present"
`

func TestLoadNormalizesLibrarySourcePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	absolute := filepath.Join(t.TempDir(), "reading.ris")
	body := strings.Replace(validLibraryConfig, "/tmp/reading.ris", absolute, 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Library.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Library.Sources))
	}
	if cfg.Library.Sources[0].Claim != LibraryClaimPDFPresent {
		t.Fatalf("claim = %q", cfg.Library.Sources[0].Claim)
	}
	if got, want := cfg.Library.Sources[0].Path, filepath.Join(home, "library", "with-pdfs.bib"); got != want {
		t.Fatalf("tilde path = %q, want %q", got, want)
	}
	if got := cfg.Library.Sources[1].Path; got != absolute || !filepath.IsAbs(got) {
		t.Fatalf("absolute path = %q, want %q", got, absolute)
	}
}

func TestLibrarySourceValidationIsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "name is required",
			body: "[[library.sources]]\nkind = \"file\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\n",
			want: "name is required",
		},
		{
			name: "duplicate names are ambiguous in reporting",
			body: validLibraryConfig + "\n[[library.sources]]\nname = \"owned-pdfs\"\nkind = \"file\"\npath = \"b.bib\"\nclaim = \"pdf_present\"\n",
			want: "twice",
		},
		{
			name: "surrounding source-name whitespace is rejected",
			body: "[[library.sources]]\nname = \" owned-pdfs \"\nkind = \"file\"\npath = \"/tmp/a.bib\"\nclaim = \"pdf_present\"\n",
			want: "surrounding whitespace",
		},
		{
			name: "kind is required",
			body: "[[library.sources]]\nname = \"a\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\n",
			want: "kind is required",
		},
		{
			name: "unsupported kind is rejected rather than ignored",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"command\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\n",
			want: "not supported",
		},
		{
			name: "path is required for a file source",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"file\"\nclaim = \"pdf_present\"\n",
			want: "path is required",
		},
		{
			name: "relative path is rejected",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"file\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\n",
			want: "must be absolute",
		},
		{
			name: "unknown format is rejected",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"file\"\npath = \"/tmp/a.bib\"\nformat = \"endnote\"\nclaim = \"pdf_present\"\n",
			want: "format",
		},
		{
			// No default: guessing would let papio skip acquisitions a source
			// never vouched for.
			name: "claim has no default",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"file\"\npath = \"/tmp/a.bib\"\n",
			want: "claim must be",
		},
		{
			name: "unknown claim is rejected",
			body: "[[library.sources]]\nname = \"a\"\nkind = \"file\"\npath = \"/tmp/a.bib\"\nclaim = \"probably\"\n",
			want: "claim must be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected validation to reject this configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLibrarySourceCountIsBounded(t *testing.T) {
	var body strings.Builder
	for i := 0; i <= MaxLibrarySources; i++ {
		body.WriteString("[[library.sources]]\nname = \"s")
		body.WriteString(string(rune('a' + i)))
		body.WriteString("\"\nkind = \"file\"\npath = \"/tmp/a.bib\"\nclaim = \"pdf_present\"\n\n")
	}
	_, err := Load(writeConfig(t, body.String()))
	if err == nil {
		t.Fatal("more sources than the cap must be rejected: every one is consulted per lookup")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("error = %v, want it to name the maximum", err)
	}
}

// Config is strict-mode, so a field name we might later want must be rejected
// today rather than silently ignored.
func TestUnknownLibraryFieldIsRejected(t *testing.T) {
	body := "[[library.sources]]\nname = \"a\"\nkind = \"file\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\nrefresh_seconds = 60\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("an unknown library source field must be rejected")
	}
}

func TestLibraryFingerprintIsStableAndSemantic(t *testing.T) {
	base := Config{Library: Library{Sources: []LibrarySource{
		{
			Name:   "owned-pdfs",
			Kind:   LibraryKindFile,
			Path:   "/tmp/library/owned.bib",
			Format: "bibtex",
			Claim:  LibraryClaimPDFPresent,
		},
		{
			Name:   "reading-list",
			Kind:   LibraryKindFile,
			Path:   "/tmp/library/reading.ris",
			Format: "ris",
			Claim:  LibraryClaimRecordPresent,
		},
	}}}

	want := base.LibraryFingerprint()
	if want == "" {
		t.Fatal("fingerprint for configured generic sources is empty")
	}
	if got := base.LibraryFingerprint(); got != want {
		t.Fatalf("fingerprint is not stable: got %q, want %q", got, want)
	}
	reordered := base
	reordered.Library.Sources = []LibrarySource{base.Library.Sources[1], base.Library.Sources[0]}
	if got := reordered.LibraryFingerprint(); got != want {
		t.Fatalf("fingerprint varies with declaration order: got %q, want %q", got, want)
	}

	changes := []struct {
		name   string
		change func(*LibrarySource)
	}{
		{"name", func(source *LibrarySource) { source.Name = "other-library" }},
		{"kind", func(source *LibrarySource) { source.Kind = "command" }},
		{"path", func(source *LibrarySource) { source.Path = "/tmp/library/other.bib" }},
		{"format", func(source *LibrarySource) { source.Format = "ris" }},
		{"claim", func(source *LibrarySource) { source.Claim = LibraryClaimRecordPresent }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			changed.Library.Sources = append([]LibrarySource(nil), base.Library.Sources...)
			tc.change(&changed.Library.Sources[0])
			if got := changed.LibraryFingerprint(); got == want {
				t.Fatalf("fingerprint did not change after changing source %s", tc.name)
			}
		})
	}

	if got := (Config{}).LibraryFingerprint(); got != "" {
		t.Fatalf("fingerprint without generic sources = %q, want empty", got)
	}
}
