package proc

import (
	"errors"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEachSystemRefusesALimitItCannotHold: limitsRefused, for each kind of system, refuses a
// negative Processes everywhere, and each field the system does not hold by its name, wrapping
// errors.ErrUnsupported: on a system with no limits, every field; on Linux, Processes; on Linux too
// a TreeMemory past memory.max's signed 64 bits, and on Windows a limit wider than the Job Object's
// field. A Contained with no TreeMemory is refused everywhere, by errContainedAlone. The zero
// Limits, and each field a system holds, pass: TreeMemory, and Contained with it, on Linux, where
// the run then asks whether a cgroup is delegated and mounted nsdelegate (cgroupFor), and
// TreeMemory and Processes on Windows, which refuses Contained by errNoContained, a process of the
// tree able to clear the job's limit. Each refusal wraps ErrRefused.
//
// What it costs when this fails: a limit the system cannot hold is dropped and the run starts
// unbounded, the caller told nothing; or a TreeMemory past 63 bits is written to memory.max as a
// negative count, which the kernel refuses or reads as another limit; or a Contained with nothing
// to contain the tree in runs as though it were held.
func TestEachSystemRefusesALimitItCannotHold(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name        string
		l           Limits
		k           limitKind
		want        error
		unsupported bool
	}{
		{"zero, none", Limits{}, limNone, nil, false},
		{"zero, Linux", Limits{}, limCgroup, nil, false},
		{"zero, Windows", Limits{}, limJob, nil, false},
		{"negative Processes, none", Limits{Processes: -1}, limNone, errProcesses, false},
		{"negative Processes, Linux", Limits{Processes: -1}, limCgroup, errProcesses, false},
		{"negative Processes, Windows", Limits{Processes: -1}, limJob, errProcesses, false},
		{"TreeMemory, none", Limits{TreeMemory: 1 << 20}, limNone, errNoTreeMemory, true},
		{"Processes, none", Limits{Processes: 2}, limNone, errNoProcessLimit, true},
		{"both, none", Limits{TreeMemory: 1 << 20, Processes: 2}, limNone, errNoTreeMemory, true},
		{"TreeMemory, Linux", Limits{TreeMemory: 1 << 20}, limCgroup, nil, false},
		{"TreeMemory at 63 bits, Linux", Limits{TreeMemory: math.MaxInt64}, limCgroup, nil, false},
		{"TreeMemory past 63 bits, Linux", Limits{TreeMemory: math.MaxInt64 + 1}, limCgroup, errTreeMemoryWide, false},
		{"Processes, Linux", Limits{Processes: 2}, limCgroup, errNoProcessLimit, true},
		{"TreeMemory and Processes, Linux", Limits{TreeMemory: 1 << 20, Processes: 2}, limCgroup, errNoProcessLimit, true},
		{"TreeMemory, Windows", Limits{TreeMemory: 1 << 20}, limJob, nil, false},
		{"Processes, Windows", Limits{Processes: 2}, limJob, nil, false},
		{"TreeMemory and Processes, Windows", Limits{TreeMemory: 1 << 20, Processes: 2}, limJob, nil, false},
		{"Contained alone, none", Limits{Contained: true}, limNone, errContainedAlone, false},
		{"Contained alone, Linux", Limits{Contained: true}, limCgroup, errContainedAlone, false},
		{"Contained alone, Windows", Limits{Contained: true}, limJob, errContainedAlone, false},
		{"Contained and Processes, Windows", Limits{Contained: true, Processes: 2}, limJob, errContainedAlone, false},
		{"Contained and TreeMemory, none", Limits{Contained: true, TreeMemory: 1 << 20}, limNone, errNoTreeMemory, true},
		{"Contained and TreeMemory, Linux", Limits{Contained: true, TreeMemory: 1 << 20}, limCgroup, nil, false},
		{"Contained and TreeMemory, Windows", Limits{Contained: true, TreeMemory: 1 << 20}, limJob, errNoContained, true},
	}
	for _, r := range rows {
		err := limitsRefused(r.l, r.k)
		if !errors.Is(err, r.want) || (r.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", r.name, err, r.want)
			continue
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v does not wrap ErrRefused", r.name, err)
		}
		if got := errors.Is(err, errors.ErrUnsupported); got != r.unsupported {
			t.Errorf("%s: %v wraps errors.ErrUnsupported %v, want %v", r.name, err, got, r.unsupported)
		}
	}
	if err := jobRefused(Limits{TreeMemory: 5}, 4); !errors.Is(err, errTreeMemoryWide) {
		t.Errorf("a TreeMemory one past the job's field: %v, want errTreeMemoryWide", err)
	}
	if err := jobRefused(Limits{TreeMemory: 4}, 4); err != nil {
		t.Errorf("a TreeMemory the job's field holds: %v, want nil", err)
	}
	if intBits == 64 {
		if err := limitsRefused(Limits{Processes: pastDWORD()}, limJob); !errors.Is(err, errProcessesWide) {
			t.Errorf("a Processes one past a DWORD, on Windows: %v, want errProcessesWide", err)
		}
		if err := jobRefused(Limits{Processes: pastDWORD()}, 4); !errors.Is(err, errProcessesWide) {
			t.Errorf("a Processes one past a DWORD: %v, want errProcessesWide", err)
		}
		if err := jobRefused(Limits{Processes: pastDWORD() - 1}, 4); err != nil {
			t.Errorf("a Processes a DWORD holds: %v, want nil", err)
		}
	}
}

