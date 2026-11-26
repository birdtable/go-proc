package execsites

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// root is the module's root, two folders up from this package's.
const root = "../.."

// execPackages is each package whose Exec replaces the process's image, by its import path, with
// the name a file gives it when it gives none.
var execPackages = map[string]string{"syscall": "syscall", "golang.org/x/sys/unix": "unix"} //nolint:gochecknoglobals // a constant table the rows read

// want is each exec the module has: the shim's of its second stage and of the program, and the
// sandbox block's of the program on Linux and on macOS (by posix_spawn, with syscall.Exec handed in).
func want() []string {
	return []string{
		"proc/shim_linux.go shimSteps syscall.Exec",
		"proc/shim_linux.go shimStart syscall.Exec",
		"sandbox/kernel_darwin.go (darwinKernel).exec syscall.Exec",
		"sandbox/kernel_linux.go (linuxKernel).exec syscall.Exec",
	}
}

// sites is each reference to an Exec of execPackages in the non-test Go files beneath top, as
// "<file> <enclosing function> <package>.Exec", sorted: a call, or the function handed on as a value,
// once for each time it is named. A reference outside every function is "(top level)", and a file
// that imports one of the packages by "." is listed by that import, as its Exec cannot be told from
// another name. Every file is read, whatever system or tag it builds for; testdata, dot folders and
// a folder that is a module of its own are not the module's.
func sites(top string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == top {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(top, p)
		if err != nil {
			return err
		}
		found, err := fileSites(p, filepath.ToSlash(rel))
		out = append(out, found...)
		return err
	})
	slices.Sort(out)
	return out, err
}

// fileSites is sites for the one file at path, named rel.
func fileSites(path, rel string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution|parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	names := map[string]string{} // the name the file gives a package of execPackages, to that name as sites says it
	var out []string
	for _, spec := range f.Imports {
		imp, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		name, ok := execPackages[imp]
		if !ok {
			continue
		}
		local := name
		if spec.Name != nil {
			local = spec.Name.Name
		}
		switch local {
		case "_":
		case ".":
			out = append(out, rel+" (import) . "+imp)
		default:
			names[local] = name
		}
	}
	if len(names) == 0 {
		return out, nil
	}
	f, err = parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	for _, d := range f.Decls {
		where := "(top level)"
		if fn, ok := d.(*ast.FuncDecl); ok {
			where = funcName(fn)
		}
		ast.Inspect(d, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Exec" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && names[x.Name] != "" {
				out = append(out, rel+" "+where+" "+names[x.Name]+".Exec")
			}
			return true
		})
	}
	return out, nil
}

