package execsites

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	pathpkg "path"
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
func execPackages() map[string]string {
	return map[string]string{"syscall": "syscall", "golang.org/x/sys/unix": "unix"}
}

// execFuncs is each exec of the module's own, by the folder of the package that declares it: the
// sandbox's spawnExec, the macOS block's posix_spawn with POSIX_SPAWN_SETEXEC, which execs whatever
// program it is given. Being unexported, it is named only in its own package.
func execFuncs() map[string]string {
	return map[string]string{"sandbox": "spawnExec"}
}

// heldNames is each name of the module's own that reaches an exec without the exec of execFuncs,
// by the folder of the package that declares it, to the one file that may name it: the sandbox's
// spawnAddr, the address of the trampoline to libSystem's posix_spawn, which rawSyscall6 calls from
// any file of the package as spawnExec does. A reference in any other file is a site, and want holds
// none.
func heldNames() map[string]map[string]string {
	return map[string]map[string]string{"sandbox": {"spawnAddr": "sandbox/spawn_darwin.go"}}
}

// want is each exec the module has: the shim's of its second stage and of the program, and the
// sandbox block's of the program on Linux and on macOS, where spawnExec execs it and syscall.Exec is
// handed in, for no path, to put the open-file limit back.
func want() []string {
	return []string{
		"proc/shim_linux.go shimSteps syscall.Exec",
		"proc/shim_linux.go shimStart syscall.Exec",
		"sandbox/kernel_darwin.go (darwinKernel).exec spawnExec",
		"sandbox/kernel_darwin.go (darwinKernel).exec syscall.Exec",
		"sandbox/kernel_linux.go (linuxKernel).exec syscall.Exec",
	}
}

// sites is each reference to an Exec of execPackages, to an exec of execFuncs outside its own
// declaration, or to a name of heldNames outside its one file, in the module's non-test Go files
// beneath top, as goFiles lists them, as "<file> <enclosing function> <name>", sorted: a call, or
// the function handed on as a value, once for each time it is named. A reference outside every
// function is "(top level)", and a file that imports one of the packages by "." is listed by that
// import, as its Exec cannot be told from another name. A raw execve, Syscall(SYS_EXECVE, ...), is
// not listed: the raw-syscall lint row keeps it to a file built for one system, and review holds the
// rest.
func sites(top string) ([]string, error) {
	files, err := goFiles(top, false)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rel := range files {
		found, err := fileSites(filepath.Join(top, filepath.FromSlash(rel)), rel)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	slices.Sort(out)
	return out, nil
}

// goFiles is each of the module's Go files beneath top, by its path from top with forward slashes,
// its test files only when tests is true. Every file is listed, whatever system or tag it builds for;
// testdata, dot folders and a folder that is a module of its own are not the module's.
func goFiles(top string, tests bool) ([]string, error) {
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
		if !strings.HasSuffix(p, ".go") || (!tests && strings.HasSuffix(p, "_test.go")) {
			return nil
		}
		rel, err := filepath.Rel(top, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	return out, err
}

// trampolineFile is the one file of the module that may call a libc function itself: the macOS
// block's, sandbox/spawn_darwin.go, whose spawnExec is the block's exec.
const trampolineFile = "sandbox/spawn_darwin.go"

// trampolineFuncs is each function trampolineFile links from syscall to call a libc function at an
// address: syscall6, and rawSyscall6, which makes the block's posix_spawn.
func trampolineFuncs() []string {
	return []string{"rawSyscall6", "syscall6"}
}

// trampolineDirectives is each directive that gives a file a function it could not otherwise call:
// //go:linkname, which links a name to another package's unexported function, and
// //go:cgo_import_dynamic, which imports a libc symbol for an assembly trampoline to jump to. Every
// //go:linkname is held, whatever it names, as the functions that reach libc or the kernel by a link
// are no closed list. The compiler refuses the other //go:cgo_ directives outside cgo's own files.
func trampolineDirectives() []string {
	return []string{"//go:linkname", "//go:cgo_import_dynamic"}
}

// trampolines is each reference to a function of trampolineFuncs, and each directive of
// trampolineDirectives, in the module's Go files beneath top outside trampolineFile, test files
// included, as "<file>:<line> <name or directive>", sorted by file and line: a call, or the function
// handed on as a value. A field or method of that name is not one, nor is a directive's text inside a
// string or after other text in a comment. A fixture that needs a directive lives in testdata.
func trampolines(top string) ([]string, error) {
	files, err := goFiles(top, true)
	if err != nil {
		return nil, err
	}
	type found struct {
		file string
		line int
		what string
	}
	var all []found
	for _, rel := range files {
		if rel == trampolineFile {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(top, filepath.FromSlash(rel)), nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, g := range f.Comments {
			for _, c := range g.List {
				if fields := strings.Fields(c.Text); len(fields) > 0 && slices.Contains(trampolineDirectives(), fields[0]) {
					all = append(all, found{rel, fset.Position(c.Slash).Line, fields[0]})
				}
			}
		}
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr: // a field or method of the name is not the function
				ast.Inspect(n.X, visit)
				return false
			case *ast.StructType: // nor is a struct's field of that name; its type is read
				for _, fl := range n.Fields.List {
					ast.Inspect(fl.Type, visit)
				}
				return false
			case *ast.Ident:
				if slices.Contains(trampolineFuncs(), n.Name) {
					all = append(all, found{rel, fset.Position(n.Pos()).Line, n.Name})
				}
			}
			return true
		}
		ast.Inspect(f, visit)
	}
	slices.SortStableFunc(all, func(a, b found) int {
		return cmp.Or(strings.Compare(a.file, b.file), cmp.Compare(a.line, b.line))
	})
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, s.file+":"+strconv.Itoa(s.line)+" "+s.what)
	}
	return out, nil
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
		name, ok := execPackages()[imp]
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
	own := execFuncs()[pathpkg.Dir(rel)] // the exec of the module's own this file's package declares, if any
	var held []string                    // the names of heldNames this file's package declares and this file may not name
	for name, file := range heldNames()[pathpkg.Dir(rel)] {
		if file != rel {
			held = append(held, name)
		}
	}
	if len(names) == 0 && own == "" && len(held) == 0 {
		return out, nil
	}
	f, err = parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	for _, d := range f.Decls {
		where, decl := "(top level)", (*ast.Ident)(nil)
		if fn, ok := d.(*ast.FuncDecl); ok {
			where, decl = funcName(fn), fn.Name
		}
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && n.Sel.Name == "Exec" && names[x.Name] != "" {
					out = append(out, rel+" "+where+" "+names[x.Name]+".Exec")
					return false
				}
				ast.Inspect(n.X, visit) // a field or method of the name of an exec is not that exec
				return false
			case *ast.StructType: // nor is a struct's field of that name; its type is read
				for _, fl := range n.Fields.List {
					ast.Inspect(fl.Type, visit)
				}
				return false
			case *ast.Ident:
				if own != "" && n.Name == own && n != decl {
					out = append(out, rel+" "+where+" "+own)
				}
				if slices.Contains(held, n.Name) {
					out = append(out, rel+" "+where+" "+n.Name)
				}
			}
			return true
		}
		ast.Inspect(d, visit)
	}
	return out, nil
}