// intBits is the width of an int here.
const intBits = 32 << (^uint(0) >> 63)

// pastDWORD is one more than a DWORD holds, as an int, read only where intBits is 64.
func pastDWORD() int {
	w := uint64(math.MaxUint32) + 1
	return int(w)
}

// TestRunRefusesALimitThisSystemCannotHoldAndStartsNothing: Run, on this system, refuses each limit
// it cannot hold with the error limitsRefused names, Code -1, and the program, which would make a
// file if run, makes none. A negative Processes is refused everywhere. Linux's refusal of a
// TreeMemory where no cgroup is delegated is TestTreeMemoryWithNoDelegationIsRefusedAndStartsNothing's.
//
// What it costs when this fails: a run asked to be bounded starts unbounded on a system that cannot
// bound it, and the caller learns nothing.
func TestRunRefusesALimitThisSystemCannotHoldAndStartsNothing(t *testing.T) {
	t.Parallel()
	type row struct {
		name string
		l    Limits
		want error
	}
	rows := []row{{"a negative Processes", Limits{Processes: -1}, errProcesses}}
	switch limitsHere {
	case limNone:
		rows = append(rows, row{"TreeMemory", Limits{TreeMemory: 1 << 30}, errNoTreeMemory}, row{"Processes", Limits{Processes: 2}, errNoProcessLimit})
	case limCgroup:
		rows = append(rows, row{"Processes", Limits{Processes: 2}, errNoProcessLimit}, row{"TreeMemory past 63 bits", Limits{TreeMemory: math.MaxInt64 + 1}, errTreeMemoryWide})
	case limJob:
		if intBits == 64 {
			rows = append(rows, row{"Processes wider than a DWORD", Limits{Processes: pastDWORD()}, errProcessesWide})
		}
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Limits = r.l
		res, err := Run(t.Context(), c)
		if !errors.Is(err, r.want) || !errors.Is(err, ErrRefused) || res.Code != -1 {
			t.Errorf("%s: %v, Code %d, want %v and Code -1", r.name, err, res.Code, r.want)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the program ran (%v), want nothing started", r.name, err)
		}
	}
}

// TestZeroLimitsChangeNothing: a run with the zero Limits starts the program as it is, by its own
// path and arguments, with nothing put before them.
//
// What it costs when this fails: every run, held to no limit, is started through something else, or
// is refused where no limit was asked.
func TestZeroLimitsChangeNothing(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "argv")
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Fatalf("a run with no limit: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	got := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	if want := slices.Concat([]string{c.Path}, c.Args); !slices.Equal(got, want) {
		t.Errorf("a run with no limit started %q, want %q", got, want)
	}
}

