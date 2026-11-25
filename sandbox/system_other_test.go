//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"os"
	"testing"
)

// TestRunAndCheckRefuseWhereTheSystemGivesNoBlock: on Windows and the BSDs,
// Run of a program by its absolute path, RunHolding and Check, with folders and a program whose
// load folders no one tells here and with none, each refuse
// with ErrNoBlock and ErrNoSystem, which say this system gives no block at all,
// and match errors.ErrUnsupported, and never with
// ErrUsage: the program is one Run takes, and the block is what is missing.
// Check's Block is the zero one, nothing closed.
//
// What it costs when this fails: a caller on a system with no block is told its line is wrong, or runs
// the program with the network open.
func TestRunAndCheckRefuseWhereTheSystemGivesNoBlock(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := realTemp(t, "rw")
	b, checked := Check(Folders{}, self)
	held, checkedHolding := Check(Folders{ReadWrite: []string{dir}}, self)
	for claim, err := range map[string]error{
		"Run":                Run(self, []string{"-test.run=^$"}, nil),
		"RunHolding":         RunHolding(Folders{ReadWrite: []string{dir}}, self, []string{"-test.run=^$"}, nil),
		"Check":              checked,
		"Check with folders": checkedHolding,
	} {
		if !errors.Is(err, ErrNoBlock) || !errors.Is(err, ErrNoSystem) || !errors.Is(err, errors.ErrUnsupported) || errors.Is(err, ErrUsage) {
			t.Errorf("%s: %v, want ErrNoBlock, ErrNoSystem and errors.ErrUnsupported, never ErrUsage", claim, err)
		}
	}
	if b != (Block{}) || held != (Block{}) {
		t.Errorf("Check: Block %+v, and %+v with folders, want nothing closed", b, held)
	}
}