// funcName is fn's name, with its receiver's type for a method: (T).m or (*T).m, a generic type's
// parameters left out.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv, star := fn.Recv.List[0].Type, ""
	if p, ok := recv.(*ast.StarExpr); ok {
		star, recv = "*", p.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr: // T[P]
		recv = r.X
	case *ast.IndexListExpr: // T[P, Q]
		recv = r.X
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
// syscall.Exec, x/sys/unix's Exec or the sandbox's spawnExec only at its four execs, each once: the
// shim's of its second stage (shimSteps) and of the program (shimStart), and the sandbox block's of
// the program on Linux, and on macOS, where it names both spawnExec and syscall.Exec. It names the
// sandbox's spawnAddr, posix_spawn's trampoline, only in sandbox/spawn_darwin.go, where spawnExec
// calls it. A raw execve is not this row's: the raw-syscall lint row keeps it to a file built for
// one system.
//
// What it costs when this fails: a new exec in the library replaces a process's image outside the
// four whose environment, descriptors and privileges the shim and the block hold, and lint passes it
// once its line carries a //nolint, or, through spawnExec or rawSyscall6 with spawnAddr, with none.
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
// second reference in a function want already names, a "." import, spawnExec called in another
// file of the sandbox, and posix_spawn called through rawSyscall6 with spawnAddr in another darwin
// file of the sandbox. One planted in a test file, in testdata, in a dot folder or in a module of
// its own is not listed, nor is a blank import, spawnExec's own declaration, the references to
// spawnAddr in sandbox/spawn_darwin.go, a field of either's name, or a name spawnExec or spawnAddr
// in a package other than the sandbox's.
//
// What it costs when this fails: the row over the real tree passes a new exec that is spelled
// another way, and the library replaces a process's image where nothing holds what it is given.
func TestAPlantedExecIsListedByItsFileAndFunction(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	for _, s := range append(want(), "sandbox/spawn_darwin.go") {
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
	plant(t, top, "sandbox/again_darwin.go", "package sandbox\n\nimport \"syscall\"\n\nfunc execAgain(prog string, argv, env []string) error {\n\treturn spawnExec(func(string, []string, []string) error { return syscall.ENOENT }, prog, argv, env)\n}\n")
	plant(t, top, "sandbox/field.go", "package sandbox\n\ntype held struct{ spawnExec bool }\n\nfunc (h held) spawning() bool { return h.spawnExec }\n")
	plant(t, top, "proc/spawn.go", "package proc\n\nfunc spawnExec() {}\n\nfunc spawnAgain() { spawnExec() }\n")
	plant(t, top, "sandbox/addr_darwin.go", "//go:build darwin\n\npackage sandbox\n\nfunc spawnAgain(path, argv, env uintptr) uintptr {\n\tbeforeExec()\n\tr, _, _ := rawSyscall6(spawnAddr, 0, path, 0, 0, argv, env)\n\tafterExec()\n\treturn r\n}\n")
	plant(t, top, "sandbox/addrfield.go", "package sandbox\n\ntype addrs struct{ spawnAddr uintptr }\n\nfunc (a addrs) spawn() uintptr { return a.spawnAddr }\n")
	plant(t, top, "proc/addr.go", "package proc\n\nvar spawnAddr uintptr\n\nfunc addrAgain() uintptr { return spawnAddr }\n")
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
		"sandbox/addr_darwin.go spawnAgain spawnAddr",
		"sandbox/again.go (*k).again unix.Exec",
		"sandbox/again_darwin.go execAgain spawnExec",
		"service/top.go (top level) syscall.Exec",
	}
	if !slices.Equal(more, planted) || len(missing) > 0 {
		t.Errorf("the sites want does not hold: %q, and those it holds that are missing: %q; want %q and none missing", more, missing, planted)
	}
}

// TestOnlyTheBlocksSpawnCallsLibcItself: no Go file of the module but sandbox/spawn_darwin.go, test
// files included, names syscall6 or rawSyscall6 or carries a //go:linkname or //go:cgo_import_dynamic
// directive. Those are how that file calls libSystem's posix_spawn with no cgo, and how any other
// file could make an exec of its own that names neither Exec nor spawnExec.
//
// What it costs when this fails: a file imports posix_spawn or execve and calls it through a
// trampoline of its own, an exec the other rows do not list and lint does not ban, and only a
// reading of the source would find it.
func TestOnlyTheBlocksSpawnCallsLibcItself(t *testing.T) {
	t.Parallel()
	got, err := trampolines(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		t.Errorf("a libc call or link outside %s: %s; make the call there, or hold the new file here with why", trampolineFile, s)
	}
}

// TestAPlantedTrampolineIsListedByItsFileAndLine: in a scratch tree that holds sandbox/spawn_darwin.go,
// a second darwin file of the sandbox that imports posix_spawn by //go:cgo_import_dynamic and calls
// syscall6 is listed at both lines; so are a test file that links syscall.rawSyscall6 and hands
// rawSyscall6 on as a value, and a //go:linkname to the runtime in another package. Not listed:
// spawn_darwin.go's own, a plant in testdata, in a dot folder or in a module of its own, a directive's
// text in a string or after other text in a comment, and a field of syscall6's name.
//
// What it costs when this fails: the row over the real tree passes a trampoline spelled another way,
// and a new file reaches posix_spawn with nothing but review to hold it.
func TestAPlantedTrampolineIsListedByItsFileAndLine(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(trampolineFile)))
	if err != nil {
		t.Fatal(err)
	}
	plant(t, top, trampolineFile, string(b))
	again := "//go:build darwin\n\npackage sandbox\n\n" +
		"//go:cgo_import_dynamic libc_posix_spawn_again posix_spawn \"/usr/lib/libSystem.B.dylib\"\n\n" +
		"var spawnAgainAddr uintptr\n\n" +
		"func spawnAgain() uintptr {\n\tr, _, _ := syscall6(spawnAgainAddr, 0, 0, 0, 0, 0, 0)\n\treturn r\n}\n"
	plant(t, top, "sandbox/again_darwin.go", again)
	plant(t, top, "sandbox/link_test.go", "package sandbox\n\nimport _ \"unsafe\"\n\n"+
		"//go:linkname zzRaw syscall.rawSyscall6\nfunc zzRaw(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2, err uintptr)\n\n"+
		"var zzSpawn = rawSyscall6\n")
	plant(t, top, "proc/clock.go", "package proc\n\nimport _ \"unsafe\"\n\n//go:linkname nanotime runtime.nanotime\nfunc nanotime() int64\n")
	plant(t, top, "sandbox/testdata/x.go", again)
	plant(t, top, "sandbox/.hidden/x.go", again)
	plant(t, top, "tools/gate/go.mod", "module gate\n")
	plant(t, top, "tools/gate/x.go", again)
	plant(t, top, "proc/text.go", "package proc\n\n// The block links syscall6 by a //go:linkname directive.\n"+
		"var fixture = \"//go:linkname zz syscall.syscall6\"\n\n"+
		"/*\n//go:cgo_import_dynamic in a block comment is no directive\n*/\n\n"+
		"type held struct{ syscall6 bool }\n\nfunc (h held) on() bool { return h.syscall6 }\n")
	got, err := trampolines(top)
	if err != nil {
		t.Fatal(err)
	}
	planted := []string{
		"proc/clock.go:5 //go:linkname",
		"sandbox/again_darwin.go:5 //go:cgo_import_dynamic",
		"sandbox/again_darwin.go:10 syscall6",
		"sandbox/link_test.go:5 //go:linkname",
		"sandbox/link_test.go:8 rawSyscall6",
	}
	if !slices.Equal(got, planted) {
		t.Errorf("the trampolines listed: %q; want %q", got, planted)
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
