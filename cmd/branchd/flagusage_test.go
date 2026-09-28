package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// The flag package treats a back-quoted word in a usage string as the flag's
// argument name, so `branchd -h` printed "-advertise-proxy-addr pgb connect"
// and "-rotate-branch-credentials password" (a bool flag that takes no
// argument). No usage string may contain a backquote.
func TestFlagUsageHasNoBackquotes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "flag" {
			return true
		}
		lit, ok := call.Args[2].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		usage, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
		}
		n++
		if strings.Contains(usage, "`") {
			name, _ := strconv.Unquote(call.Args[0].(*ast.BasicLit).Value)
			t.Errorf("%s: usage of -%s contains a backquote, which flag prints as the argument name: %q", fset.Position(lit.Pos()), name, usage)
		}
		return true
	})
	if n < 20 {
		t.Fatalf("found only %d flag definitions with a literal usage string in main.go; the scan is broken", n)
	}
}
