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

	// The spawn body lives in StartSessionResuming; StartSession is the
	// seed-less entry point that delegates to it (cli_conversation_resume.go).
	// The invariant follows the BODY — and the delegation is pinned too, so a
	// future StartSession that grew its own spawn path would not slip past this
	// positional check unexamined.
	var body *ast.BlockStmt
	delegates := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		switch fn.Name.Name {
		case "StartSessionResuming":
			body = fn.Body
		case "StartSession":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "StartSessionResuming" {
					delegates = true
				}
				return true
			})
		}
	}
	if body == nil {
		t.Fatal("StartSessionResuming (the session spawn body) not found in session.go")
	}
	if !delegates {
		t.Fatal("StartSession no longer delegates to StartSessionResuming — it has its own spawn path, which this ordering check does not cover")
	}

	attribution, start := token.NoPos, token.NoPos
	var attributionArgs []ast.Expr
	keeperArg := ""
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "ensureGrokBillingIdentityNamed" && !attribution.IsValid() {
				attribution = call.Pos()
				attributionArgs = call.Args
			}
			if fn.Name == "startGrokBillingAttributionKeeper" && keeperArg == "" && len(call.Args) == 1 {
				if ident, ok := call.Args[0].(*ast.Ident); ok {
					keeperArg = ident.Name
				}
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
		t.Fatal("StartSession no longer names a Grok billing identity — a direct Grok run's billing records become unattributable")
	}
	if !start.IsValid() {
		t.Fatal("proc.Start() not found in StartSession")
	}
	if attribution > start {
		t.Fatalf("the billing identity (%s) is named AFTER proc.Start() (%s) — the child can log its billing record before the identity line exists, so the record is refused",
			fset.Position(attribution), fset.Position(start))
	}

	// The marker and the keeper must carry ONE decision. Re-resolving the
	// credentials for the marker (ensureGrokBillingAttribution) re-reads the
	// auth cache and every config layer, so a source that changes between the
	// two calls leaves the log naming one account while the keeper reasserts
	// another on its next tick — and the reassertion wins, binding the child's
	// records to the account the override just ruled out. Positional for the
	// same reason as the ordering above: the divergence is a race between the
	// two reads, so no spawn-based test observes it reliably.
	if keeperArg == "" {
		t.Fatal("startGrokBillingAttributionKeeper is no longer armed with a named identity variable in StartSession")
	}
	if len(attributionArgs) != 2 {
		t.Fatalf("ensureGrokBillingIdentityNamed takes %d args in StartSession, want 2", len(attributionArgs))
	}
	named, ok := attributionArgs[1].(*ast.Ident)
	if !ok || named.Name != keeperArg {
		t.Fatalf("ensureGrokBillingIdentityNamed is passed %s, but the keeper was armed with %q — the marker and the keeper must share one resolution of the direct identity",
			exprName(attributionArgs[1]), keeperArg)
	}
}

func exprName(e ast.Expr) string {
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return "a re-resolved expression"
}
