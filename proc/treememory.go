package proc

import (
	"errors"
	"fmt"
)

// TreeMemoryHold is how this system holds c's Limits.TreeMemory.
type TreeMemoryHold int

// The answers of HoldsTreeMemory, a closed list.
const (
	TreeMemoryRefused     TreeMemoryHold = iota // neither held while the tree runs nor readable after it
	TreeMemoryHeld                              // Run holds the cap while the tree runs
	TreeMemoryJudgedAfter                       // the cap is refused, but a ReadPeak run reads a peak to judge after
)

// errTreeMemoryNone is HoldsTreeMemory of a Cmd with no TreeMemory, which asks for no cap to hold.
var errTreeMemoryNone = fmt.Errorf("%w: TreeMemory is 0, which asks for no cap, so there is none to hold", ErrRefused)

// HoldsTreeMemory is how this system holds c's Limits.TreeMemory here and now, from two of Holds'
// answers, and the PeakKind a run that holds it or judges it after reads.
//
// It is TreeMemoryHeld where Holds of c with ReadPeak set gives no refusal, with the kind Holds gave.
// It is TreeMemoryJudgedAfter where that refusal wraps ErrNotHeld and Holds of c with TreeMemory 0
// and ReadPeak set gives no refusal and a kind other than PeakNone, which is the kind given: a
// tree's charge (Linux) or one process's peak (darwin, the BSDs), for the caller to judge the cap by
// once the run has ended. It is TreeMemoryRefused, with PeakNone and no error, where both refuse,
// or the second gives PeakNone. A refusal from the first that does not wrap ErrNotHeld is given as it
// is, as Run would give it, with TreeMemoryRefused and PeakNone, and so is a c with TreeMemory 0,
// with a refusal that wraps ErrRefused and not ErrNotHeld.
//
// Every other field of c is kept as given, Contained among them, which asks for the cap held while
// the tree runs: a Contained c whose cap is not held is TreeMemoryRefused, never
// TreeMemoryJudgedAfter, as a peak read after the run contains nothing. Another field this system
// cannot hold, a Grace say, refuses both calls as well, and is TreeMemoryRefused too.
//
// Holds' caveat holds here: a TreeMemoryHeld or a TreeMemoryJudgedAfter does not promise a run.
func HoldsTreeMemory(c Cmd) (TreeMemoryHold, PeakKind, error) {
	return holdsTreeMemory(c, Holds)
}

// holdsTreeMemory is HoldsTreeMemory of c, asking holds where it asks Holds.
func holdsTreeMemory(c Cmd, holds func(Cmd) (PeakKind, error)) (TreeMemoryHold, PeakKind, error) {
	if c.Limits.TreeMemory == 0 {
		return TreeMemoryRefused, PeakNone, errTreeMemoryNone
	}
	c.ReadPeak = true
	kind, err := holds(c)
	switch {
	case err == nil:
		return TreeMemoryHeld, kind, nil
	case !errors.Is(err, errNotHeld):
		return TreeMemoryRefused, PeakNone, err
	}
	c.Limits.TreeMemory = 0
	if kind, err = holds(c); err != nil || kind == PeakNone {
		return TreeMemoryRefused, PeakNone, nil
	}
	return TreeMemoryJudgedAfter, kind, nil
}
