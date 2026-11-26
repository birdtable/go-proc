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
func LoadChild(args []string) (int, bool) {
	if len(args) < 4 || len(args) > 5 || args[1] != loadChild {
		return 0, false
	}
	prog, report := args[2], args[3]
	f := Folders{ReadWrite: []string{filepath.Dir(report)}, ReadOnly: []string{filepath.Dir(prog)}}
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
	err := run()
	_ = os.WriteFile(report, []byte(fmt.Sprint(err)), 0o600)
	if errors.Is(err, ErrUsage) {
		return childUsage, true
	}
	return childFailed, true
}
