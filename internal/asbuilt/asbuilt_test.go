package asbuilt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// root is the module's root, two folders up from this package's.
const root = "../.."

// read is each exported error var the non-test files of the package in dir declare, an Err name of
// a var at the top of a file, and each place those files read one after init: anywhere but in a
// top-level var's value, which runs once at init, and a func literal there runs later. Every file
// is read, whatever system or tag it builds for.
func read(dir string) (vars, live []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
	}
	for _, f := range files {
		for _, spec := range topVars(f) {
			for _, name := range spec.Names {
				if strings.HasPrefix(name.Name, "Err") {
					vars = append(vars, name.Name)
				}
			}
		}
	}
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr: // another package's name, or a field, is not this package's var
			ast.Inspect(n.X, visit)
			return false
		case *ast.Field: // a field's name is no read
			ast.Inspect(n.Type, visit)
			return false
		case *ast.Ident:
			if slices.Contains(vars, n.Name) {
				live = append(live, fset.Position(n.Pos()).String())
			}
		}
		return true
	}
	for _, f := range files {
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
				ast.Inspect(g, func(n ast.Node) bool {
					if lit, ok := n.(*ast.FuncLit); ok {
						ast.Inspect(lit.Body, visit)
						return false
					}
					return true
				})
				continue
			}
			ast.Inspect(d, visit)
		}
	}
	return vars, live, nil
}

// topVars is the var specs at the top of f.
func topVars(f *ast.File) []*ast.ValueSpec {
	var out []*ast.ValueSpec
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
			for _, s := range g.Specs {
				out = append(out, s.(*ast.ValueSpec)) //nolint:forcetypeassert // a var's specs are value specs
			}
		}
	}
	return out
}

// libraries is the folder of each package under root but internal's, which hold no gate: each folder that holds a Go file, testdata and dot folders aside.
func libraries(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || rel == "internal") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, filepath.Dir(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return slices.Compact(out)
}

// TestNoGateReadsAnExportedErrorVarAfterInit: no code of go-proc's libraries reads an exported error
// var of its own package after init, on any system, so each gate returns, wraps and matches the
// value the var was built with, through its unexported copy. The vars the gates of the libraries
// refuse by are among those read, so the check is not empty.
//
// What it costs when this fails: a gate reads the exported var as it is when the gate runs, so
// `proc.ErrStale = nil`, or the same in any exported error var, anywhere in the binary turns that
// gate's refusal into a go: Stop signals a reused pid, sandbox.Run takes a line it should refuse.
func TestNoGateReadsAnExportedErrorVarAfterInit(t *testing.T) {
	t.Parallel()
	var all, live []string
	for _, dir := range libraries(t) {
		vars, reads, err := read(dir)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, vars...)
		live = append(live, reads...)
	}
	if len(live) != 0 {
		t.Errorf("an exported error var is read after init, so a value stored in it later changes what the gate does:\n%s", strings.Join(live, "\n"))
	}
	for _, name := range []string{"ErrStale", "ErrRefused", "ErrBatch", "ErrNotProgram", "ErrUsage", "ErrNoBlock", "ErrAbsent", "ErrPercent", "ErrNoAverage"} {
		if !slices.Contains(all, name) {
			t.Errorf("the check read no var %s, so it proves little: read %v", name, all)
		}
	}
}

// TestTheCheckFindsAReadAfterInit: on planted packages the check finds a read after init in a
// function, in a func literal in a top-level var, and in a method, and none in a top-level var's
// value, a copy or a wrap made at init, nor another package's name or a field of the same name.
//
// What it costs when this fails: the check reads the source and never refuses, which is a check
// nothing could turn red.
func TestTheCheckFindsAReadAfterInit(t *testing.T) {
	t.Parallel()
	head := "package p\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n\t\"io/fs\"\n)\n\nvar ErrX = errors.New(\"x\")\n\nvar errX, errW = ErrX, fmt.Errorf(\"%w\", ErrX)\n\n"
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"a copy and a wrap at init, another package's name, a field": {"type s struct{ ErrX error }\n\nfunc f(v s) error { _ = fs.ErrNotExist; return v.ErrX }\n", 0},
		"a return in a function":            {"func f() error { return ErrX }\n", 1},
		"a wrap in a function":              {"func f() error { return fmt.Errorf(\"%w\", ErrX) }\n", 1},
		"a func literal in a top-level var": {"var f = func() error { return ErrX }\n", 1},
		"a match in a method":               {"type t struct{}\n\nfunc (t) Is(e error) bool { return e == ErrX }\n", 1},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(head+tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		vars, live, err := read(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(vars, []string{"ErrX"}) || len(live) != tc.want {
			t.Errorf("%s: read vars %v and reads %v after init, want [ErrX] and %d", name, vars, live, tc.want)
		}
	}
}
