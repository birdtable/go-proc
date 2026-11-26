package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// loadChild is the first argument that makes the test binary the caller of RunHolding, in a process
// of its own, which the block is set on.
const loadChild = "sandbox-load-child"

// argvChild is the first argument that makes the test binary that caller with the program's own
// arguments after the report, every load folder kept.
const argvChild = "sandbox-argv-child"

// starter is the first argument that makes the test binary, or a copy of it, a program behind the
// block that starts the program its next argument names, with the rest as that one's, and exits
// with its exit, or starterRefused when it could not start it.
const starter = "sandbox-starter"

// starterRefused is the starter's exit when the program it was given could not start.
const starterRefused = 126

// childUsage and childFailed are LoadChild's exits when the program did not run: for ErrUsage, and
// for any other error.
const (
	childUsage  = 2
	childFailed = 3
)

// LoadChild is the test binary as that caller, which TestMain asks first with the process's
// arguments: loadChild, a program and a report. It holds the program to the program's own folder,
// read-only, and the report's, read-write, and runs it by RunHolding. With one argument more it
// runs it by RunHolding's own steps with that load folder left out, "-" for none, the plant.
// Either returns only when the program did not run: the error is written to the report, and the
// exit is childUsage for ErrUsage and childFailed for any other.
//
// With argvChild in place of loadChild, every argument after the report is the program's own, and
// no load folder is left out.
func LoadChild(args []string) (int, bool) {
	if len(args) >= 3 && args[1] == starter {
		return started(args[2:]), true
	}
	if len(args) < 4 || args[1] != argvChild && (len(args) > 5 || args[1] != loadChild) {
		return 0, false
	}
	prog, report := args[2], args[3]
	f := Folders{ReadWrite: []string{filepath.Dir(report)}, ReadOnly: []string{filepath.Dir(prog)}}
	if args[1] == argvChild {
		return answered(report, RunHolding(f, prog, args[4:], nil)), true
	}
	run := func() error { return RunHolding(f, prog, nil, nil) }
	if len(args) == 5 {
		run = func() error {
			argv, err := usage(prog, nil)
			if err != nil {
				return err
			}
			n, err := holding(f)
			if err != nil {
				return err
			}
			dirs, err := loadHere(prog, prog)
			if err != nil {
				return err
			}
			return runHere(n.with(slices.DeleteFunc(dirs, func(d string) bool { return d == args[4] })), prog, argv, nil)
		}
	}
	return answered(report, run()), true
}

// started is the exit of the program argv names, run with argv and the standard streams, or
// starterRefused, written to stdout with why, when it could not start.
func started(argv []string) int {
	files := []*os.File{os.NewFile(0, "stdin"), os.NewFile(1, "stdout"), os.NewFile(2, "stderr")}
	p, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Files: files}) //nolint:forbidigo // the starter is the program behind the block that starts another, the exec the block holds
	if err != nil {
		_, _ = fmt.Fprintf(files[1], "the starter could not start %q: %v\n", argv[0], err)
		return starterRefused
	}
	st, err := p.Wait()
	if err != nil {
		return starterRefused
	}
	return st.ExitCode()
}

// answered writes err, the error of a program that did not run, to the report, and is the child's
// exit for it.
func answered(report string, err error) int {
	_ = os.WriteFile(report, []byte(fmt.Sprint(err)), 0o600)
	if errors.Is(err, ErrUsage) {
		return childUsage
	}
	return childFailed
}
