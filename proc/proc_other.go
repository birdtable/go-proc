//go:build !unix && !windows

package proc

import (
	"os"
	"time"
)

// A group is nothing here: grouped refuses, and nothing is started.
type group struct{}

// grouped refuses: there is no tree to kill here.
func grouped(*process, time.Duration, time.Duration, Limits, func(), bool) (*group, error) {
	return nil, errNoTree
}

func (*group) start() error                 { return errNoTree }
func (*group) wait() (killed, waited error) { return nil, nil }
func (*group) stop() error                  { return nil }
func (*group) wasCut() bool                 { return false }
func (*group) limited() bool                { return false }
func (*group) inCgroup(*runCgroup) error    { return nil }
func (*group) leftOver() bool               { return true }

// bareNames are the names Bare gives a child here: PATH, LANG and HOME.
func bareNames() []string { return []string{"PATH", "LANG", "HOME"} }

// systemNames are the names a child here needs to start, or to find the system's own files, beyond
// PATH, LANG and the home names: none.
func systemNames() []string { return nil }

// envKey is an environment name as this system compares it: as written.
func envKey(name string) string { return name }

// programsOnly is whether Run starts only a program file, by its name: only on Windows.
const programsOnly = false

// graceHeld is whether a run's tree is sent a signal it can catch before the kill: none is here.
func graceHeld(bool) bool { return false }

// treeIDHere is how this system names a tree: it has none.
const treeIDHere = idNone

// detach starts nothing here: there is no tree to name.
func detach(*process, procRoot, string) (Tree, error) { return Tree{}, errNoTree }

// killer is nothing here: Stop stops nothing.
type killer struct{}

// ender is the killer Stop uses.
func ender() killer { return killer{} }

// place reads nothing here.
func place(procRoot) (string, error) { return "", nil }

// stopTree stops nothing here: there is no tree to name.
func stopTree(TreeID, killer) error { return errNoTree }

// stopAt stops nothing here.
func stopAt(TreeID, procRoot, string, killer) error { return errNoTree }

// interruptAt interrupts nothing here.
func interruptAt(TreeID, procRoot, string, killer) error { return errNoTree }

// isTerminal is false: Run refuses every run here, with errNoTree.
func isTerminal(*os.File) (bool, error) { return false, nil }

// brokenPipe is false: Run refuses every run here, with errNoTree, and copies nothing.
func brokenPipe(error) bool { return false }
