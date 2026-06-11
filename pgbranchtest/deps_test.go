package pgbranchtest

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoInternalImports enforces the package contract: the production code of
// pgbranchtest is a self-contained client (stdlib only) so importing it never
// couples consumers to pgbranch internals. Test files are exempt — the
// integration test bootstraps a real server from internal packages.
func TestNoInternalImports(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for path, f := range pkg.Files {
			if strings.HasSuffix(filepath.Base(path), "_test.go") {
				continue
			}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if strings.Contains(p, "/internal/") || strings.Contains(p, ".") {
					t.Errorf("%s imports %q: production pgbranchtest code must be stdlib-only", path, p)
				}
			}
		}
	}
}
