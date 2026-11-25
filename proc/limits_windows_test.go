//go:build windows

package proc

import (
	"bytes"
	"errors"
	"testing"
)

// TestAJobHeldToTreeMemoryEndsTheRunWithWhyLimit: a helper that allocates 1 GiB, held to 256 MiB, ends
// with WhyLimit and Code -1, before it writes done. The cap leaves room for a binary built with
// -race, whose shadow memory is committed as it is used.
//
// What it costs when this fails: a run held to TreeMemory allocates past it, or passes it and reads as
// a program that crashed by itself.
func TestAJobHeldToTreeMemoryEndsTheRunWithWhyLimit(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "hog", "1024")
	c.Limits = Limits{TreeMemory: 256 << 20}
	res, err := Run(t.Context(), c)
	if !endedWith(err, WhyLimit) || res.Code != -1 || bytes.Contains(res.Stdout, []byte("done")) {
		t.Errorf("a hog held to 256 MiB: %v, Code %d, stdout %q, stderr %q, want WhyLimit, Code -1 and no done", err, res.Code, res.Stdout, res.Stderr)
	}
}

// TestAJobHeldToTwoProcessesStartsNoThird: a helper held to Processes 2 starts one holding helper,
// the second of the tree, and the next, a third, does not start: the run ends with WhyLimit and
// Code -1, and the helper never wrote that it started a second.
//
// What it costs when this fails: a run held to Processes starts more, or passes it unseen and holds
// what it started until its deadline.
func TestAJobHeldToTwoProcessesStartsNoThird(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "procs", "2")
	c.Limits = Limits{Processes: 2}
	res, err := Run(t.Context(), c)
	if !endedWith(err, WhyLimit) || res.Code != -1 || bytes.Contains(res.Stdout, []byte("started 2")) {
		t.Errorf("a tree held to 2 processes: %v, Code %d, stdout %q, stderr %q, want WhyLimit, Code -1 and no started 2", err, res.Code, res.Stdout, res.Stderr)
	}
}

// TestAContainedRunIsRefusedOnWindows: a helper run Contained, held to 1 GiB, is refused with
// errNoContained, ErrRefused and errors.ErrUnsupported, Code -1, and writes nothing, as it never
// starts.
//
// What it costs when this fails: a caller told its tree is held against its own writes runs a tree
// that may duplicate the job's handle from the caller and clear the limit, its run none the wiser.
func TestAContainedRunIsRefusedOnWindows(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "write", "5", "0", "exit")
	c.Limits = Limits{TreeMemory: 1 << 30, Contained: true}
	res, err := Run(t.Context(), c)
	if !errors.Is(err, errNoContained) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 || len(res.Stdout) != 0 {
		t.Errorf("a Contained helper: %v, Code %d, stdout %q, stderr %q, want errNoContained, ErrRefused and ErrUnsupported, Code -1 and no output", err, res.Code, res.Stdout, res.Stderr)
	}
}

// TestAJobHeldToLimitsItKeepsWithinRunsAsItWould: a helper held to TreeMemory and Processes it stays
// within exits 0 with its output, and no WhyLimit.
//
// What it costs when this fails: a limit's watch ends a run that kept to it, or the run's output or
// exit is lost to the watch.
func TestAJobHeldToLimitsItKeepsWithinRunsAsItWould(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "write", "5", "0", "exit")
	c.Limits = Limits{TreeMemory: 1 << 30, Processes: 4}
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || string(res.Stdout) != "ooooo" {
		t.Errorf("a helper within its limits: %v, Code %d, stdout %q, stderr %q, want exit 0 and ooooo", err, res.Code, res.Stdout, res.Stderr)
	}
}

// TestAJobWithoutTerminalHoldsTheProgramToItsLimitsNotItsConsoleHost: a run without Terminal gives the program
// a console of its own, whose host process Windows starts. A helper without Terminal held to
// Processes 1, itself alone, exits 0 with its output and no WhyLimit, so the host does not take
// the one place; and one held to Processes 2 starts a second, and not a third, as without it.
//
// What it costs when this fails: a run without Terminal and a process limit ends with WhyLimit
// before its program started anything, as its console's host took a place; or the host's place
// lets the program start one more process than its limit.
func TestAJobWithoutTerminalHoldsTheProgramToItsLimitsNotItsConsoleHost(t *testing.T) {
	t.Parallel()
	t.Run("Processes 1", func(t *testing.T) {
		t.Parallel()
		c := helperCmd(t, "write", "5", "0", "exit")
		c.Limits = Limits{Processes: 1}
		res, err := Run(t.Context(), c)
		if err != nil || res.Code != 0 || string(res.Stdout) != "ooooo" {
			t.Errorf("a helper without Terminal held to 1 process: %v, Code %d, stdout %q, stderr %q, want exit 0 and ooooo", err, res.Code, res.Stdout, res.Stderr)
		}
	})
	t.Run("Processes 2", func(t *testing.T) {
		t.Parallel()
		c := helperCmd(t, "procs", "2")
		c.Limits = Limits{Processes: 2}
		res, err := Run(t.Context(), c)
		if !endedWith(err, WhyLimit) || res.Code != -1 || !bytes.Contains(res.Stdout, []byte("started 1")) || bytes.Contains(res.Stdout, []byte("started 2")) {
			t.Errorf("a tree without Terminal held to 2 processes: %v, Code %d, stdout %q, stderr %q, want WhyLimit, Code -1, started 1 and no started 2", err, res.Code, res.Stdout, res.Stderr)
		}
	})
}
