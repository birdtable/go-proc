package sandbox

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"
)

// unsafeAt is one argument of a raw call that uses package unsafe: the call,
// as its first argument is written, the argument's place and the argument as
// written.
type unsafeAt struct {
	call string
	arg  int
	form string
}

// rawUnsafe is every use of package unsafe the one raw call may make, and
// no other: seccomp takes its program by address and package syscall has no
// wrapper for it, so kernel_linux.go passes the pointer in the call's own
// arguments, with a nolint for gosec's G103 on the line. The Landlock calls
// are go-landlock's, and openat2 is x/sys unix's Openat2.
func rawUnsafe() []unsafeAt {
	return []unsafeAt{
		{"uintptr(a.seccomp)", 3, "uintptr(unsafe.Pointer(&fprog))"},
	}
}

// onlyImports type-checks one file alone: unsafe is the real package, and
// every other import is an empty package of its path.
type onlyImports struct{}

func (onlyImports) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	pkg := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
	pkg.MarkComplete()
	return pkg, nil
}

// unsafeUses holds src, kernel_linux.go, to rawUnsafe. A name that resolves
// to package unsafe, under any import name or a dot import, must sit in an
// argument of a call into package syscall, and those arguments must be
// rawUnsafe's, each once, each in the call its first argument names.
// An unsafe operation can also name no unsafe identifier: a go:linkname
// reaches any unexported symbol, and cgo runs code this check does not read.
// So the file has no //go: directive but its leading //go:build, no //export
// and no import "C". Each finding says where and what.
func unsafeUses(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "kernel_linux.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: onlyImports{}, Error: func(error) {}}
	_, _ = conf.Check("netblock", fset, []*ast.File{f}, info) // the errors are the empty imports'
	uses := map[*ast.Ident]bool{}
	for id, obj := range info.Uses {
		if obj != nil && obj.Pkg() == types.Unsafe {
			uses[id] = false
		}
	}
	form := func(e ast.Expr) string {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, e); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	intoSyscall := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		pn, ok := info.Uses[x].(*types.PkgName)
		return ok && pn.Imported().Path() == "syscall"
	}
	var got []unsafeAt
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || !intoSyscall(c.Fun) || len(c.Args) == 0 {
			return true
		}
		call := form(c.Args[0])
		for i, a := range c.Args {
			in := false
			ast.Inspect(a, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if _, ok := uses[id]; ok {
						uses[id], in = true, true
					}
				}
				return true
			})
			if in {
				got = append(got, unsafeAt{call, i, form(a)})
			}
		}
		return true
	})
	want := rawUnsafe()
	out := directives(fset, f)
	for id, held := range uses {
		if !held {
			out = append(out, fmt.Sprintf("line %d: unsafe.%s outside the raw call's arguments", fset.Position(id.Pos()).Line, id.Name))
		}
	}
	for _, u := range got {
		if !slices.Contains(want, u) {
			out = append(out, fmt.Sprintf("the call on %s, argument %d, is %s, which is not one of the forms rawUnsafe holds", u.call, u.arg, u.form))
		}
	}
	for _, w := range want {
		if n := slices.Index(got, w); n < 0 || slices.Contains(got[n+1:], w) {
			out = append(out, fmt.Sprintf("the call on %s, argument %d, as %s is not there once", w.call, w.arg, w.form))
		}
	}
	slices.Sort(out)
	return out
}