// TestNoProgramThatImportsProcLinksTesting: no file of the package but its tests, for any system,
// imports testing, and none sets Cmd.seam, procFS.standIn or procFS.noOpenat2: each is read in seams
// or procOwn alone, and set only by a test. A Cmd or a procFS is given in a non-test file only by
// keys, and never as the element of an array, slice or map, whose literal may give one by position
// with its type left out, a generic one instantiated with it included. Neither is reached by another
// name, an alias, a defined type, a type parameter's constraint or a type argument, nor made by a
// conversion, and nothing else has a field or parameter named seam, standIn or noOpenat2, so none
// has their shape, which a conversion or an assignment would bring in. Only their own
// declarations at package level are left unread; one of either name in a function is read as any
// other.
//
// It reads syntax, so it does not see unsafe: a write through an unsafe.Pointer to a Cmd or a
// procFS sets either, and the package imports unsafe in its Windows files, so the import is not
// refused. That is the guard's limit, and a review reads for it.
//
// What it costs when this fails: every program that imports proc links testing, or a program's
// own runs read a test's stand-in for this system's cgroups, as a test does, or take a folder for
// /proc, and Stop kills by a planted stat.
func TestNoProgramThatImportsProcLinksTesting(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the package's files: %v, %v", files, err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"testing"` {
				t.Errorf("%s imports testing", name)
			}
		}
		for _, decl := range f.Decls {
			in := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				in = fn.Name.Name
			}
			own := seamTypesDeclared(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if own[n] {
					return false // the type's own declaration
				}
				if said := seamBreach(n, in); said != "" {
					t.Errorf("%s %s", name, said)
				}
				return true
			})
		}
	}
}

// seamField is the field by which the type of that name holds a seam, Cmd's seam or procFS's
// standIn, or "" for any other.
func seamField(typ string) string {
	switch typ {
	case "Cmd":
		return "seam"
	case "procFS":
		return "standIn"
	}
	return ""
}

// seamTypesDeclared is the type specs of a package-level declaration that declare Cmd or procFS
// themselves, as a struct: the only specs the guard does not read. A spec of either name inside a
// function, or as an alias or a defined type of another, shadows it, and is read.
func seamTypesDeclared(decl ast.Decl) map[ast.Node]bool {
	own := map[ast.Node]bool{}
	if gd, ok := decl.(*ast.GenDecl); ok {
		for _, s := range gd.Specs {
			if ts, ok := s.(*ast.TypeSpec); ok && seamField(ts.Name.Name) != "" {
				_, isStruct := ts.Type.(*ast.StructType)
				own[ts] = isStruct && ts.Assign == 0 && ts.TypeParams == nil
			}
		}
	}
	return own
}

// seamBreach is what node n of a non-test file does that may set Cmd.seam or procFS.standIn, as
// the end of a sentence about its file, or "". in is the name of the function n is in, or "".
func seamBreach(n ast.Node, in string) string {
	if said := seamTypeBreach(n); said != "" {
		return said
	}
	return seamValueBreach(n, in)
}

// seamTypeBreach is how a type expression or declaration n reaches a Cmd or a procFS by a way
// whose literal the guard would not read, or "".
func seamTypeBreach(n ast.Node) string {
	switch n := n.(type) {
	case *ast.TypeSpec:
		if namesSeamType(n.Type) || n.TypeParams != nil && namesSeamType(n.TypeParams) {
			return "declares " + n.Name.Name + " by a Cmd or a procFS, another name for one, whose literal the guard would not read"
		}
	case *ast.FuncType:
		if n.TypeParams != nil && namesSeamType(n.TypeParams) {
			return "constrains a type parameter by a Cmd or a procFS, whose literal may then give one by position"
		}
	case *ast.Field:
		for _, id := range n.Names {
			if id.Name == "seam" || id.Name == "standIn" || id.Name == "noOpenat2" {
				return "has a field " + id.Name + " outside Cmd and procFS, the shape a conversion or an assignment would bring in"
			}
		}
	case *ast.ArrayType:
		if isSeamType(n.Elt) {
			return "holds a Cmd or a procFS in an array or slice, whose literal may give one by position with its type elided"
		}
	case *ast.MapType:
		if isSeamType(n.Key) || isSeamType(n.Value) {
			return "holds a Cmd or a procFS in a map, whose literal may give one by position with its type elided"
		}
	case *ast.IndexExpr:
		if namesSeamType(n.Index) {
			return "instantiates a generic type or function with a Cmd or a procFS, whose literal may give one by position with its type elided"
		}
	case *ast.IndexListExpr:
		for _, e := range n.Indices {
			if namesSeamType(e) {
				return "instantiates a generic type or function with a Cmd or a procFS, whose literal may give one by position with its type elided"
			}
		}
	}
	return ""
}

