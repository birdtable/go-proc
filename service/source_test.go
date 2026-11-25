package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// personText is each place in f that tests text against a literal with a space in it, which is
// words meant for a person and not a machine answer: a call of strings or bytes from matchers, a ==
// or !=, or a case of a switch; and each import of regexp.
func personText(fset *token.FileSet, f *ast.File) []string {
	// matchers are the calls of strings and bytes that test text against text. ContainsAny is not
	// among them: its argument is a set of characters, not words.
	matchers := []string{"Contains", "HasPrefix", "HasSuffix", "Index", "LastIndex", "EqualFold", "Cut", "CutPrefix", "CutSuffix", "Count", "Split"}
	var found []string
	at := func(n ast.Node, what string) { found = append(found, fset.Position(n.Pos()).String()+": "+what) }
	words := func(e ast.Expr) bool {
		if conv, ok := e.(*ast.CallExpr); ok && len(conv.Args) == 1 {
			if _, ok := conv.Fun.(*ast.ArrayType); ok {
				e = conv.Args[0] // []byte("...") is its literal
			}
		}
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(lit.Value)
		return err == nil && strings.ContainsAny(s, " \t")
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == `"regexp"` {
			at(imp, "imports regexp")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || (pkg.Name != "strings" && pkg.Name != "bytes") || !slices.Contains(matchers, sel.Sel.Name) {
				return true
			}
			if slices.ContainsFunc(n.Args, words) {
				at(n, pkg.Name+"."+sel.Sel.Name+" of words")
			}
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) && (words(n.X) || words(n.Y)) {
				at(n, "a comparison with words")
			}
		case *ast.CaseClause:
			if slices.ContainsFunc(n.List, words) {
				at(n, "a case of words")
			}
		}
		return true
	})
	return found
}

// exported is each exported name f declares that the package must not have: a Confirm of any kind,
// and a variable other than the package's three errors, since a caller can write an exported variable.
func exported(f *ast.File) []string {
	var found []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if strings.HasPrefix(d.Name.Name, "Confirm") {
				found = append(found, "func "+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				var names []*ast.Ident
				switch s := spec.(type) {
				case *ast.ValueSpec:
					names = s.Names
				case *ast.TypeSpec:
					names = []*ast.Ident{s.Name}
				}
				for _, n := range names {
					switch {
					case strings.HasPrefix(n.Name, "Confirm"):
						found = append(found, d.Tok.String()+" "+n.Name)
					case d.Tok == token.VAR && n.IsExported() && !slices.Contains([]string{"ErrAbsent", "ErrPercent", "ErrUsage"}, n.Name):
						found = append(found, "var "+n.Name)
					}
				}
			}
		}
	}
	return found
}

// TestNoSourceLineDecidesByWordsMeantForAPerson: no Go file of the package that is no test matches
// a tool's output against words, imports regexp, exports a Confirm, or exports a variable other
// than ErrAbsent, ErrPercent and ErrUsage; and a planted file that does each is found, once for each.
//
// What it costs when this fails: a query decides by an English phrase and answers wrongly on a
// machine in another language or after the tool rewords it, a caller is asked to confirm what the
// tool's own answer should settle, or a caller can swap a value the package decides by.
func TestNoSourceLineDecidesByWordsMeantForAPerson(t *testing.T) {
	t.Parallel()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		read++
		for _, found := range append(personText(fset, f), exported(f)...) {
			t.Errorf("%s: %s", name, found)
		}
	}
	if read < 3 {
		t.Fatalf("read %d files of the package, want its 3 or more", read)
	}

	planted := `package service
import ("bytes"; "regexp"; "strings")
var Hook = func() {}
var ErrAbsent error
type ConfirmPrompt struct{}
func Confirm() bool { return true }
func f(out []byte, s string) {
	_ = strings.Contains(s, "could not find")
	_ = bytes.HasPrefix(out, []byte("ERROR: no such"))
	_ = strings.EqualFold("Not Found", s)
	_ = s == "not found"
	switch s { case "Access is denied.": }
	_ = regexp.MustCompile
	_ = strings.Contains(s, "not-found")
	_ = s == "loaded"
	_ = strings.Join([]string{"a b"}, " ")
}
`
	f, err := parser.ParseFile(fset, "planted.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := personText(fset, f)
	for i := range got {
		_, got[i], _ = strings.Cut(got[i], ": ")
	}
	want := []string{"imports regexp", "strings.Contains of words", "bytes.HasPrefix of words", "a comparison with words", "a case of words", "strings.EqualFold of words"}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("the planted file's %q is not found: %q", w, got)
		}
	}
	if len(got) != 6 {
		t.Errorf("the planted file gives %d findings, want 6: %q", len(got), got)
	}
	if got, want := exported(f), []string{"var Hook", "type ConfirmPrompt", "func Confirm"}; !slices.Equal(got, want) {
		t.Errorf("the planted file's exports give %q, want %q", got, want)
	}
	if _, err := os.Stat("plan.go"); err != nil {
		t.Fatalf("the test does not run in the package's folder: %v", err)
	}
}
