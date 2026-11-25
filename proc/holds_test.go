package proc

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestErrNotHeldIsTheClassOfRefusalsThisSystemCannotHold: ErrNotHeld is matched by every refusal made
// to wrap ErrRefused and errors.ErrUnsupported, and by no other; one that carries an
// errno, ENOSYS say, may match errors.ErrUnsupported without it, which this row does not see. The row is the set:
// errNoTreeMemory, errNoProcessLimit, errNoContained, errNoGrace, errNoFileRun and errFileNoProc, each still ErrRefused and errors.ErrUnsupported, its text unchanged; errInRun, Linux's,
// is in its own row. A refusal of the caller's own Cmd, and errNoTree, which is no refusal, are not
// ErrNotHeld. The source holds it: in the package's files, for every system, each fmt.Errorf that wraps
// ErrRefused and errors.ErrUnsupported is a notHeld, and a notHeld holds nothing else, so a new refusal
// of the class matches ErrNotHeld or fails here.
//
// What it costs when this fails: a caller that asks whether this system can hold a run before it runs
// reads a refusal of its own Cmd as the system's, or one of the system's as its own, and matches text.
func TestErrNotHeldIsTheClassOfRefusalsThisSystemCannotHold(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"errNoTreeMemory": errNoTreeMemory, "errNoProcessLimit": errNoProcessLimit, "errNoContained": errNoContained,
		"errNoGrace": errNoGrace, "errNoFileRun": errNoFileRun, "errFileNoProc": errFileNoProc,
	} {
		if !errors.Is(err, ErrNotHeld) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrNotHeld, ErrRefused and errors.ErrUnsupported", name, err)
		}
		if strings.Contains(err.Error(), ErrNotHeld.Error()) {
			t.Errorf("%s: %q, want its own text, without ErrNotHeld's", name, err)
		}
	}
	for name, err := range map[string]error{
		"errTimeout": errTimeout, "errGrace": errGrace, "errEmptyPath": errEmptyPath, "errTreeMemoryWide": errTreeMemoryWide,
		"errContainedTerminal": errContainedTerminal, "errContainedDir": errContainedDir, "ErrBatch": ErrBatch, "errNoTree": errNoTree,
		"errNUL": errNUL,
	} {
		if errors.Is(err, ErrNotHeld) {
			t.Errorf("%s: %v, want no ErrNotHeld: it is no refusal of what this system cannot hold", name, err)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the package's files: %v, %v", files, err)
	}
	fset := token.NewFileSet()
	inClass := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		held := map[ast.Expr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok && isIdent(lit.Type, "notHeld") {
				if len(lit.Elts) != 1 || !refusalOfClass(lit.Elts[0]) {
					t.Errorf("%s: a notHeld that holds other than one fmt.Errorf wrapping ErrRefused and errors.ErrUnsupported", fset.Position(lit.Pos()))
				}
				for _, e := range lit.Elts {
					held[e] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && refusalOfClass(call) {
				inClass++
				if !held[call] {
					t.Errorf("%s: a refusal that wraps ErrRefused and errors.ErrUnsupported is no notHeld, so it does not match ErrNotHeld", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	if inClass < 6 {
		t.Errorf("found %d refusals of the class in the package's files, want at least the 6 of the row", inClass)
	}
}

// refusalOfClass is whether e is a call of fmt.Errorf with ErrRefused (or errRefused) and
// errors.ErrUnsupported among its arguments.
func refusalOfClass(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || !isSelector(call.Fun, "fmt", "Errorf") {
		return false
	}
	var refused, unsupported bool
	for _, a := range call.Args {
		refused = refused || isIdent(a, "ErrRefused") || isIdent(a, "errRefused")
		unsupported = unsupported || isSelector(a, "errors", "ErrUnsupported")
	}
	return refused && unsupported
}

// isIdent is whether e is the bare name name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isSelector is whether e is pkg.name.
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, pkg) && sel.Sel.Name == name
}

// TestHoldsAgreesWithRunOnThisSystem: for each Cmd, Holds gives the refusal Run then gives, or none
// where Run runs, and starts nothing itself: a plain run, one that reads its peak, one with a Grace,
// one held to Processes, one held to TreeMemory that reads its peak, and one with no Timeout. Where
// Run runs and reads a peak, its kind is the one Holds gave, and where Holds gave PeakNone, Run reads
// none. A Grace is refused with errNoGrace and ErrNotHeld where a run without Terminal is sent no signal it can catch (Windows), a
// Processes with errNoProcessLimit and ErrNotHeld where no Job Object holds it (all but Windows), and
// a missing Timeout with errTimeout, no ErrNotHeld. On Linux a TreeMemory refused by the cgroup check
// may name another cause in each, as Holds asks whether the parent holds a process where Run writes,
// and is then errNoTreeMemory in both. The TreeMemory row's result is logged.
//
// What it costs when this fails: a caller that asks Holds before a run is told a run holds that Run
// refuses, and learns it only after a whole run; or is refused one Run would run.
func TestHoldsAgreesWithRunOnThisSystem(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		set  func(*Cmd)
		want error // the refusal Holds gives on this system, or nil where it is decided by the host
	}{
		{"a plain run", func(*Cmd) {}, nil},
		{"a run that reads its peak", func(c *Cmd) { c.ReadPeak = true }, nil},
		{"a Grace of 1s", func(c *Cmd) { c.Grace = time.Second }, cond(!graceHeld(false), errNoGrace)},
		{"Processes 4", func(c *Cmd) { c.Limits = Limits{Processes: 4} }, cond(limitsHere != limJob, errNoProcessLimit)},
		{"TreeMemory 256 MiB and ReadPeak", func(c *Cmd) { c.Limits, c.ReadPeak = Limits{TreeMemory: 256 << 20}, true }, cond(limitsHere == limNone, errNoTreeMemory)},
		{"no Timeout", func(c *Cmd) { c.Timeout = 0 }, errTimeout},
	} {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		r.set(&c)
		kind, held := Holds(c)
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: Holds started the program (%v), want nothing started", r.name, err)
		}
		if r.want != nil && !errors.Is(held, r.want) {
			t.Errorf("%s: Holds gave %v, want %v", r.name, held, r.want)
		}
		if r.want != nil && errors.Is(held, ErrNotHeld) != errors.Is(r.want, ErrNotHeld) {
			t.Errorf("%s: Holds gave %v, ErrNotHeld %v, want %v", r.name, held, errors.Is(held, ErrNotHeld), errors.Is(r.want, ErrNotHeld))
		}
		if c.Limits.TreeMemory != 0 {
			t.Logf("Holds of %s here: %q, %v", r.name, kind, held)
		}
		res, ran := Run(t.Context(), c)
		_, statErr := os.Stat(mark)
		switch {
		case held != nil:
			same := held.Error() == ran.Error() || (errors.Is(held, errNoTreeMemory) && errors.Is(ran, errNoTreeMemory))
			if !same || kind != PeakNone || statErr == nil {
				t.Errorf("%s: Holds gave %q, %v; Run gave %v and the program ran %v; want Run's refusal the same and nothing run", r.name, kind, held, ran, statErr == nil)
			}
		case errors.Is(ran, ErrRefused) || statErr != nil:
			t.Errorf("%s: Holds gave none; Run gave %v and the program ran %v (%v), want it run", r.name, ran, statErr == nil, statErr)
		case res.PeakRead && res.PeakKind != kind:
			t.Errorf("%s: Holds gave the kind %q; Run read %v of the kind %q", r.name, kind, res.PeakRead, res.PeakKind)
		}
	}
}

// TestANULInTheCmdIsRefusedAsItsOwn: a Cmd whose Path, an Arg, a variable of its Env or its Dir holds
// a NUL is refused by Holds and by Run with errNUL, ErrRefused and not ErrNotHeld or
// errors.ErrUnsupported, Code -1 and nothing started, held to nothing and Contained alike, on every
// system; where a File runs (Linux), so is one whose File's name holds a NUL, made with os.NewFile.
//
// What it costs when this fails: on Linux the start gives a NUL as EINVAL, which a Contained start
// reads as the host's, so a party the run contains that names an argument, a file name say, makes
// the caller read "this system cannot hold the run" and run it again without Contained.
func TestANULInTheCmdIsRefusedAsItsOwn(t *testing.T) {
	t.Parallel()
	type row struct {
		name string
		set  func(*Cmd)
	}
	rows := []row{
		{"Path", func(c *Cmd) { c.Path += "\x00x" }},
		{"an Arg", func(c *Cmd) { c.Args = append(c.Args, "a\x00b") }},
		{"a variable of Env", func(c *Cmd) { c.Env = append(c.Env, "PROC_NUL=a\x00b") }},
		{"Dir", func(c *Cmd) { c.Dir = t.TempDir() + "\x00x" }},
	}
	if fileRuns {
		rows = append(rows, row{"File's name", func(c *Cmd) { c.Path, c.File = "", nulNamedFile(t) }})
	}
	for _, r := range rows {
		for _, contained := range []bool{false, true} {
			mark := filepath.Join(t.TempDir(), "started")
			c := helperCmd(t, "touch", mark)
			r.set(&c)
			if contained {
				c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
			}
			_, held := Holds(c)
			res, ran := Run(t.Context(), c)
			_, statErr := os.Stat(mark)
			for _, err := range []error{held, ran} {
				if !errors.Is(err, errNUL) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrNotHeld) || errors.Is(err, errors.ErrUnsupported) {
					t.Errorf("%s holding a NUL, Contained %v: %q, want errNUL and ErrRefused, not ErrNotHeld or errors.ErrUnsupported", r.name, contained, err)
				}
			}
			if res.Code != -1 || statErr == nil {
				t.Errorf("%s holding a NUL, Contained %v: Code %d, the program ran %v, want Code -1 and nothing run", r.name, contained, res.Code, statErr == nil)
			}
		}
	}
}

// cond is err where is holds, and nil otherwise.
func cond(is bool, err error) error {
	if is {
		return err
	}
	return nil
}