// directives is each way into an unsafe operation that names no unsafe
// identifier: a //go: directive but the leading //go:build, an //export, and
// an import of "C".
func directives(fset *token.FileSet, f *ast.File) []string {
	var out []string
	for _, g := range f.Comments {
		for _, c := range g.List {
			line := fset.Position(c.Pos()).Line
			switch {
			case strings.HasPrefix(c.Text, "//go:build ") && c.Pos() < f.Package:
			case strings.HasPrefix(c.Text, "//go:"), strings.HasPrefix(c.Text, "//export "):
				out = append(out, fmt.Sprintf("line %d: the directive %s", line, c.Text))
			}
		}
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"C"` {
			out = append(out, fmt.Sprintf("line %d: import \"C\", whose code this check does not read", fset.Position(im.Pos()).Line))
		}
	}
	return out
}

// TestKernelLinuxUsesUnsafeOnlyInItsRawCalls: the nolint for G103 is on
// the one raw call's line, seccomp's, so this holds the file's unsafe uses to
// that call's arguments, as written today. Each plant is the file with one
// edit, and each must fail.
//
// What it costs when this fails: an unsafe use grows past the raw call with no reviewer seeing it,
// and the filter's memory is no longer the one call's alone.
func TestKernelLinuxUsesUnsafeOnlyInItsRawCalls(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("kernel_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if got := unsafeUses(t, src); len(got) > 0 {
		t.Fatalf("kernel_linux.go: %q", got)
	}
	const prctl = "return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)"
	const closeRange = "unix.CloseRange(uint(fd), math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)"
	const importsEnd = "\t\"golang.org/x/sys/unix\"\n)\n"
	plants := []struct {
		claim string
		edits [][2]string // each old must be in the file once
	}{
		{"a second unsafe.Pointer elsewhere", [][2]string{{prctl, "_ = unsafe.Pointer(nil)\n\t" + prctl}}},
		{"unsafe as a type outside the calls", [][2]string{{"type linuxKernel struct{}", "type linuxKernel struct{ p unsafe.Pointer }"}}},
		{"unsafe under another name too", [][2]string{{"\t\"unsafe\"\n", "\t\"unsafe\"\n\tu \"unsafe\"\n"}, {"type linuxKernel struct{}", "type linuxKernel struct{ p u.Pointer }"}}},
		{"unsafe by a dot import too", [][2]string{{"\t\"unsafe\"\n", "\t\"unsafe\"\n\t. \"unsafe\"\n"}, {"type linuxKernel struct{}", "type linuxKernel struct{ p Pointer }"}}},
		{"another unsafe call in a raw call's argument", [][2]string{{"uintptr(unsafe.Pointer(&fprog))", "uintptr(unsafe.Add(unsafe.Pointer(&fprog), 0))"}}},
		{"a second raw call that passes a pointer", [][2]string{{prctl,
			"_, _, _ = syscall.RawSyscall(unix.SYS_CLOSE_RANGE, uintptr(unsafe.Pointer(&fd)), 0, 0)\n\t" + prctl}}},
		{"a pointer in another call", [][2]string{{closeRange, "unix.CloseRange(uint(fd), math.MaxUint32, uint(uintptr(unsafe.Pointer(&fd))))"}}},
		{"the seccomp pointer passed to another call number", [][2]string{{"syscall.RawSyscall(uintptr(a.seccomp),", "syscall.RawSyscall(unix.SYS_CLOSE_RANGE,"}}},
		{"a linkname, which names no unsafe identifier", [][2]string{{importsEnd,
			importsEnd + "\n//go:linkname zzNanotime runtime.nanotime\nfunc zzNanotime() int64\n"}}},
		{"a linkname above the package clause, where only //go:build is let be", [][2]string{{"//go:build linux\n",
			"//go:linkname zzNanotime runtime.nanotime\n//go:build linux\n"}}},
		{"another directive", [][2]string{{"func (linuxKernel) noNewPrivs() error {", "//go:nosplit\nfunc (linuxKernel) noNewPrivs() error {"}}},
		{"cgo", [][2]string{{"\t\"math\"\n", "\t\"C\"\n\t\"math\"\n"}}},
	}
	for _, p := range plants {
		planted, edited := src, 0
		for _, e := range p.edits {
			if strings.Count(planted, e[0]) != 1 {
				t.Fatalf("%s: the plant's anchor %q is not in kernel_linux.go once", p.claim, e[0])
			}
			planted = strings.Replace(planted, e[0], e[1], 1)
			edited++
		}
		if edited == 0 {
			t.Fatalf("%s: the plant makes no edit", p.claim)
		}
		if got := unsafeUses(t, planted); len(got) == 0 {
			t.Errorf("%s: no finding", p.claim)
		}
	}
	if len(plants) < 12 {
		t.Fatalf("ran %d plants", len(plants))
	}
}
