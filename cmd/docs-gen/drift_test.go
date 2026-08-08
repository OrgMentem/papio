package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"papio/internal/cli"
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

// TestSkillInvocationsResolve pins SKILL.md — the root agent skill that teaches
// a coding agent to drive the CLI directly — to the live cobra tree. The skill
// is the one page an agent reads INSTEAD of the docs site, so a renamed command
// or dropped flag there is not a stale sentence: it is an agent confidently
// running something that no longer exists. commands.md cannot cover this
// because SKILL.md lives outside docs/ and is never generated.
//
// It checks what it can resolve — every `papio …` invocation, plus every inline
// span opening with a top-level command name — and treats anything else as
// prose. Placeholders (`<job-id>`) and values are arguments, not commands.
// TestSkillFlagMentionsResolve covers the flags this walk cannot see.
func TestSkillInvocationsResolve(t *testing.T) {
	root := cli.NewRoot(io.Discard, io.Discard)
	invocations := skillInvocations(mustRead(t, "SKILL.md"), root)
	// A floor, not a presence check: a parser regression that quietly matched
	// three lines instead of a hundred would otherwise keep passing.
	if len(invocations) < 95 {
		t.Fatalf("parsed only %d command invocations from SKILL.md — the parser, not the skill, "+
			"is what shrank", len(invocations))
	}

	for _, tokens := range invocations {
		cmd := root
		for _, token := range tokens {
			parts := strings.Split(token, "|")
			// `watch add|list|run|remove` names siblings: every alternative has
			// to exist, and the walk continues under the first.
			if len(parts) > 1 && skillAnyChild(cmd, parts) {
				for _, part := range parts {
					if skillChild(cmd, part) == nil {
						t.Errorf("SKILL.md offers %q, but `%s` has no %s subcommand",
							strings.Join(tokens, " "), cmd.CommandPath(), part)
					}
				}
				if first := skillChild(cmd, parts[0]); first != nil {
					cmd = first
				}
				continue
			}
			for _, part := range parts {
				if !strings.HasPrefix(part, "-") {
					if child := skillChild(cmd, part); child != nil {
						cmd = child
					}
					continue
				}
				for _, flag := range strings.Split(part, "/") {
					if name, ok := skillFlagName(flag); ok && !skillHasFlag(cmd, name) {
						t.Errorf("SKILL.md runs %q, but `%s` accepts no %s flag",
							strings.Join(tokens, " "), cmd.CommandPath(), flag)
					}
				}
			}
		}
	}
}

// skillAbsentFlags are flags SKILL.md names in order to say they do NOT exist.
// The assertion is inverted for them: adding one to the CLI has to update that
// sentence. Without this the guard cannot tell a truthful "there is no --agent
// flag" from a typo, and the difference would rest on where the sentence sits.
var skillAbsentFlags = map[string]bool{"agent": true}

// TestSkillFlagMentionsResolve checks the flags an invocation walk structurally
// cannot: a capability bullet names its command once and then discusses the
// flags in bare spans (`--limit`, `--oa-only`, `--desired-version`), and a
// recipe explains in prose what its fenced example ran. Roughly half the flag
// names in SKILL.md are written that way, so without this the skill's promise —
// dropping a flag fails the build — held for command lines only.
//
// Each flag is attributed to the commands its SECTION names — its own block's
// spans, its siblings', and its fenced examples' — because a sentence routinely
// discusses the flags of the command in the example above it rather than the
// one it happens to mention. Section scope is the point: a flag list under
// `acquire` checked against the whole tree passes while `acquire` no longer
// declares it, as long as any other command shares the name.
func TestSkillFlagMentionsResolve(t *testing.T) {
	root := cli.NewRoot(io.Discard, io.Discard)
	known := skillKnownFlags(root)
	checked := 0

	blocks, unterminatedFence := skillBlocks(mustRead(t, "SKILL.md"), root)
	if unterminatedFence {
		t.Fatal("SKILL.md leaves a code fence open — every block after it would go unchecked")
	}
	for _, block := range blocks {
		contexts := block.scope()
		for _, flag := range block.flags {
			name, ok := skillFlagName(flag)
			if !ok {
				continue
			}
			if skillAbsentFlags[name] {
				if known[name] {
					t.Errorf("SKILL.md says %s does not exist, but the CLI now declares it: %q",
						flag, skillHead(block.text))
				}
				continue
			}
			checked++
			if len(contexts) == 0 {
				if !known[name] {
					t.Errorf("SKILL.md mentions %s, which no papio command declares: %q", flag, skillHead(block.text))
				}
				continue
			}
			accepted := false
			paths := make([]string, 0, len(contexts))
			for _, cmd := range contexts {
				paths = append(paths, "`"+cmd.CommandPath()+"`")
				accepted = accepted || skillHasFlag(cmd, name)
			}
			if !accepted {
				named := "its section"
				if block.bullet && len(block.commands) > 0 {
					named = "it"
				}
				t.Errorf("SKILL.md discusses %s in %q, but no command %s names (%s) accepts it",
					flag, skillHead(block.text), named, strings.Join(paths, ", "))
			}
		}
	}
	// Floors sit just under today's counts (110 invocations, 52 mentions): a
	// parser regression that halves coverage has to fail, not shrink quietly.
	if checked < 40 {
		t.Fatalf("checked only %d bare flag mentions — the block parser stopped matching", checked)
	}
}

