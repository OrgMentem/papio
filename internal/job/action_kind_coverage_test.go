// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"papio/internal/protocol"
)

// TestActionKindDispositionIsExhaustive guards every named human-action kind
// at the source of truth. A new ActionKind must be consciously disposed by
// every user-facing and protocol surface; otherwise this test names the site
// that was forgotten.
func TestActionKindDispositionIsExhaustive(t *testing.T) {
	root := repoRoot(t)
	jobPath := filepath.Join(root, "internal", "job", "job.go")
	file, err := parser.ParseFile(token.NewFileSet(), jobPath, nil, 0)
	if err != nil {
		t.Fatalf("parse job.go: %v", err)
	}

	sources := map[string]string{}
	for name, path := range map[string]string{
		"action guidance":          filepath.Join(root, "internal", "app", "action_guidance.go"),
		"reminder vocabulary":      filepath.Join(root, "internal", "app", "action_reminder.go"),
		"open-action explanations": filepath.Join(root, "internal", "errcat", "errcat.go"),
		"inbox dismissal table":    filepath.Join(root, "extension", "src", "inbox.ts"),
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		source := string(contents)
		switch name {
		case "open-action explanations":
			source = sourceRegion(source, "func explainOpenAction(", "func explainNoAccess(")
		case "inbox dismissal table":
			source = sourceRegion(source, "const DISMISS_DISPOSITION", "};")
		}
		sources[name] = source
	}

	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if !strings.HasPrefix(name.Name, "ActionKind") {
					continue
				}
				declared++
				if len(value.Values) != len(value.Names) {
					t.Errorf("%s has %d names and %d values; ActionKind constants must have one string value", name.Name, len(value.Names), len(value.Values))
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s is not a string literal ActionKind", name.Name)
					continue
				}
				kind, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Errorf("unquote %s (%s): %v", name.Name, literal.Value, err)
					continue
				}

				for site, source := range sources {
					// Go disposition tables may use the named constant rather than
					// duplicating its persisted string. The extension table is
					// TypeScript and must carry the wire string itself.
					needle := strconv.Quote(kind)
					if site != "inbox dismissal table" {
						if strings.Contains(source, needle) || containsIdentifier(source, name.Name) {
							continue
						}
					} else if strings.Contains(source, needle+":") {
						continue
					}
					t.Errorf("ActionKind %s (%q) is missing from %s", name.Name, kind, site)
				}
				if !containsProtocolRouteClass(kind) {
					t.Errorf("ActionKind %s (%q) is missing from protocol.TriageRouteClasses()", name.Name, kind)
				}
			}
		}
	}
	if declared == 0 {
		t.Fatal("job.go declares no ActionKind string constants")
	}
}

func containsProtocolRouteClass(kind string) bool {
	for _, route := range protocol.TriageRouteClasses() {
		if route == kind {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate action kind coverage test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func sourceRegion(source, start, end string) string {
	begin := strings.Index(source, start)
	if begin < 0 {
		return ""
	}
	source = source[begin:]
	if finish := strings.Index(source, end); finish >= 0 {
		return source[:finish]
	}
	return source
}

func containsIdentifier(source, identifier string) bool {
	for offset := 0; ; {
		index := strings.Index(source[offset:], identifier)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !isIdentifierByte(source[index-1])
		after := index + len(identifier)
		afterOK := after == len(source) || !isIdentifierByte(source[after])
		if beforeOK && afterOK {
			return true
		}
		offset = after
	}
}

func isIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
