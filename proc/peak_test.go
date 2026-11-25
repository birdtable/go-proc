package proc

import (
	"errors"
	"runtime"
	"testing"
)

// peakCmd is helperCmd that asks for its peak.
func peakCmd(t *testing.T, mode string, args ...string) Cmd {
	t.Helper()
	c := helperCmd(t, mode, args...)
	c.ReadPeak = true
	return c
}

// TestARunThatDoesNotAskReadsNoPeak: a run without ReadPeak whose program holds 32 MiB gives
// PeakRead false, Peak 0 and PeakKind PeakNone.
//
// What it costs when this fails: a caller that did not ask pays for a reading, on Linux a cgroup
// of the run's own, or reads a Peak it cannot tell from one read.
func TestARunThatDoesNotAskReadsNoPeak(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), helperCmd(t, "hog", "32"))
	if err != nil || res.Code != 0 {
		t.Fatalf("a hog of 32 MiB: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	if res.PeakRead || res.Peak != 0 || res.PeakKind != PeakNone {
		t.Errorf("a run that did not ask for its peak: Peak %d, PeakRead %v, PeakKind %q, want 0, false and none", res.Peak, res.PeakRead, res.PeakKind)
	}
}

// TestAPeakIsAtLeastWhatTheProgramHeld: a program that holds 64 MiB, each page written, reads a
// Peak of 64 MiB or more, in bytes, with PeakRead true and this system's PeakKind, where this
// system reads one; where it reads none, PeakRead is false, Peak 0 and PeakKind PeakNone.
//
// What it costs when this fails: a caller that sizes a limit, or a machine, by a run's peak sizes
// it below what the program held: by a reading in KiB taken for bytes, or a reading of something
// else; or it takes one process's peak for the whole tree's, or a resident set for a charge, and
// compares figures that do not compare.
func TestAPeakIsAtLeastWhatTheProgramHeld(t *testing.T) {
	t.Parallel()
	reads := peakHere(t)
	res, err := Run(t.Context(), peakCmd(t, "hog", "64"))
	if err != nil || res.Code != 0 {
		t.Fatalf("a hog of 64 MiB that asks for its peak: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	switch {
	case !reads && (res.PeakRead || res.Peak != 0 || res.PeakKind != PeakNone):
		t.Errorf("a run on a system that reads no peak: Peak %d, PeakRead %v, PeakKind %q, want 0, false and none", res.Peak, res.PeakRead, res.PeakKind)
	case reads && (!res.PeakRead || res.Peak < 64<<20 || res.PeakKind != peakKindWant):
		t.Errorf("a hog of 64 MiB: Peak %d bytes, PeakRead %v, PeakKind %q, want %d or more, true and %q", res.Peak, res.PeakRead, res.PeakKind, 64<<20, peakKindWant)
	}
}

// TestAPeakCountsADescendantTheProgramWaitedFor: a program that starts a hog of 64 MiB and waits
// for it, holding little itself, reads a Peak of 64 MiB or more, where this system reads one; where
// it reads none, PeakRead is false and Peak 0.
//
// What it costs when this fails: a program that does its work in a child, as a shell or a build
// driver does, reads the peak of the driver alone, and the caller sizes by a fraction of the run.
func TestAPeakCountsADescendantTheProgramWaitedFor(t *testing.T) {
	t.Parallel()
	reads := peakHere(t)
	res, err := Run(t.Context(), peakCmd(t, "hog-child", "64"))
	if err != nil || res.Code != 0 {
		t.Fatalf("a program whose child hogs 64 MiB: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	switch {
	case !reads && (res.PeakRead || res.Peak != 0):
		t.Errorf("a run on a system that reads no peak: Peak %d, PeakRead %v, want 0 and false", res.Peak, res.PeakRead)
	case reads && (!res.PeakRead || res.Peak < 64<<20):
		t.Errorf("a program whose child hogs 64 MiB: Peak %d bytes, PeakRead %v, want %d or more and true", res.Peak, res.PeakRead, 64<<20)
	}
}

// TestAPeakIsTheProgramsNotTheCallers: with this process holding 256 MiB, each page written, a
// program that holds little reads a Peak below those 256 MiB, where this system reads one; where it
// reads none, PeakRead is false and Peak 0. On Linux a program Go starts shares its starter's memory
// until its exec, which carries that memory's peak into the program's, so a reading of the program's
// own wait would be this process's 256 MiB or more. Other tests running beside it hold more of this
// process's memory, and so only raise what a wrong reading would read. The program is this test
// binary, and under -race it carries the race runtime's shadow: on windows-amd64 its committed peak
// read about 46 MiB without -race and 174 to 183 MiB with it, so the bar is the caller's 256 MiB,
// the line between the program's reading and the caller's, not a guess at the program's size.
//
// What it costs when this fails: a caller that holds much reads its own size as each program's
// peak, and sizes every limit and machine by itself.
func TestAPeakIsTheProgramsNotTheCallers(t *testing.T) {
	t.Parallel()
	reads := peakHere(t)
	held := hog(256)
	res, err := Run(t.Context(), peakCmd(t, "write", "0", "0", "exit"))
	runtime.KeepAlive(held)
	if err != nil || res.Code != 0 {
		t.Fatalf("a program that holds little: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	switch {
	case !reads && (res.PeakRead || res.Peak != 0):
		t.Errorf("a run on a system that reads no peak: Peak %d, PeakRead %v, want 0 and false", res.Peak, res.PeakRead)
	case reads && (!res.PeakRead || res.Peak >= 256<<20):
		t.Errorf("a program that holds little, started by a caller that holds 256 MiB: Peak %d bytes, PeakRead %v, want below %d and true", res.Peak, res.PeakRead, 256<<20)
	}
}

// TestAPeakRefusedOrNeverRunReadsNone: a run refused before it starts, for a Timeout of zero, gives
// PeakRead false and Peak 0 with ReadPeak, as does one whose program names no file.
//
// What it costs when this fails: a caller that reads Peak without PeakRead takes a run that never
// ran for one that held nothing.
func TestAPeakRefusedOrNeverRunReadsNone(t *testing.T) {
	t.Parallel()
	refused := peakCmd(t, "hold")
	refused.Timeout = 0
	missing := peakCmd(t, "hold")
	missing.Path, missing.Args = missing.Dir+"/no-such-program", nil
	for name, c := range map[string]Cmd{"refused": refused, "never run": missing} {
		res, err := Run(t.Context(), c)
		if err == nil {
			t.Errorf("a run %s: no error, want one", name)
		}
		if res.PeakRead || res.Peak != 0 {
			t.Errorf("a run %s (%v): Peak %d, PeakRead %v, want 0 and false", name, err, res.Peak, res.PeakRead)
		}
		if name == "refused" && !errors.Is(err, ErrRefused) {
			t.Errorf("a run refused: %v, want ErrRefused", err)
		}
	}
}