var (
	// The info string is anything a fence can carry (`bash`, `shell-session`,
	// `json5`): matching only `[a-z]*` let a fence hide its contents from the
	// invocation walk while still toggling the block parser's fence state.
	skillFence     = regexp.MustCompile("(?s)(?:```|~~~)[A-Za-z0-9_+.-]*\n(.*?)(?:```|~~~)")
	skillFenceLine = regexp.MustCompile("^(`{3,}|~{3,})[ \t]*([A-Za-z0-9_+.-]*)[ \t]*$")
	skillSpan      = regexp.MustCompile("`([^`\n]+)`")
	skillQuote     = regexp.MustCompile(`"[^"]*"|'[^']*'`)
)

// skillInvocations returns the tokenized command lines SKILL.md claims are
// runnable: fenced-block lines and inline spans that start with `papio` or with
// one of its top-level command names. Frontmatter is skipped — its trigger
// phrases are natural language that happens to share words with commands.
func skillInvocations(body string, root *cobra.Command) [][]string {
	body = skillBody(body)

	candidates := make([]string, 0, 64)
	for _, block := range skillFence.FindAllStringSubmatch(body, -1) {
		candidates = append(candidates, strings.Split(block[1], "\n")...)
	}
	for _, span := range skillSpan.FindAllStringSubmatch(body, -1) {
		candidates = append(candidates, span[1])
	}

	var invocations [][]string
	for _, candidate := range candidates {
		tokens := skillTokens(candidate)
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] == "papio" {
			tokens = tokens[1:]
		} else if skillChild(root, tokens[0]) == nil {
			continue
		}
		if len(tokens) > 0 {
			invocations = append(invocations, tokens)
		}
	}
	return invocations
}

// skillBody drops the YAML frontmatter, whose trigger phrases are natural
// language that happens to share words with commands ("watch for new papers").
func skillBody(body string) string {
	if !strings.HasPrefix(body, "---\n") {
		return body
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		return body
	}
	return body[4+end+5:]
}

// skillTokens normalizes one candidate command line: shell comment dropped,
// quoted arguments collapsed, and Markdown's optionality brackets and sentence
// punctuation trimmed off each token.
func skillTokens(candidate string) []string {
	if i := strings.IndexByte(candidate, '#'); i >= 0 {
		candidate = candidate[:i]
	}
	fields := strings.Fields(skillQuote.ReplaceAllString(candidate, "x"))
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.Trim(field, "[]().,")
		if field == "" || field == "--" {
			continue
		}
		tokens = append(tokens, field)
	}
	return tokens
}

func skillChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return child
		}
	}
	return nil
}

func skillAnyChild(cmd *cobra.Command, names []string) bool {
	for _, name := range names {
		if skillChild(cmd, name) != nil {
			return true
		}
	}
	return false
}

// skillBlock is one prose unit of SKILL.md: a bullet with its continuation
// lines, or a paragraph. commands are the ones the block itself names, section
// the union named anywhere under the same heading, and flags the bare flag
// spans — a span opening with a command is an invocation, checked already.
type skillBlock struct {
	text     string
	bullet   bool
	commands []*cobra.Command
	section  []*cobra.Command
	flags    []string
}

// scope returns the commands a bare flag in this block must belong to. A bullet
// that names its own commands is a self-contained capability entry, so it is
// held to them; widening it to the section let `--follow` pass under `acquire`
// because `status` shares the heading. A paragraph inherits the section: it
// routinely explains the fenced example above it rather than a command it
// happens to mention.
func (b skillBlock) scope() []*cobra.Command {
	if b.bullet && len(b.commands) > 0 {
		return b.commands
	}
	return b.section
}

