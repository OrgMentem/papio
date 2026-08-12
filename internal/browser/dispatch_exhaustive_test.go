// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A protocol-defined request the extension can send, with no dispatch case in
// the bridge, is not "unsupported" — it falls through to the generic
// unknown-frame default, which returns ErrInvalidFrame. That error is the
// transport/framing class, which is fatal by contract, so the daemon tears the
// browser session down instead of answering. institutional_reconcile_request
// shipped exactly that way: the handler existed, the protocol validated the
// frame, the extension sent it after every restart to re-sync bindings, and
// each one disconnected the session it was repairing. The feature-disabled
// branch answered it politely, so the only covered path was the one that
// could not fail.
//
// This walks the real source rather than a hand-maintained list, because a
// hand-maintained list is the same failure one level up: it silently covers
// less as the protocol grows. The extension side has the mirror guard in
// extension/test/correlated-types.test.ts, which pins every requestNative
// expectedType into CORRELATED_RESULT_TYPES for the same reason.
func TestEveryInboundRequestTypeHasADispatchCase(t *testing.T) {
	fset := token.NewFileSet()

	protoFile, err := parser.ParseFile(fset, "../protocol/protocol.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing protocol.go: %v", err)
	}
	requestConst := regexp.MustCompile(`^Msg\w*Request$`)
	requests := map[string]bool{}
	for _, decl := range protoFile.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if requestConst.MatchString(name.Name) {
					requests[name.Name] = true
				}
			}
		}
	}
	if len(requests) == 0 {
		t.Fatal("found no Msg*Request constants; the parser stopped matching the protocol")
	}

	bridgeFile, err := parser.ParseFile(fset, "bridge.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing bridge.go: %v", err)
	}

	protocolSelector := func(expr ast.Expr) string {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "protocol" {
			return ""
		}
		return sel.Sel.Name
	}

	// Only cases in the function that owns the unknown-frame default count.
	// A type can appear in a case clause elsewhere and still be undispatched:
	// institutional_reconcile_request had one in the recognition predicate and
	// another in the feature-disabled responder, and was fatal regardless,
	// because neither of those is the switch that routes a live frame. The
	// dispatcher is identified by carrying both the ErrInvalidFrame default and
	// the bulk of the protocol case clauses, so neither a helper that merely
	// mentions the error nor a small unrelated switch can be mistaken for it.
	casesIn := func(fn *ast.FuncDecl) map[string]bool {
		found := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if name := protocolSelector(expr); name != "" {
					found[name] = true
				}
			}
			return true
		})
		return found
	}
	dispatched := map[string]bool{}
	for _, decl := range bridgeFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		mentionsInvalidFrame := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && ident.Name == "ErrInvalidFrame" {
				mentionsInvalidFrame = true
				return false
			}
			return true
		})
		if !mentionsInvalidFrame {
			continue
		}
		if found := casesIn(fn); len(found) > len(dispatched) {
			dispatched = found
		}
	}

	outbound := map[string]bool{}
	ast.Inspect(bridgeFile, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// b.frame(protocol.MsgX, ...) is the daemon sending a frame, so that
		// type is outbound and owes no inbound dispatch case.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "frame" || len(call.Args) == 0 {
			return true
		}
		if name := protocolSelector(call.Args[0]); name != "" {
			outbound[name] = true
		}
		return true
	})
	if len(dispatched) == 0 {
		t.Fatal("found no protocol.Msg* case clauses in bridge.go; the parser stopped matching the dispatch")
	}

	var undispatched []string
	for name := range requests {
		if dispatched[name] || outbound[name] {
			continue
		}
		undispatched = append(undispatched, name)
	}
	sort.Strings(undispatched)
	if len(undispatched) > 0 {
		t.Fatalf("inbound request types with no dispatch case in bridge.go: %s\n"+
			"each one reaches the unknown-frame default, which is ErrInvalidFrame and tears down the browser session.\n"+
			"add a case to the dispatch switch, or send it via b.frame if it is outbound-only.",
			strings.Join(undispatched, ", "))
	}
}
