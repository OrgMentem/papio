package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The repo guards code against code well: TestTerminalReasonVocabularyIsExhaustive
// parses job.go so a new reason cannot skip its table, adapters cannot register
// without a fixture, and FieldSpec[T] makes the TS protocol interfaces
// compiler-enforced. None of that guarded code against the DOCS describing it,
// and every documentation defect found in the 2026-08-07 audit lived in that gap:
// permissions the extension had gained, a terminal job state, twelve missing
// ADRs, twenty-three missing adapters, two missing CLI flags.
//
// These tests close it with one shape: everything the code declares must be
// MENTIONED in the page that enumerates it. They assert mention, not phrasing,
// so prose stays free while additions cannot land undocumented.
//
// Each test asserts its own parse found something first. A guard that silently
// matches nothing is worse than no guard — that is exactly how the terminal
// reason table came to cover less than it claimed.

func mustRead(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// TestExtensionPermissionsAreDocumented is the highest-value guard here.
// manifest.json changes whenever a feature or adapter lands, the pages that
// enumerate permissions are the project's privacy claims, and being wrong there
// is a false privacy statement on a public site. It has already drifted:
// alarms, tabGroups and login.openathens.net were all added after the prose was
// written, leaving two pages asserting the extension held no identity-provider
// host permission while it required one.
func TestExtensionPermissionsAreDocumented(t *testing.T) {
	var manifest struct {
		Permissions             []string `json:"permissions"`
		HostPermissions         []string `json:"host_permissions"`
		OptionalHostPermissions []string `json:"optional_host_permissions"`
	}
	if err := json.Unmarshal([]byte(mustRead(t, "extension/manifest.json")), &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(manifest.Permissions) == 0 || len(manifest.HostPermissions) == 0 {
		t.Fatal("parsed no permissions from extension/manifest.json — the guard would pass vacuously")
	}

	const pageA = "docs/concepts/access-modes.md"
	const pageB = "docs/concepts/browser-handoff.md"
	prose := mustRead(t, pageA) + "\n" + mustRead(t, pageB)

	for _, perm := range manifest.Permissions {
		// Whole-word: "tabs" must not be satisfied by "tabGroups".
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(perm) + `\b`).MatchString(prose) {
			t.Errorf("manifest permission %q is documented in neither %s nor %s. "+
				"Those pages enumerate what the extension can do; an undocumented "+
				"permission makes their privacy claims false.", perm, pageA, pageB)
		}
	}
	for _, host := range manifest.HostPermissions {
		if !strings.Contains(prose, host) {
			t.Errorf("REQUIRED host permission %q is documented in neither %s nor %s. "+
				"Required host access is granted at install and must never be described "+
				"as opt-in.", host, pageA, pageB)
		}
	}
	// The broad optional grant is the one a reader is most likely to be misled
	// about, because the surrounding prose frames optional access as per-provider.
	for _, host := range manifest.OptionalHostPermissions {
		if strings.HasPrefix(host, "https://*/") && !strings.Contains(prose, host) {
			t.Errorf("optional_host_permissions contains the catch-all %q, which is not "+
				"mentioned in %s or %s. A grant covering every site cannot be left implied "+
				"by prose about per-provider access.", host, pageA, pageB)
		}
	}
}

// TestRegisteredAdaptersAreDocumented keeps the provider matrix honest. It had
// fallen to 4 of 27 registered adapters.
func TestRegisteredAdaptersAreDocumented(t *testing.T) {
	src := mustRead(t, "extension/src/adapters/types.ts")
	ids := regexp.MustCompile(`(?m)^\s*id:\s*"([a-z0-9-]+)"`).FindAllStringSubmatch(src, -1)
	if len(ids) == 0 {
		t.Fatal("parsed no adapter ids from extension/src/adapters/types.ts")
	}

	const page = "docs/concepts/provider-compatibility.md"
	matrix := mustRead(t, page)
	for _, m := range ids {
		if !strings.Contains(matrix, m[1]) {
			t.Errorf("adapter %q is registered but absent from %s. Every registered "+
				"adapter belongs in the compatibility matrix, including one with no "+
				"download route (say so in its row rather than omitting it).", m[1], page)
		}
	}
}

// TestEveryADRIsSummarized pins the public curated summary to dev/adr/, which is
// outside docs_dir and therefore never published. The summary is the only public
// surface for those decisions, and it had stalled around ADR-0010 while the log
// reached 0019.
func TestEveryADRIsSummarized(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(repoRoot, "dev", "adr", "[0-9][0-9][0-9][0-9]-*.md"))
	if err != nil {
		t.Fatalf("glob dev/adr: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("found no ADRs under dev/adr/")
	}

	const page = "docs/contributing/architecture-decisions.md"
	summary := mustRead(t, page)
	for _, entry := range entries {
		number := filepath.Base(entry)[:4]
		if !strings.Contains(summary, "ADR-"+number) {
			t.Errorf("ADR-%s exists in dev/adr/ but is not summarized in %s. dev/adr/ is "+
				"unpublished, so that page is the only public record of the decision.",
				number, page)
		}
	}

	// dev/ is outside docs_dir: a link into it 404s on the live site.
	if strings.Contains(summary, "](dev/") || strings.Contains(summary, "](../dev/") ||
		strings.Contains(summary, "](../../dev/") {
		t.Errorf("%s links into dev/, which is not published and will 404. Refer to "+
			"ADRs by number and title instead.", page)
	}
}

// TestJobStatesAreDocumented pins the lifecycle page to job.go. The `imported`
// terminal state existed in code and in no diagram.
func TestJobStatesAreDocumented(t *testing.T) {
	src := mustRead(t, "internal/job/job.go")
	states := regexp.MustCompile(`State\w+\s+(?:State\s*)?=\s*"([a-z_]+)"`).FindAllStringSubmatch(src, -1)
	if len(states) == 0 {
		t.Fatal("parsed no job states from internal/job/job.go")
	}

	const page = "docs/concepts/acquisition-pipeline.md"
	prose := mustRead(t, page)
	for _, m := range states {
		if !strings.Contains(prose, m[1]) {
			t.Errorf("job state %q is defined in internal/job/job.go but never appears in %s. "+
				"The lifecycle page is meant to enumerate the states.", m[1], page)
		}
	}
}

// TestConfigFieldsAreDocumented pins the hand-authored config reference to the
// struct tags. Config is strict-mode, so an undocumented field is one a user
// cannot discover but which will reject their file if misspelled.
func TestConfigFieldsAreDocumented(t *testing.T) {
	src := mustRead(t, "internal/config/config.go")
	tags := regexp.MustCompile(`toml:"([^"]+)"`).FindAllStringSubmatch(src, -1)
	if len(tags) == 0 {
		t.Fatal("parsed no toml tags from internal/config/config.go")
	}

	const page = "docs/reference/config-reference.md"
	reference := mustRead(t, page)
	seen := make(map[string]bool, len(tags))
	for _, m := range tags {
		name := strings.Split(m[1], ",")[0]
		if name == "" || name == "-" || seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(reference, name) {
			t.Errorf("config field %q is declared in internal/config/config.go but absent "+
				"from %s, which is hand-authored and has no generator. Configuration is "+
				"strict-mode, so an undocumented field is undiscoverable.", name, page)
		}
	}
}

// TestInitFlagsAreDocumented pins the getting-started setup table to the
// generated command reference. commands.md is regenerated from the binary and
// gated in CI, so it is a trustworthy proxy for the real flag set.
func TestInitFlagsAreDocumented(t *testing.T) {
	commands := mustRead(t, "docs/reference/commands.md")
	section := regexp.MustCompile(`(?s)\n## ` + "`papio init`" + `\n(.*?)(?:\n## )`).FindStringSubmatch(commands)
	if section == nil {
		t.Fatal("could not locate the `papio init` section in docs/reference/commands.md")
	}
	flags := regexp.MustCompile("`(--[a-z0-9-]+)`").FindAllStringSubmatch(section[1], -1)
	if len(flags) == 0 {
		t.Fatal("parsed no flags from the `papio init` section")
	}

	const page = "docs/guide/getting-started.md"
	guide := mustRead(t, page)
	for _, m := range flags {
		if !strings.Contains(guide, m[1]) {
			t.Errorf("`papio init` accepts %s but %s never mentions it. That page's table "+
				"presents itself as the scripted-setup flag set.", m[1], page)
		}
	}
}

// TestInternalDocLinksResolve exists because `zensical build` reports a broken
// link as an "issue" and still exits 0, so the docs workflow stays green with
// broken links. Verified against zensical 0.0.51.
func TestInternalDocLinksResolve(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	pages, err := filepath.Glob(filepath.Join(docsDir, "*", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	roots, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil {
		t.Fatalf("glob docs root: %v", err)
	}
	pages = append(pages, roots...)
	if len(pages) == 0 {
		t.Fatal("found no docs pages")
	}

	link := regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	checked := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		for _, m := range link.FindAllStringSubmatch(string(body), -1) {
			target := m[1]
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target = target[:i]
			}
			target = strings.TrimSpace(target)
			if target == "" || strings.HasPrefix(target, "http://") ||
				strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			checked++
			if _, err := os.Stat(filepath.Join(filepath.Dir(page), target)); err != nil {
				rel, _ := filepath.Rel(repoRoot, page)
				t.Errorf("%s links to %q, which does not exist", rel, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("checked no internal links — the guard would pass vacuously")
	}
}