// skillBlocks splits SKILL.md into those units. A fence separates blocks rather
// than folding into one — gluing code to a sentence would misattribute both —
// but the commands it runs still count towards its section, because a recipe's
// prose discusses the flags of the example directly above it. Paragraphs are
// included deliberately: Recipes and the canonical loop are headings, fences,
// and prose, with no bullet anywhere to hang a flag mention on.
func skillBlocks(body string, root *cobra.Command) (blocks []skillBlock, unterminatedFence bool) {
	var current strings.Builder
	var fenced []*cobra.Command
	sectionStart := 0
	var bullet bool
	flush := func() {
		if current.Len() == 0 {
			return
		}
		text := current.String()
		current.Reset()
		commands, flags := skillSpanFlags(root, text)
		blocks = append(blocks, skillBlock{text: text, bullet: bullet, commands: commands, flags: flags})
		bullet = false
	}
	// endSection publishes the union of the section's commands to its blocks, so
	// a bullet listing flags under a command named two bullets (or one fenced
	// example) earlier is still checked against that command, not the whole tree.
	endSection := func() {
		flush()
		var union []*cobra.Command
		seen := map[*cobra.Command]bool{}
		add := func(cmds []*cobra.Command) {
			for _, cmd := range cmds {
				if !seen[cmd] {
					seen[cmd] = true
					union = append(union, cmd)
				}
			}
		}
		for _, block := range blocks[sectionStart:] {
			add(block.commands)
		}
		add(fenced)
		for i := range blocks[sectionStart:] {
			blocks[sectionStart+i].section = union
		}
		fenced = nil
		sectionStart = len(blocks)
	}

	// CommonMark fencing, because the shortcuts are what a maintainer trips
	// over: an opener's delimiter and run length decide what closes it, so a
	// nested ``` example inside a ```` block is content, and `~~~` is a fence
	// too. Blind toggling on "```" turned one unclosed `shell-session` fence
	// into silence for every block after it.
	fence := ""
	for _, line := range strings.Split(skillBody(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if marker := skillFenceLine.FindStringSubmatch(trimmed); marker != nil {
			opener, info := marker[1], marker[2]
			if fence == "" {
				fence = opener
				flush()
				continue
			}
			if opener[0] == fence[0] && len(opener) >= len(fence) && info == "" {
				fence = ""
				flush()
				continue
			}
			// Anything else inside a fence is fenced content, not a delimiter.
		}
		switch {
		case fence != "":
			if cmd := skillResolve(root, skillTokens(strings.TrimPrefix(trimmed, "papio "))); cmd != root {
				fenced = append(fenced, cmd)
			}
		case strings.HasPrefix(trimmed, "#"):
			endSection()
		case strings.HasPrefix(trimmed, "- "):
			flush()
			bullet = true
			current.WriteString(trimmed)
		case trimmed == "":
			flush()
		case current.Len() > 0:
			current.WriteString(" " + trimmed)
		default:
			current.WriteString(trimmed)
		}
	}
	endSection()
	// An unclosed fence swallows the rest of the file: every later block would
	// be silently unchecked, and the floors can still be met by what came
	// first. Report it as the parse failure it is.
	return blocks, fence != ""
}

// skillSpanFlags splits one block into the commands it names and the flags it
// mentions on their own. A span that opens with a command is a checked
// invocation already, so only bare flag spans are returned here.
func skillSpanFlags(root *cobra.Command, item string) ([]*cobra.Command, []string) {
	var contexts []*cobra.Command
	var orphans []string
	for _, span := range skillSpan.FindAllStringSubmatch(item, -1) {
		tokens := skillTokens(span[1])
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] == "papio" {
			tokens = tokens[1:]
		}
		if len(tokens) > 0 && !strings.HasPrefix(tokens[0], "-") {
			if cmd := skillResolve(root, tokens); cmd != root {
				contexts = append(contexts, cmd)
			}
			continue
		}
		for _, token := range tokens {
			for _, flag := range strings.FieldsFunc(token, func(r rune) bool { return r == '|' || r == '/' }) {
				if strings.HasPrefix(flag, "-") {
					orphans = append(orphans, flag)
				}
			}
		}
	}
	return contexts, orphans
}

// skillResolve walks as deep into the command tree as the tokens name, ignoring
// arguments and flags.
func skillResolve(root *cobra.Command, tokens []string) *cobra.Command {
	cmd := root
	for _, token := range tokens {
		for _, part := range strings.Split(token, "|") {
			if child := skillChild(cmd, part); child != nil {
				cmd = child
				break
			}
		}
	}
	return cmd
}

func skillKnownFlags(root *cobra.Command) map[string]bool {
	known := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			known[f.Name] = true
			if f.Shorthand != "" {
				known[f.Shorthand] = true
			}
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	return known
}

// skillHead names a bullet in a failure message without reprinting all of it.
func skillHead(item string) string {
	if len(item) > 72 {
		return item[:72] + "…"
	}
	return item
}

// skillFlagName reports the flag a token names, or ok=false when the token is
// not a checkable flag (a bare `-`, or `--help`, which cobra installs lazily).
func skillFlagName(token string) (string, bool) {
	name := strings.TrimLeft(token, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if name == "" || name == "help" {
		return "", false
	}
	return name, true
}

// skillHasFlag consults every set a real invocation could satisfy. LocalFlags
// covers the command's own persistent flags — `Flags()` alone does not, which
// would make the root's global `--json` and `--config` look unrecognized.
func skillHasFlag(cmd *cobra.Command, name string) bool {
	if len(name) == 1 {
		return cmd.LocalFlags().ShorthandLookup(name) != nil || cmd.InheritedFlags().ShorthandLookup(name) != nil
	}
	return cmd.LocalFlags().Lookup(name) != nil || cmd.InheritedFlags().Lookup(name) != nil
}
