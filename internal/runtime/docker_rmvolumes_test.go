package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleRoot walks up from the package directory to the directory holding
// go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}

// Every container removal in the module, integration tests included, passes
// RemoveVolumes: true. The postgres image declares VOLUME
// /var/lib/postgresql/data, so docker gives each branch container an
// anonymous volume; removing the container without RemoveVolumes (the API
// form of `docker rm -v`) leaves that volume behind, one per removal. Named
// volumes are never touched by it, so there is no case for leaving it off.
func TestContainerRemovalsDropAnonymousVolumes(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	calls := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			// hidden trees (.git, editor and agent worktrees under .claude),
			// build output, and nested modules are not this module's code
			switch name := d.Name(); {
			case strings.HasPrefix(name, "."), name == "node_modules", name == "dist", name == "site", name == "bin":
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ContainerRemove" || len(call.Args) != 3 {
				return true
			}
			calls++
			if !setsRemoveVolumes(call.Args[2]) {
				pos := fset.Position(call.Pos())
				rel, _ := filepath.Rel(root, pos.Filename)
				t.Errorf("%s:%d: ContainerRemove without RemoveVolumes: true leaks the container's anonymous volumes", rel, pos.Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("found no ContainerRemove calls; the scan is not looking at the module")
	}
}

// setsRemoveVolumes reports whether expr is a RemoveOptions literal with
// RemoveVolumes: true. Anything else (a variable, a literal without the
// field) cannot be shown to remove the volumes.
func setsRemoveVolumes(expr ast.Expr) bool {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		val, vok := kv.Value.(*ast.Ident)
		if ok && vok && key.Name == "RemoveVolumes" && val.Name == "true" {
			return true
		}
	}
	return false
}

// dockerRmLine matches a `docker rm` / `docker container rm` command at the
// start of a script or code-block line (prose mentioning `docker rm` in
// backticks does not start with it).
var dockerRmLine = regexp.MustCompile(`^\s*docker (?:container )?rm\b(.*)$`)

// The same holds for the scripts and the commands the docs tell people to run:
// `docker rm` of a postgres container needs -v (--volumes).
func TestDockerRmCommandsDropAnonymousVolumes(t *testing.T) {
	root := moduleRoot(t)
	var files []string
	for _, glob := range []string{"hack/*.sh", "docs/*.md", "*.md", "action/*.yml", ".github/workflows/*.yml"} {
		m, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			m := dockerRmLine.FindStringSubmatch(line)
			if m == nil || removesVolumesFlag(m[1]) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s:%d: %q removes a container without -v, leaking its anonymous volumes", rel, i+1, strings.TrimSpace(line))
		}
	}
}

// removesVolumesFlag reports whether docker rm's arguments include -v or
// --volumes, alone or in a short-flag group such as -fv.
func removesVolumesFlag(args string) bool {
	for _, f := range strings.Fields(args) {
		if f == "--volumes" || (strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f, "v")) {
			return true
		}
	}
	return false
}
