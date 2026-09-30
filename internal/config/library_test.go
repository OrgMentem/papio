// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
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
	home := isolatedHome(t)
	absolute := filepath.Join(t.TempDir(), "reading.ris")
	body := strings.Replace(validLibraryConfig, `path = "/tmp/reading.ris"`, libraryPathTOML(t, absolute), 1)

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
	absolute := filepath.Join(t.TempDir(), "a.bib")
	reading := filepath.Join(t.TempDir(), "reading.ris")
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
			body: "[[library.sources]]\nname = \"a\"\nkind = \"folder\"\npath = \"a.bib\"\nclaim = \"pdf_present\"\n",
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
			body := strings.ReplaceAll(tc.body, `path = "/tmp/a.bib"`, libraryPathTOML(t, absolute))
			body = strings.ReplaceAll(body, `path = "/tmp/reading.ris"`, libraryPathTOML(t, reading))
			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatal("expected validation to reject this configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// libraryPathTOML preserves native backslashes as data, not TOML escapes.
func libraryPathTOML(t *testing.T, path string) string {
	t.Helper()
	data, err := toml.Marshal(struct {
		Path string `toml:"path"`
	}{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
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
// today rather than silently ignored. `command` is also the shell-string
// spelling a user might reach for instead of an argv array.
func TestUnknownLibraryFieldIsRejected(t *testing.T) {
	path := libraryPathTOML(t, filepath.Join(t.TempDir(), "a.bib"))
	body := "[[library.sources]]\nname = \"a\"\nkind = \"file\"\n" + path + "\nclaim = \"pdf_present\"\ncommand = \"papis export\"\n"
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("an unknown library source field must be rejected")
	}
}

// argvTOML preserves native path separators as data, not TOML escapes.
func argvTOML(t *testing.T, argv ...string) string {
	t.Helper()
	data, err := toml.Marshal(struct {
		Argv []string `toml:"argv"`
	}{Argv: argv})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestLibraryCommandSourceLoads(t *testing.T) {
	program := filepath.Join(t.TempDir(), "export-holdings")
	body := "[[library.sources]]\nname = \"live\"\nkind = \"command\"\n" + argvTOML(t, program, "--with-pdf") +
		"\nformat = \"bibtex\"\nclaim = \"pdf_present\"\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	source := cfg.Library.Sources[0]
	if len(source.Argv) != 2 || source.Argv[0] != program || source.Argv[1] != "--with-pdf" {
		t.Fatalf("argv = %q", source.Argv)
	}
	limits, err := source.CommandLimits()
	if err != nil {
		t.Fatal(err)
	}
	want := LibraryCommandLimits{
		Timeout:        DefaultLibraryCommandTimeoutSeconds * time.Second,
		MaxOutputBytes: DefaultLibraryCommandMaxOutputBytes,
		Refresh:        DefaultLibraryCommandRefreshSeconds * time.Second,
	}
	if limits != want {
		t.Fatalf("limits = %+v, want defaults %+v", limits, want)
	}

	bounded := body + "timeout_seconds = 300\nmax_output_bytes = 1\nrefresh_seconds = 10\n"
	cfg, err = Load(writeConfig(t, bounded))
	if err != nil {
		t.Fatalf("bounds at the edge of their ranges: %v", err)
	}
	limits, err = cfg.Library.Sources[0].CommandLimits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Timeout != 300*time.Second || limits.MaxOutputBytes != 1 || limits.Refresh != 10*time.Second {
		t.Fatalf("limits = %+v", limits)
	}
}

// Validation must not depend on the loading process's PATH. The daemon's PATH
// is not the PATH of the shell that ran doctor, so a bare name that one
// process finds would fail to load, or name another program, in the other.
func TestLibraryCommandBareProgramIsRejectedEvenWhenOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX program fixture")
	}
	bin := t.TempDir()
	program := filepath.Join(bin, "export-holdings")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	body := "[[library.sources]]\nname = \"live\"\nkind = \"command\"\nargv = [\"export-holdings\", \"--all\"]\nclaim = \"record_present\"\n"
	_, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("error = %v, want a bare program name rejected although it is on PATH", err)
	}
}

// Load keeps argv exactly as written, so the library fingerprint hashes the
// config's own text rather than something resolved from the loading process.
func TestLibraryCommandArgvIsKeptAsWritten(t *testing.T) {
	body := "[[library.sources]]\nname = \"live\"\nkind = \"command\"\nargv = [\"~/bin/export-holdings\", \"--all\"]\nclaim = \"record_present\"\n"
	t.Setenv("HOME", t.TempDir())
	first, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Library.Sources[0].Argv; len(got) != 2 || got[0] != "~/bin/export-holdings" || got[1] != "--all" {
		t.Fatalf("argv = %q, want it as written", got)
	}
	t.Setenv("HOME", t.TempDir())
	second, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if first.LibraryFingerprint() != second.LibraryFingerprint() {
		t.Fatal("the library fingerprint changed with the loading process's HOME")
	}
}

func TestLibraryCommandSourceValidationIsFailClosed(t *testing.T) {
	program := filepath.Join(t.TempDir(), "export-holdings")
	command := "[[library.sources]]\nname = \"live\"\nkind = \"command\"\nclaim = \"pdf_present\"\n"
	withArgv := command + argvTOML(t, program) + "\n"
	file := "[[library.sources]]\nname = \"a\"\nkind = \"file\"\n" +
		libraryPathTOML(t, filepath.Join(t.TempDir(), "a.bib")) + "\nclaim = \"pdf_present\"\n"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"argv is required", command, "argv is required"},
		{"empty argv", command + "argv = []\n", "argv is required"},
		{"blank program", command + "argv = [\"\"]\n", "argv[0] must name a program"},
		{"padded program", command + "argv = [\" sh\"]\n", "argv[0] must name a program"},
		{"relative program path", command + "argv = [\"./export-holdings\"]\n", "must be an absolute path"},
		{"bare program name", command + "argv = [\"papio-test-no-such-program\"]\n", "must be an absolute path"},
		{"NUL in an argument", command + argvTOML(t, program, "a\x00b") + "\n", "NUL"},
		{"shell string instead of argv", command + "argv = \"papis export --all\"\n", "parsing config"},
		{"path beside argv", withArgv + libraryPathTOML(t, filepath.Join(t.TempDir(), "a.bib")) + "\n", "path must be empty"},
		{"timeout below range", withArgv + "timeout_seconds = -1\n", "timeout_seconds must be between"},
		{"timeout above range", withArgv + "timeout_seconds = 301\n", "timeout_seconds must be between"},
		{"output cap above range", withArgv + "max_output_bytes = 134217729\n", "max_output_bytes must be between"},
		{"output cap negative", withArgv + "max_output_bytes = -5\n", "max_output_bytes must be between"},
		{"refresh below range", withArgv + "refresh_seconds = 9\n", "refresh_seconds must be between"},
		{"refresh above range", withArgv + "refresh_seconds = 86401\n", "refresh_seconds must be between"},
		{"file source with argv", file + argvTOML(t, program) + "\n", "apply only to kind \"command\""},
		{"file source with a timeout", file + "timeout_seconds = 5\n", "apply only to kind \"command\""},
		{"file source with a refresh interval", file + "refresh_seconds = 60\n", "apply only to kind \"command\""},
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

func TestLibraryFingerprintCoversCommandArgv(t *testing.T) {
	source := LibrarySource{
		Name:  "live",
		Kind:  LibraryKindCommand,
		Argv:  []string{"/usr/local/bin/export-holdings", "--with-pdf"},
		Claim: LibraryClaimPDFPresent,
	}
	base := Config{Library: Library{Sources: []LibrarySource{source}}}
	changed := source
	changed.Argv = []string{"/usr/local/bin/export-holdings", "--all"}
	other := Config{Library: Library{Sources: []LibrarySource{changed}}}
	if base.LibraryFingerprint() == other.LibraryFingerprint() {
		t.Fatal("fingerprint did not change when the command's argv changed")
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