// seamValueBreach is how an expression n, in the function named in, may set Cmd.seam or
// procFS.standIn, or "".
func seamValueBreach(n ast.Node, in string) string {
	switch n := n.(type) {
	case *ast.CallExpr:
		fun := ast.Unparen(n.Fun)
		if s, ok := fun.(*ast.StarExpr); ok {
			fun = ast.Unparen(s.X)
		}
		if id, ok := fun.(*ast.Ident); ok && seamField(id.Name) != "" && len(n.Args) == 1 {
			return "converts to " + id.Name + ", which may set " + seamField(id.Name)
		}
	case *ast.KeyValueExpr:
		if k, ok := n.Key.(*ast.Ident); ok && (k.Name == "seam" || k.Name == "standIn" || k.Name == "noOpenat2") {
			return "sets " + k.Name + " in a literal"
		}
	case *ast.CompositeLit:
		id, ok := n.Type.(*ast.Ident)
		if !ok || seamField(id.Name) == "" {
			break
		}
		for _, e := range n.Elts {
			if _, keyed := e.(*ast.KeyValueExpr); !keyed {
				return "gives a " + id.Name + " by position, which may set " + seamField(id.Name)
			}
		}
	case *ast.SelectorExpr:
		if n.Sel.Name == "seam" && in != "seams" {
			return "reaches seam outside seams"
		}
		if (n.Sel.Name == "standIn" || n.Sel.Name == "noOpenat2") && in != "procOwn" {
			return "reaches " + n.Sel.Name + " outside procOwn"
		}
	}
	return ""
}

// isSeamType is whether a type expression is a Cmd or a procFS or a pointer to one, the element
// types whose literals inside an array, slice or map literal may leave the type out.
func isSeamType(e ast.Expr) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	id, ok := e.(*ast.Ident)
	return ok && seamField(id.Name) != ""
}

// namesSeamType is whether a type expression or a type parameter list names a Cmd or a procFS
// anywhere in it, as the package's own identifier and not as the last part of a qualified one,
// such as exec.Cmd.
func namesSeamType(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			return false
		case *ast.Ident:
			found = found || seamField(n.Name) != ""
		}
		return !found
	})
	return found
}

// TestEverySignalOnUnixGoesBySignalPid: in every file of the package that builds on a Unix, its
// tests among them, a signal is sent only inside signalPid, which refuses a pid below 2 by its size:
// no other function calls a Kill, Killpg, Tkill, Tgkill or PidfdSendSignal, a Signal of one argument,
// as os.Process's is, or a function named signal, the name the package gives a killer. A file that
// builds on Windows alone is left out: Windows ends a tree by its job's handle, not by a pid.
//
// What it costs when this fails: a pid of 0, 1 or -1 reaches the kill by a path the guard does not
// see, and a kill(-1) ends every process the caller may signal.
func TestEverySignalOnUnixGoesBySignalPid(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the package's files: %v, %v", files, err)
	}
	sends := map[string]bool{"Kill": true, "Killpg": true, "Tkill": true, "Tgkill": true, "PidfdSendSignal": true, "signal": true}
	checked := 0
	for _, name := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if !buildsOnUnix(t, name, f) {
			continue
		}
		checked++
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && name == "proc_unix.go" && fn.Recv == nil && fn.Name.Name == "signalPid" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var fun string
				switch f := call.Fun.(type) {
				case *ast.Ident:
					fun = f.Name
				case *ast.SelectorExpr:
					fun = f.Sel.Name
				}
				if sends[fun] || fun == "Signal" && len(call.Args) == 1 {
					t.Errorf("%s: a signal sent by %s outside signalPid", fset.Position(call.Pos()), fun)
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no file of the package builds on a Unix")
	}
}

// buildsOnUnix is whether the file name, parsed as f, builds on Linux, macOS or FreeBSD, by its name's
// system and its go:build line.
func buildsOnUnix(t *testing.T, name string, f *ast.File) bool {
	t.Helper()
	if strings.HasSuffix(strings.TrimSuffix(name, "_test.go"), "_windows") || strings.HasSuffix(name, "_windows.go") {
		return false
	}
	for _, group := range f.Comments {
		if group.Pos() > f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, goos := range []string{"linux", "darwin", "freebsd"} {
				if expr.Eval(func(tag string) bool { return tag == goos || tag == "unix" }) {
					return true
				}
			}
			return false
		}
	}
	return true
}
