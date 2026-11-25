//go:build darwin || linux

package load

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestReadAgreesWithTheSystemsOwnFigure: each average Read gives lies within the system's own
// figure, sysctl's on macOS and /proc/loadavg's on Linux, read between two Reads, give or take the
// two decimals the system prints. The kernel moves its averages every five seconds, so a figure
// read across a move lies between the Read before it and the one after; a run slow enough to span
// two moves tries again, three times in all.
//
// What it costs when this fails: Read's figures are not the system's. A caller that holds its
// heavy work above a load holds it at the wrong load, or never.
func TestReadAgreesWithTheSystemsOwnFigure(t *testing.T) {
	t.Parallel()
	var miss string
	for range 3 {
		before := mustRead(t)
		sys := systemFigure(t)
		after := mustRead(t)
		miss = disagreement(before, sys, after)
		if miss == "" {
			return
		}
	}
	t.Fatal(miss)
}

// TestFortyFiveConcurrentReadersGetNoError: 45 goroutines that each Read 200 times, all at once,
// get no error and every Reading's CPUs is runtime.NumCPU(). The load is logged with the result:
// The row is meant for a load of 50 or more.
//
// What it costs when this fails: a program whose tests or workers read the load at once, as a
// test deadline scaled by load does, gets an error under the very load it reads for.
func TestFortyFiveConcurrentReadersGetNoError(t *testing.T) {
	t.Parallel()
	start := mustRead(t)
	var errs, reads atomic.Int64
	var first atomic.Value
	var wg sync.WaitGroup
	for range 45 {
		wg.Go(func() {
			for range 200 {
				r, err := Read()
				reads.Add(1)
				if err == nil && r.CPUs != runtime.NumCPU() {
					err = errors.New("a Reading's CPUs is not runtime.NumCPU()")
				}
				if err != nil {
					errs.Add(1)
					first.CompareAndSwap(nil, err)
				}
			}
		})
	}
	wg.Wait()
	t.Logf("45 readers, %d reads, %d errors, at load %.2f %.2f %.2f on %d CPUs", reads.Load(), errs.Load(), start.One, start.Five, start.Fifteen, start.CPUs)
	if errs.Load() != 0 {
		t.Fatalf("%d of %d reads failed, the first: %v", errs.Load(), reads.Load(), first.Load())
	}
}

func mustRead(t *testing.T) Reading {
	t.Helper()
	r, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return r
}

// disagreement says how sys, the system's figure printed to two decimals, lies outside the
// averages of the Reads before and after it; "" when it lies within them.
func disagreement(before Reading, sys [3]float64, after Reading) string {
	b := [3]float64{before.One, before.Five, before.Fifteen}
	a := [3]float64{after.One, after.Five, after.Fifteen}
	const tol = 0.01
	for i := range sys {
		lo, hi := min(b[i], a[i])-tol, max(b[i], a[i])+tol
		if sys[i] < lo || sys[i] > hi {
			return fmt.Sprintf("the system's average %d is %.2f, outside %.2f and %.2f, the Reads before and after it", i, sys[i], b[i], a[i])
		}
	}
	return ""
}