// funcName is fn's name, with its receiver's type for a method: (T).m or (*T).m.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if idx, ok := recv.(*ast.IndexExpr); ok { // a generic receiver, T[P]
		recv = idx.X
	}
	star := ""
	if p, ok := recv.(*ast.StarExpr); ok {
		star, recv = "*", p.X
	}
	if idx, ok := recv.(*ast.IndexExpr); ok {
		recv = idx.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return "(" + star + id.Name + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

// extra is each site of got that want does not hold, and missing each of want that got does not,
// each counted: a function that names Exec twice where want holds it once has one extra.
func extra(got, want []string) (more, missing []string) {
	left := slices.Clone(want)
	for _, g := range got {
		if i := slices.Index(left, g); i >= 0 {
			left = slices.Delete(left, i, i+1)
			continue
		}
		more = append(more, g)
	}
	return more, left
}

// TestTheModuleExecsOnlyAtTheShimsTwoAndTheSandboxBlocksTwo: the module's non-test source names
// syscall.Exec or x/sys/unix's Exec in four places, each once: the shim's exec of its second stage
// (shimSteps) and of the program (shimStart), and the sandbox block's exec of the program on Linux
// and on macOS.
//
// What it costs when this fails: a new exec in the library replaces a process's image outside the
// four whose environment, descriptors and privileges the shim and the block hold, and lint passes it
// once its line carries a //nolint.
func TestTheModuleExecsOnlyAtTheShimsTwoAndTheSandboxBlocksTwo(t *testing.T) {
	t.Parallel()
	got, err := sites(root)
	if err != nil {
		t.Fatal(err)
	}
	more, missing := extra(got, want())
	for _, s := range more {
		t.Errorf("an exec that is none of the four: %s; route it through the shim or the block, or name it in want with why", s)
	}
	for _, s := range missing {
		t.Errorf("an exec want names is not in the source: %s; take it out of want with the code", s)
	}
}

// TestAPlantedExecIsListedByItsFileAndFunction: in a scratch copy of the four files that exec, an
// exec planted in a non-test file is listed by its file and function, and is the only site want
// does not hold, however it is reached: a call that carries //nolint:forbidigo, x/sys/unix's Exec by
// another name, the function handed on as a value in a method, a reference at the top level, a
// second reference in a function want already names, and a "." import. One planted in a test file,
// in testdata, in a dot folder or in a module of its own is not listed, nor is a blank import.
//
// What it costs when this fails: the row over the real tree passes a new exec that is spelled
// another way, and the library replaces a process's image where nothing holds what it is given.
func TestAPlantedExecIsListedByItsFileAndFunction(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	for _, s := range want() {
		file := strings.Fields(s)[0]
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		plant(t, top, file, string(b))
	}
	plant(t, top, "proc/start_linux.go", "package proc\n\nimport \"syscall\"\n\nfunc startAgain() error {\n\treturn syscall.Exec(\"/bin/sh\", nil, nil) //nolint:forbidigo // planted\n}\n")
	plant(t, top, "sandbox/again.go", "package sandbox\n\nimport sys \"golang.org/x/sys/unix\"\n\ntype k struct{}\n\nfunc (*k) run(f func(string, []string, []string) error) error { return f(\"\", nil, nil) }\n\nfunc (p *k) again() error { return p.run(sys.Exec) }\n")
	plant(t, top, "service/top.go", "package service\n\nimport \"syscall\"\n\nvar execAgain = syscall.Exec\n")
	plant(t, top, "proc/dot_unix.go", "package proc\n\nimport . \"syscall\"\n\nvar _ = Getpid\n")
	plant(t, top, "proc/blank.go", "package proc\n\nimport _ \"syscall\"\n")
	plant(t, top, "proc/start_test.go", "package proc\n\nimport \"syscall\"\n\nvar testExec = syscall.Exec\n")
	plant(t, top, "proc/testdata/x.go", "package x\n\nimport \"syscall\"\n\nvar e = syscall.Exec\n")
	plant(t, top, "proc/.hidden/x.go", "package x\n\nimport \"syscall\"\n\nvar e = syscall.Exec\n")
	plant(t, top, "tools/gate/go.mod", "module gate\n")
	plant(t, top, "tools/gate/x.go", "package gate\n\nimport \"syscall\"\n\nvar e = syscall.Exec\n")
	// a second reference to syscall.Exec in shimStart, which want names once
	shim := filepath.Join(top, "proc", "shim_linux.go")
	b, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	twice := strings.Replace(string(b), "func shimStart(fd int, path string, argv []string) error {\n", "func shimStart(fd int, path string, argv []string) error {\n\t_ = syscall.Exec\n", 1)
	if twice == string(b) {
		t.Fatal("proc/shim_linux.go has no shimStart of the signature the row plants into; plant into the one it has")
	}
	plant(t, top, "proc/shim_linux.go", twice)
	got, err := sites(top)
	if err != nil {
		t.Fatal(err)
	}
	more, missing := extra(got, want())
	planted := []string{
		"proc/dot_unix.go (import) . syscall",
		"proc/shim_linux.go shimStart syscall.Exec",
		"proc/start_linux.go startAgain syscall.Exec",
		"sandbox/again.go (*k).again unix.Exec",
		"service/top.go (top level) syscall.Exec",
	}
	if !slices.Equal(more, planted) || len(missing) > 0 {
		t.Errorf("the sites want does not hold: %q, and those it holds that are missing: %q; want %q and none missing", more, missing, planted)
	}
}

// plant writes text to the file rel beneath top, making its folders.
func plant(t *testing.T, top, rel, text string) {
	t.Helper()
	p := filepath.Join(top, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
