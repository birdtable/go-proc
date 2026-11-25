//go:build !linux && !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package proc

import (
	"errors"
	"fmt"
)

// processEnd cannot watch a process here, so a holder that asks ends at once rather than outlive its
// test binary. No detached holder runs here: a tree cannot be named on these systems, so Start
// refuses before it starts one.
func processEnd(pid int) (<-chan error, error) {
	return nil, fmt.Errorf("process %d cannot be watched on this system: %w", pid, errors.ErrUnsupported)
}

// peakKindWant is what a peak read on a system that reads no peak counts, no reading, as the tests know it apart from
// the peakKind Run reads, so a kind wrong for its system is red.
const peakKindWant = PeakNone
