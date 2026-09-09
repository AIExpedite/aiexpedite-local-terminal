package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A direct Grok child can write its startup `billing: fetched credits config`
// record the instant it is spawned, and grokRecordBelongsToCurrentAccount only
// accepts an identity logged EARLIER in unified.jsonl than the record it is
// attributing. So StartSession MUST append the identity before proc.Start() —
// appending afterwards leaves the whole direct-run capture dependent on which
// of parent and child the scheduler runs first, which is exactly the silent
// regression this pins.
//
// The ordering cannot be pinned behaviourally: both orders are a race, so a
// spawn-based test would pass under either arrangement on most schedules. The
// invariant is positional, so the assertion is too.
func TestStartSession_AttributesGrokBillingBeforeSpawn(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "session.go", nil, 0)
	if err != nil {
		t.Fatalf("parse session.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "StartSession" && fn.Recv != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("StartSession not found in session.go")
	}

	attribution, start := token.NoPos, token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "ensureGrokBillingAttribution" && !attribution.IsValid() {
				attribution = call.Pos()
			}
		case *ast.SelectorExpr:
			ident, ok := fn.X.(*ast.Ident)
			if ok && ident.Name == "proc" && fn.Sel.Name == "Start" && !start.IsValid() {
				start = call.Pos()
			}
		}
		return true
	})

	if !attribution.IsValid() {
		t.Fatal("StartSession no longer calls ensureGrokBillingAttribution — a direct Grok run's billing records become unattributable")
	}
	if !start.IsValid() {
		t.Fatal("proc.Start() not found in StartSession")
	}
	if attribution > start {
		t.Fatalf("ensureGrokBillingAttribution (%s) runs AFTER proc.Start() (%s) — the child can log its billing record before the identity line exists, so the record is refused",
			fset.Position(attribution), fset.Position(start))
	}
}
