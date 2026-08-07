package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// repoRoot is where mkdocs.yml and docs/ live, relative to this package.
const repoRoot = "../.."

// TestEveryDocsPageIsNavigated pins docs/ to mkdocs.yml's nav in both
// directions.
//
// A page that exists under docs/ but is absent from nav is NOT unpublished:
// the site generator still builds it to HTML, gives it a canonical URL, and
// indexes it for search — it is merely unlinked. It is, however, invisible to
// llms.txt/llms-full.txt, which readLLMSNav derives from nav alone. So an
// orphan is the worst of both: publicly reachable and searchable, but missing
// from the generated agent index.
//
// This has bitten the repo twice. ADRs were moved docs/adr -> dev/adr (commit
// 16d930d) for exactly this reason, and internal build plans then reappeared
// under docs/plans and shipped to the public site. Internal material belongs in
// dev/ (dev/adr, dev/plans), which is outside docs_dir and therefore never
// built.
//
// The other direction — a nav entry naming a file that does not exist — is a
// broken build rather than a leak, and is cheap to catch in the same pass.
func TestEveryDocsPageIsNavigated(t *testing.T) {
	navPages, err := readLLMSNav(filepath.Join(repoRoot, "mkdocs.yml"))
	if err != nil {
		t.Fatalf("read nav: %v", err)
	}
	inNav := make(map[string]bool, len(navPages))
	for _, p := range navPages {
		inNav[p.rel+".md"] = true
	}

	docsDir := filepath.Join(repoRoot, "docs")
	onDisk := make(map[string]bool)
	err = filepath.WalkDir(docsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(docsDir, path)
		if err != nil {
			return err
		}
		onDisk[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}

	for _, page := range sortedKeys(onDisk) {
		if !inNav[page] {
			t.Errorf("docs/%s is not in mkdocs.yml nav: it will still be built, "+
				"served at a canonical URL, and search-indexed, but omitted from "+
				"llms.txt. Add it to nav, or move it to dev/ if it is internal.", page)
		}
	}
	for _, page := range sortedKeys(inNav) {
		if !onDisk[page] {
			t.Errorf("mkdocs.yml nav references docs/%s, which does not exist", page)
		}
	}
}

// TestNoInternalMaterialUnderDocs keeps the unpublished tiers out of docs_dir.
// dev/adr and dev/plans are deliberately outside the site; a docs/adr or
// docs/plans directory means internal material is being published again.
func TestNoInternalMaterialUnderDocs(t *testing.T) {
	for _, dir := range []string{"adr", "plans", "scratch", "pr"} {
		path := filepath.Join(repoRoot, "docs", dir)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("docs/%s exists: internal material under docs_dir is built and "+
				"published. It belongs in dev/%s.", dir, dir)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat docs/%s: %v", dir, err)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
