package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// sessionLaunchFields are set once, in a session's literal, from what its
// child was launched with. The stream readers and the follow-up gate read them
// without a lock (ACPManager.readStream reads liveFollowUps on every frame),
// which is correct only while nothing writes them after construction.
var sessionLaunchFields = map[string]bool{"liveFollowUps": true, "launchedVersion": true}

// A write to a launch field after the session literal — in a test or in the
// product — races the session's reader. The uncertified Muse Code follow-up
// test once did exactly that, and only the Linux `-race` job could see it, and
// only under full-suite load. This rule holds on every platform,
// every run: the fields are composite-literal keys, never assignment targets.
func TestSessionLaunchFieldsAreSetOnlyInTheirLiteral(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	literalKeys := map[string]bool{}
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sessionLaunchFields[sel.Sel.Name] {
						violations = append(violations, fset.Position(node.Pos()).String()+": assigns ."+sel.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "liveFollowUps" {
					literalKeys[name] = true
				}
			}
			return true
		})
	}
	// An empty or misdirected scan must not pass: the production session
	// literals that set the tracker are where the rule is kept.
	for _, want := range []string{"acp_family.go", "codex_appserver.go"} {
		if !literalKeys[want] {
			t.Errorf("no liveFollowUps literal key found in %s: the scan did not read the package", want)
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("%s after construction; set it in the session literal instead", v)
	}
}
