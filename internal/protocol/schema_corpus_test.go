// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The published JSON Schemas under protocol/ are the third representation of
// the wire contract, beside this package and extension/src/protocol.ts. The
// corpus tests in protocol_test.go pin the two executable parsers; nothing
// pinned the schemas, and two defects shipped through that hole:
//
//   - browser-v1.schema.json wrote every NUL guard as "^[^\\u0000]*$", which
//     JSON-decodes to the ECMA-262 escape \u0000. That is legal per the
//     JSON Schema spec but not a regex any RE2-based validator can compile,
//     so the whole schema failed its own metaschema and no Go consumer could
//     load it at all. The portable spelling "^[^\u0000]*$" decodes to a
//     literal NUL inside the class and compiles under both engines.
//   - provider_outcome was absent from the top-level type enum while owning a
//     conditional branch, a Msg* const, and a valid fixture, so the published
//     schema rejected a frame both parsers accept.
//
// Both are permanently closed here. Adding a message type now requires the
// enum entry, not just the const and the branch.

package protocol

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaRuntimeOnly names invalid fixtures whose defect the published schema
// cannot express, so schema acceptance is correct and the executable parsers
// are the only fail-closed layer. Each entry states the invariant. The test
// below fails both when an unlisted fixture is accepted and when a listed one
// is rejected, so the map cannot silently over-cover as the schemas tighten.
var schemaRuntimeOnly = map[string]string{
	"acquisition-bundle-path-mismatch.json":                   "artifact path must equal the SHA-256 digest; a cross-field identity JSON Schema has no vocabulary for",
	"browser-job-offer-invalid-date.json":                     "expected-completion timestamp is validated as a real RFC 3339 instant; JSON Schema format is annotation-only by default",
	"browser-triage-snapshot-counts-pending-mismatch-v3.json": "counts must agree with the item array they summarise; a cross-field arithmetic invariant",
}

func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func compileSchema(t *testing.T, rel string) *jsonschema.Schema {
	t.Helper()
	path := repoPath(t, "protocol", rel)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(rel, doc); err != nil {
		t.Fatalf("add %s: %v", rel, err)
	}
	schema, err := c.Compile(rel)
	if err != nil {
		t.Fatalf("compile %s: %v", rel, err)
	}
	return schema
}

func publishedSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	out := map[string]*jsonschema.Schema{}
	for _, name := range []string{
		"browser-v1.schema.json",
		"work-request-v1.schema.json",
		"acquisition-bundle-v1.schema.json",
		"acquisition-bundle-v2.schema.json",
	} {
		out[name] = compileSchema(t, name)
	}
	return out
}

// schemaForFixture mirrors decodeByPrefix so a fixture is validated against the
// same contract its decoder implements. Bundles carry the version as the string
// const, not an integer; selecting on a bare "2" silently falls through to v1
// and reports a healthy fixture as broken.
func schemaForFixture(t *testing.T, name string, doc any, schemas map[string]*jsonschema.Schema) *jsonschema.Schema {
	t.Helper()
	switch {
	case strings.HasPrefix(name, "browser-"):
		return schemas["browser-v1.schema.json"]
	case strings.HasPrefix(name, "work-request-"):
		return schemas["work-request-v1.schema.json"]
	case strings.HasPrefix(name, "acquisition-bundle-"):
		fields, ok := doc.(map[string]any)
		if !ok {
			t.Fatalf("fixture %s is not a JSON object", name)
		}
		switch fields["schema_version"] {
		case AcquisitionBundleSchemaVersion:
			return schemas["acquisition-bundle-v1.schema.json"]
		case AcquisitionBundleSchemaVersionV2:
			return schemas["acquisition-bundle-v2.schema.json"]
		default:
			t.Fatalf("fixture %s has schema_version %v with no published schema", name, fields["schema_version"])
		}
	}
	t.Fatalf("fixture %s maps to no published schema", name)
	return nil
}

func corpusFixtures(t *testing.T, kind string) []string {
	t.Helper()
	dir := repoPath(t, "testdata", "protocol", kind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func readFixture(t *testing.T, kind, name string) any {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "testdata", "protocol", kind, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture %s is not JSON: %v", name, err)
	}
	return doc
}

// TestPublishedSchemasCompile is the cheapest guard in this file and the one
// that was missing: an uncompilable schema makes every other schema assertion
// vacuous, and browser-v1 was uncompilable for its whole life.
func TestPublishedSchemasCompile(t *testing.T) {
	publishedSchemas(t)
}

func TestValidCorpusMatchesPublishedSchemas(t *testing.T) {
	schemas := publishedSchemas(t)
	for _, name := range corpusFixtures(t, "valid") {
		doc := readFixture(t, "valid", name)
		if err := schemaForFixture(t, name, doc, schemas).Validate(doc); err != nil {
			t.Errorf("valid fixture %s rejected by its published schema: %v", name, err)
		}
	}
}

func TestInvalidCorpusSchemaDispositionIsPinned(t *testing.T) {
	schemas := publishedSchemas(t)
	seen := map[string]bool{}
	for _, name := range corpusFixtures(t, "invalid") {
		doc := readFixture(t, "invalid", name)
		accepted := schemaForFixture(t, name, doc, schemas).Validate(doc) == nil
		reason, runtimeOnly := schemaRuntimeOnly[name]
		seen[name] = true
		switch {
		case accepted && !runtimeOnly:
			t.Errorf("invalid fixture %s accepted by its published schema; either express the defect in the schema or record the runtime-only invariant in schemaRuntimeOnly", name)
		case !accepted && runtimeOnly:
			t.Errorf("invalid fixture %s is now rejected by its published schema, so schemaRuntimeOnly[%q] (%s) is stale; drop the entry", name, name, reason)
		}
	}
	for name := range schemaRuntimeOnly {
		if !seen[name] {
			t.Errorf("schemaRuntimeOnly names %s, which is not in the invalid corpus", name)
		}
	}
}

// goMessageTypes reads the Msg* const block rather than a hand-kept list, so a
// new message type joins this vocabulary the moment it is declared.
func goMessageTypes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "protocol.go", nil, 0)
	if err != nil {
		t.Fatalf("parse protocol.go: %v", err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			if !strings.HasPrefix(value.Names[0].Name, "Msg") {
				continue
			}
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", value.Names[0].Name, err)
			}
			out = append(out, unquoted)
		}
	}
	slices.Sort(out)
	return out
}

