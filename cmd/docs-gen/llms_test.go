// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLLMSTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const llmsTestNav = `site_name: t
nav:
  - Home: index.md
  - Guide:
      - Start: guide/start.md
      - Other: guide/other.md
  - Reference: ref.md
`

func TestGenerateLLMSOutput(t *testing.T) {
	root := writeLLMSTree(t, map[string]string{
		"docs/index.md":       "---\ntemplate: home.html\n---\n<!-- hero -->\nLanding **text** here.\n",
		"docs/guide/start.md": "# Getting started\n\n<!-- hidden -->\nRun `papio init`\nthen go.\n\n## More\n",
		"docs/guide/other.md": "No heading here.\n",
		"docs/ref.md":         "# Ref\n",
		"docs/unlisted.md":    "# Unlisted\n",
		"mkdocs.yml":          llmsTestNav,
	})
	out := filepath.Join(root, "out", "nested")
	if err := generateLLMS(filepath.Join(root, "docs"), filepath.Join(root, "mkdocs.yml"), out); err != nil {
		t.Fatalf("generateLLMS: %v", err)
	}
	indexBytes, err := os.ReadFile(filepath.Join(out, "llms.txt"))
	if err != nil {
		t.Fatal(err)
	}
	index := string(indexBytes)
	wantIndexHead := "# papio\n\n" +
		"> A local paper-acquisition broker — searches scholarly works, creates acquisition jobs, validates candidate PDFs, and hands ready artifacts to Zotero through zotio.\n\n" +
		"For AI agents: papio is a local paper-acquisition broker. Direct path:\n" +
		"1. Install: `brew install orgmentem/tap/papio`, a release archive, or build `./cmd/papio` (see [Getting started](" + llmsSiteURL + "guide/getting-started/)); install Poppler and Tesseract for PDF validation.\n" +
		"2. Initialize the local profile with `papio init`.\n" +
		"3. Drive the CLI directly with `papio <command> --json`; discover commands with `papio --help` and `papio <command> --help`. The [agent skill](https://github.com/OrgMentem/papio/blob/main/SKILL.md) packages this path; see [Use in a coding agent](" + llmsSiteURL + "guide/agent-skill/). For an MCP-only host, run `papio mcp` instead; its tools are `papio_*` (see [MCP tools](" + llmsSiteURL + "reference/mcp-tools/)).\n" +
		"4. Check integration readiness with `papio doctor` after setup or configuration changes.\n\n" +
		"The complete documentation as one Markdown file: " + llmsSiteURL + "llms-full.txt\n\n"
	wantTail := "\n## Overview\n\n" +
		"- [papio](" + llmsSiteURL + "): Landing text here.\n" +
		"\n## Guide\n\n" +
		"- [Getting started](" + llmsSiteURL + "guide/start/): Run papio init then go.\n" +
		"- [guide/other](" + llmsSiteURL + "guide/other/): No heading here.\n" +
		"\n## Reference\n\n" +
		"- [Ref](" + llmsSiteURL + "ref/)\n"
	if index != wantIndexHead+wantTail {
		t.Fatalf("llms.txt mismatch:\n%s", index)
	}
	if strings.Contains(index, "Unlisted") {
		t.Fatal("llms.txt lists a page that is not in nav")
	}

	fullBytes, err := os.ReadFile(filepath.Join(out, "llms-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	full := string(fullBytes)
	for _, banned := range []string{"template: home.html", "<!--", "hidden", "Unlisted"} {
		if strings.Contains(full, banned) {
			t.Fatalf("llms-full.txt contains %q", banned)
		}
	}
	wantFullHead := "# papio — full documentation\n\n" +
		"A local paper-acquisition broker. This file concatenates the complete docs as Markdown for AI agents. Source: " + llmsSiteURL + "\n" +
		"For the token-efficient path, start with the getting-started guide, then `papio init`, `papio <command> --json`, and `papio doctor` rather than consuming this whole file.\n"
	wantFullTail := "\n\n---\n\n> Source: " + llmsSiteURL + "\n\nLanding **text** here.\n" +
		"\n\n---\n\n> Source: " + llmsSiteURL + "guide/start/\n\n# Getting started\n\nRun `papio init`\nthen go.\n\n## More\n" +
		"\n\n---\n\n> Source: " + llmsSiteURL + "guide/other/\n\nNo heading here.\n" +
		"\n\n---\n\n> Source: " + llmsSiteURL + "ref/\n\n# Ref\n"
	if full != wantFullHead+wantFullTail {
		t.Fatalf("llms-full.txt mismatch:\n%s", full)
	}
}

func TestGenerateLLMSRefusesMissingNavPage(t *testing.T) {
	root := writeLLMSTree(t, map[string]string{
		"docs/index.md": "# Home\n",
		"mkdocs.yml":    "nav:\n  - Home: index.md\n  - Gone: gone.md\n",
	})
	out := filepath.Join(root, "out")
	err := generateLLMS(filepath.Join(root, "docs"), filepath.Join(root, "mkdocs.yml"), out)
	if err == nil || !strings.Contains(err.Error(), "gone.md") {
		t.Fatalf("err = %v, want the missing nav page named", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "llms.txt")); statErr == nil {
		t.Fatal("llms.txt written despite the refusal")
	}
}

// Each output write must surface its own failure: a directory sitting at the
// output path makes that write fail after the output directory already exists.
func TestGenerateLLMSReportsOutputWriteFailure(t *testing.T) {
	for _, name := range []string{"llms.txt", "llms-full.txt"} {
		t.Run(name, func(t *testing.T) {
			root := writeLLMSTree(t, map[string]string{
				"docs/index.md": "# Home\n",
				"mkdocs.yml":    "nav:\n  - Home: index.md\n",
			})
			out := filepath.Join(root, "out")
			if err := os.MkdirAll(filepath.Join(out, name), 0o755); err != nil {
				t.Fatal(err)
			}
			err := generateLLMS(filepath.Join(root, "docs"), filepath.Join(root, "mkdocs.yml"), out)
			if err == nil {
				t.Fatalf("generateLLMS ignored an unwritable %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v, want %s named", err, name)
			}
		})
	}
}

func TestReadLLMSNavRejectsInvalidShapes(t *testing.T) {
	root := writeLLMSTree(t, map[string]string{
		"bad.yml": "nav:\n  - Sec:\n      - T:\n          - nested.md\n",
	})
	if _, err := readLLMSNav(filepath.Join(root, "bad.yml")); err == nil {
		t.Fatal("nested non-scalar nav page accepted")
	}
	if _, err := readLLMSNav(filepath.Join(root, "missing.yml")); err == nil {
		t.Fatal("missing nav file accepted")
	}
}

func TestLLMSTitleAndDesc(t *testing.T) {
	if got := llmsTitle("intro\n# Real\n", "x"); got != "Real" {
		t.Fatalf("title = %q", got)
	}
	if got := llmsTitle("no heading", "index"); got != "papio" {
		t.Fatalf("index fallback = %q", got)
	}
	if got := llmsTitle("no heading", "guide/x"); got != "guide/x" {
		t.Fatalf("rel fallback = %q", got)
	}
	if got := llmsDesc("# H\n\n![img](x)\n<div>\n\n**Bold** `code`\nline two\n\nsecond para"); got != "Bold code line two" {
		t.Fatalf("desc = %q", got)
	}
	if got := llmsDesc("# Only heading"); got != "" {
		t.Fatalf("desc = %q, want empty", got)
	}
}
