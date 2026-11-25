//go:build !unix && !windows

package proc

import (
	"os"
	"testing"
)

// sendsTerm, whether a run without Terminal is sent a signal it can catch before the kill, and
// boundsEveryRead are what this system does, as the tests know it apart from what Run reads
// (graceHeld, unheldBounded), so an answer wrong for its system is red.
const (
	sendsTerm       = false
	boundsEveryRead = false
)

// systemHelper says there is no helper mode of this system alone: it runs no program.
func systemHelper(string, []string) (int, bool) { return 0, false }

// emptyEnvChild is the whole environment a child gets from a run whose list is empty: none.
func emptyEnvChild() []string { return []string{} }

// console is whether this process has a console: none known here.
func console() bool { return false }

// quiet starts a child as it is.
func quiet(*process) {}

// inherited is what a program is told of f, the first of its run's ExtraFiles: its descriptor, 3.
func inherited(_ testing.TB, _ *os.File) string { return "3" }
