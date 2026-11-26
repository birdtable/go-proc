package proc

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the rows close a handle the helper put in this process, and start one as a process outside exec is started, by calls x/sys alone has
)

// TestAProcessOutsideTheJobThatHoldsTheOutputEndsTheRunWithWhyEscaped: a helper duplicates its
// stdout handle into its parent, this test binary, which no job of the run holds, writes the
// handle's value and exits 0. Its job leaves nothing, but a process outside it holds the pipe's
// write end, so the run reads it for its Drain, a second, and ends with Ended{WhyEscaped} and
// Code -1, keeping the value, before the safe default's 10 s are out.
//
// What it costs when this fails: a program that hands its output to a process outside its job, as
// any program of the same user can, holds the run open for as long as that process lives.
func TestAProcessOutsideTheJobThatHoldsTheOutputEndsTheRunWithWhyEscaped(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "dup-out")
	c.Drain = time.Second
	begun := time.Now()
	res, err := Run(t.Context(), c)
	took := time.Since(begun)
	if v, perr := strconv.ParseUint(strings.TrimSpace(string(res.Stdout)), 10, 64); perr == nil && v != 0 {
		// The helper's handle is this process's own now, and holds the pipe until it is closed.
		t.Cleanup(func() { _ = windows.CloseHandle(windows.Handle(v)) })
	} else {
		t.Errorf("the dup-out helper wrote %q and %q, want the handle it put in this process", res.Stdout, res.Stderr)
	}
	if !endedWith(err, WhyEscaped) || res.Code != -1 {
		t.Errorf("a run whose output a process outside its job holds: %v, Code %d, want Ended{%s} and Code -1", err, res.Code, WhyEscaped)
	}
	if took >= drainWait {
		t.Errorf("a run with a Drain of %v whose output a process outside its job holds returned %v after it began, want it ended by its Drain", c.Drain, took)
	}
}

// TestAProcessStartedWithEveryInheritableHandleDuringARunHoldsNoneOfItsPipes: once a run's program
// has started, as the run first reads its Stdin, this test binary starts a holding helper as a
// process is started outside exec, by CreateProcess with every inheritable handle of this process
// handed on, and the helper outlives the program. It holds none of the run's pipes: the program
// reads its stdin to the end and exits 0, and the run ends with its whole output, x, and neither
// WhyEscaped nor WhyDeadline.
//
// What it costs when this fails: a process any code of the caller's starts while a run is under way
// holds the run's output open, so the run ends WhyEscaped with its output cut, or its stdin, so a
// program that reads it to its end runs until its Timeout.
func TestAProcessStartedWithEveryInheritableHandleDuringARunHoldsNoneOfItsPipes(t *testing.T) {
	t.Parallel()
	in := &startsOnRead{t: t, started: make(chan error, 1)}
	c := helperCmd(t, "stdin")
	c.Stdin, c.Drain, c.Timeout = in, time.Second, time.Minute
	res, err := Run(t.Context(), c)
	select {
	case serr := <-in.started:
		if serr != nil {
			t.Fatalf("the holding helper did not start: %v", serr)
		}
	default:
		t.Fatalf("the run never read its Stdin: %v, wrote %q and %q", err, res.Stdout, res.Stderr)
	}
	if err != nil || res.Code != 0 || string(res.Stdout) != "x" {
		t.Errorf("a run during which a process started with every inheritable handle: %v, Code %d, wrote %q and %q, want Code 0 and x", err, res.Code, res.Stdout, res.Stderr)
	}
}

// startsOnRead is a Stdin whose first Read starts a holding helper with every inheritable handle of
// this process handed on, gives what that start met on started, and gives x; its next is the end.
type startsOnRead struct {
	t       *testing.T
	read    bool
	started chan error
}

func (s *startsOnRead) Read(p []byte) (int, error) {
	if s.read {
		return 0, io.EOF
	}
	s.read = true
	s.started <- inheritingAll(s.t)
	return copy(p, "x"), nil
}

// inheritingAll starts the hold helper by CreateProcess with every inheritable handle of this
// process handed on, as a process started outside exec is, and ends it with the test.
func inheritingAll(t *testing.T) error {
	t.Helper()
	process, _, err := startInheritingAll("hold")
	if err != nil {
		return err
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(process)
	})
	return nil
}

// startInheritingAll starts this test binary as the helper of the mode and args by CreateProcess
// with every inheritable handle of this process handed on, and gives its process handle, which the
// caller ends and closes, and its pid.
func startInheritingAll(mode string, args ...string) (windows.Handle, int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, 0, err
	}
	words := []string{windows.EscapeArg(exe), windows.EscapeArg(helperMode + mode)}
	for _, a := range args {
		words = append(words, windows.EscapeArg(a))
	}
	line, err := windows.UTF16PtrFromString(strings.Join(words, " "))
	if err != nil {
		return 0, 0, err
	}
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, line, nil, nil, true, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
		return 0, 0, err
	}
	_ = windows.CloseHandle(pi.Thread)
	return pi.Process, int(pi.ProcessId), nil
}