var tsUnionRE = regexp.MustCompile(`(?s)export type BrowserMessageType\s*=(.*?);`)

func tsMessageTypes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "extension", "src", "protocol.ts"))
	if err != nil {
		t.Fatalf("read protocol.ts: %v", err)
	}
	match := tsUnionRE.FindSubmatch(raw)
	if match == nil {
		t.Fatal("extension/src/protocol.ts declares no BrowserMessageType union")
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllSubmatch(match[1], -1) {
		out = append(out, string(m[1]))
	}
	slices.Sort(out)
	return out
}

func schemaMessageTypes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "protocol", "browser-v1.schema.json"))
	if err != nil {
		t.Fatalf("read browser schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse browser schema: %v", err)
	}
	out := slices.Clone(doc.Properties.Type.Enum)
	slices.Sort(out)
	return out
}

// TestBrowserMessageVocabularyIsTriMaintained is the drift guard for the trap
// that hid provider_outcome: the type is declared in three places and adding it
// to two of them looks complete from either side.
func TestBrowserMessageVocabularyIsTriMaintained(t *testing.T) {
	goTypes := goMessageTypes(t)
	if len(goTypes) < 8 {
		t.Fatalf("parsed %d Msg* consts from protocol.go, want the full vocabulary", len(goTypes))
	}
	for _, other := range []struct {
		name  string
		types []string
	}{
		{"extension/src/protocol.ts BrowserMessageType", tsMessageTypes(t)},
		{"protocol/browser-v1.schema.json properties.type.enum", schemaMessageTypes(t)},
	} {
		if slices.Equal(goTypes, other.types) {
			continue
		}
		t.Errorf("message vocabulary drift between internal/protocol Msg* consts and %s:\n  missing from %s: %s\n  absent from Msg* consts: %s",
			other.name, other.name, describeDiff(goTypes, other.types), describeDiff(other.types, goTypes))
	}
}

// TestGuidanceVariantVocabularyIsTriMaintained is the same trap one level down.
// guidance_variant is a closed enum declared in protocol.go, protocol.ts and
// browser-v1.schema.json; landing a new member in two of the three leaves the
// daemon emitting a value the extension or the published schema rejects, and
// the row silently drops out of its task family. Order is part of the contract
// here — the TS lock test compares the array literally — so compare as-is.
func TestGuidanceVariantVocabularyIsTriMaintained(t *testing.T) {
	goVariants := TriageGuidanceVariants()
	if len(goVariants) < 11 {
		t.Fatalf("parsed %d guidance variants, want the full vocabulary", len(goVariants))
	}
	for _, other := range []struct {
		name     string
		variants []string
	}{
		{"extension/src/protocol.ts GUIDANCE_VARIANTS", tsGuidanceVariants(t)},
		{"protocol/browser-v1.schema.json family_runs.guidance_variant.enum", schemaGuidanceVariants(t)},
	} {
		if slices.Equal(goVariants, other.variants) {
			continue
		}
		t.Errorf("guidance variant drift between internal/protocol and %s:\n  go: %q\n  %s: %q",
			other.name, goVariants, other.name, other.variants)
	}
}

var tsGuidanceRE = regexp.MustCompile(`(?s)export const GUIDANCE_VARIANTS\s*=\s*\[(.*?)\]`)

func tsGuidanceVariants(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "extension", "src", "protocol.ts"))
	if err != nil {
		t.Fatalf("read protocol.ts: %v", err)
	}
	match := tsGuidanceRE.FindSubmatch(raw)
	if match == nil {
		t.Fatal("extension/src/protocol.ts declares no GUIDANCE_VARIANTS array")
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllSubmatch(match[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

func schemaGuidanceVariants(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "protocol", "browser-v1.schema.json"))
	if err != nil {
		t.Fatalf("read browser schema: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties struct {
				GuidanceVariant *struct {
					Enum []string `json:"enum"`
				} `json:"guidance_variant"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse browser schema: %v", err)
	}
	for _, def := range doc.Defs {
		if def.Properties.GuidanceVariant != nil {
			return slices.Clone(def.Properties.GuidanceVariant.Enum)
		}
	}
	t.Fatal("browser schema declares no guidance_variant enum")
	return nil
}

func describeDiff(have, want []string) string {
	var only []string
	for _, v := range have {
		if !slices.Contains(want, v) {
			only = append(only, v)
		}
	}
	if len(only) == 0 {
		return "(none)"
	}
	return fmt.Sprintf("%q", only)
}
