// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// internal/nativehost/host.go treats any non-nil error out of Bridge.Sync as a
// bad connection and tears down the native-messaging session, so a handler that
// returns a plain error for a routine condition disconnects the user's browser
// instead of failing one request. reviewPreview shipped that bug: every click on
// a stale review action silently dropped the extension.
//
// A one-off audit walked all 37 error returns in bridge.go and found every leaf
// handler already correct, and correct in exactly one shape: the error is the
// marshal failure of the response frame itself. That made the invariant
// mechanical, so it belongs in a test rather than in a repeated audit. This is
// that test.
//
// It fails on any new error return in a leaf handler that is not that shape,
// which is precisely when a human should look. Adding another legitimate
// marshal return needs no change here.

package browser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// failClosedHubs are the three methods excluded from the leaf-handler rule.
// They are the frame loop, the offer pump, and the dispatcher, not leaf
// handlers: Sync's own errors are the transport contract rather than an
// application condition, and poll/handle route frames rather than answer one
// request. Their error returns are reviewed by hand.
var failClosedHubs = map[string]string{
	"Sync":   "frame loop; its errors are the transport contract itself",
	"poll":   "offer pump; not a request/response handler",
	"handle": "dispatcher; routes frames rather than answering one",
}

// frameBuilders construct a response frame and are the only calls whose error
// may legitimately be returned raw from a leaf handler. sessionBusy qualifies
// for the same reason frame and helloAck do: its sole error is the marshal of
// the error frame it builds, and handleHello now composes it with an ack
// rather than tail-returning it.
var frameBuilders = map[string]bool{"frame": true, "helloAck": true, "sessionBusy": true}

func TestLeafHandlersOnlyReturnFrameMarshalErrors(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bridge.go", nil, 0)
	if err != nil {
		t.Fatalf("parse bridge.go: %v", err)
	}

	checked, sites := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !isBridgeMethod(fn) || !isHandlerSignature(fset, fn) {
			continue
		}
		if _, hub := failClosedHubs[fn.Name.Name]; hub {
			continue
		}
		checked++
		sites += checkHandler(t, fset, fn)
	}

	if checked < 20 {
		t.Fatalf("inspected %d leaf handlers, want the full set; the signature match is probably broken", checked)
	}
	if sites == 0 {
		t.Fatal("found no error returns at all in leaf handlers; the walk is probably broken")
	}
	for name := range failClosedHubs {
		if findFunc(file, name) == nil {
			t.Errorf("failClosedHubs names %s, which no longer exists in bridge.go", name)
		}
	}
}

// checkHandler reports every error return in fn that is not the frame-marshal
// idiom, and returns how many error returns it saw.
//
// It works in two passes because both halves matter. The first pass records the
// returns that are provably the idiom: the guarded return must directly follow
// the frame call that bound its error, since matching the variable name alone
// would accept a store error that happens to reuse err. The second pass then
// requires EVERY error return to be one of those. Checking only the guard shape
// would skip a bare `return nil, fmt.Errorf("item is unavailable")` entirely —
// which is the exact shape of the reviewPreview bug this test exists to catch.
func checkHandler(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) int {
	t.Helper()

	idiomatic := map[*ast.ReturnStmt]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var list []ast.Stmt
		switch block := n.(type) {
		case *ast.BlockStmt:
			list = block.List
		case *ast.CaseClause:
			list = block.Body
		default:
			return true
		}
		for i, stmt := range list {
			ret, name := errorGuardReturn(stmt)
			if ret != nil && i > 0 && bindsFrameError(list[i-1], name) {
				idiomatic[ret] = true
			}
		}
		return true
	})

	seen := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 2 || isNil(ret.Results[1]) {
			return true
		}
		seen++
		if idiomatic[ret] {
			return true
		}
		t.Errorf("%s: %s returns a raw error that is not a frame-marshal failure.\n"+
			"  A non-nil error here disconnects the user's browser, so a routine condition must\n"+
			"  return a structured result instead (see b.triageDecisionResult, b.reviewPreviewError).\n"+
			"  If this really is an unrecoverable internal fault, route it through a frame builder\n"+
			"  or move the method into failClosedHubs with a reason.",
			fset.Position(ret.Pos()), fn.Name.Name)
		return true
	})
	return seen
}

// errorGuardReturn matches `if <name> != nil { return nil, <name> }` and yields
// the return statement it guards, so the caller can mark that exact node.
func errorGuardReturn(stmt ast.Stmt) (*ast.ReturnStmt, string) {
	ifStmt, ok := stmt.(*ast.IfStmt)
	if !ok || ifStmt.Init != nil || ifStmt.Else != nil || len(ifStmt.Body.List) != 1 {
		return nil, ""
	}
	cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ || !isNil(cond.Y) {
		return nil, ""
	}
	guard, ok := cond.X.(*ast.Ident)
	if !ok {
		return nil, ""
	}
	ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 2 || !isNil(ret.Results[0]) {
		return nil, ""
	}
	returned, ok := ret.Results[1].(*ast.Ident)
	if !ok || returned.Name != guard.Name {
		return nil, ""
	}
	return ret, guard.Name
}

// bindsFrameError matches `<frame>, <name> := b.frame(...)` / `b.helloAck(...)`
// / `b.sessionBusy(...)`.
func bindsFrameError(stmt ast.Stmt, name string) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
		return false
	}
	bound, ok := assign.Lhs[1].(*ast.Ident)
	if !ok || bound.Name != name {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == "b" && frameBuilders[sel.Sel.Name]
}

func isNil(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isBridgeMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Bridge"
}

func isHandlerSignature(fset *token.FileSet, fn *ast.FuncDecl) bool {
	results := fn.Type.Results
	if results == nil || len(results.List) != 2 {
		return false
	}
	slice, ok := results.List[0].Type.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	sel, ok := slice.Elt.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "json" || sel.Sel.Name != "RawMessage" {
		return false
	}
	errType, ok := results.List[1].Type.(*ast.Ident)
	return ok && errType.Name == "error" && !strings.Contains(fn.Name.Name, "Test")
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
