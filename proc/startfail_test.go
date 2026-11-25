package proc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// errBadExeFormat is Windows's ERROR_BAD_EXE_FORMAT (193) and errExeMachineMismatch its
// ERROR_EXE_MACHINE_TYPE_MISMATCH (216), spelled by their numbers so this file builds on every
// system: x/sys/windows's names build on Windows alone. A non-PE file is 193 on a host that loads
// its own machine, and 216 on one that loads another machine's images through emulation (a host
// whose loader reads the bytes as an image for a machine it does not run natively); both are the
// start's own refusal to run the file.
var (
	errBadExeFormat       = syscall.Errno(193)
	errExeMachineMismatch = syscall.Errno(216)
)

// hostGOOS is the system this test runs on, as runtime.GOOS names it: the one place the test reads it.
func hostGOOS() string {
	return runtime.GOOS //nolint:forbidigo // arch.ports.1: a start's cause and the file name that reaches the start are the system's own
}

// TestAPlainRunWhoseProgramCannotStartAnswersTheStartsError: a plain Run, with no Limits and none
// Contained, whose program cannot start answers the start's own error, on every system: a path that is
// not there matches fs.ErrNotExist; a file the account cannot start matches fs.ErrPermission on Unix,
// where a file of mode 0600 holds a script, and ERROR_BAD_EXE_FORMAT or, on a host that emulates
// another machine, ERROR_EXE_MACHINE_TYPE_MISMATCH on Windows, where mode bits stop no exec and the
// file is a .exe that is no PE file. Each answer is not an Ended and not ErrRefused, its Code is -1
// and PeakRead is false.
//
// What it costs when this fails: a caller that tells a program that never started from one that ran
// and failed reads the wrong answer, an Ended or a refusal, and nothing holds that it does not.
func TestAPlainRunWhoseProgramCannotStartAnswersTheStartsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// On Windows Run reads a program's name before any start, and refuses one that is no .exe or .com
	// (ErrNotProgram, an ErrRefused), so each row names its file through FileName.
	goos := hostGOOS()
	absent := filepath.Join(dir, FileName(goos, "absent"))
	unstartable := filepath.Join(dir, FileName(goos, "unstartable"))
	mode, causes := os.FileMode(0o600), []error{fs.ErrPermission}
	body := []byte("#!/bin/sh\nexit 0\n")
	if goos == "windows" {
		mode, causes, body = 0o700, []error{errBadExeFormat, errExeMachineMismatch}, []byte("this is no PE file\n")
	}
	if err := writeHeld(unstartable, body, mode); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what, path string
		causes     []error
	}{
		{"a path that is not there", absent, []error{fs.ErrNotExist}},
		{"a file the account cannot start", unstartable, causes},
	} {
		c := helperCmd(t, "none")
		c.Path = tc.path
		res, err := Run(t.Context(), c)
		matched := false
		for _, cz := range tc.causes {
			if errors.Is(err, cz) {
				matched = true
			}
		}
		switch {
		case err == nil:
			t.Errorf("a plain Run of %s: no error, want the start's, matching %v", tc.what, tc.causes)
			continue
		case !matched:
			t.Errorf("a plain Run of %s: %v, want an error matching one of %v", tc.what, err, tc.causes)
		}
		if _, ok := errors.AsType[Ended](err); ok {
			t.Errorf("a plain Run of %s: %v is an Ended, want the start's own error", tc.what, err)
		}
		if errors.Is(err, ErrRefused) {
			t.Errorf("a plain Run of %s: %v is ErrRefused, want the start's own error: the run was allowed and the program did not start", tc.what, err)
		}
		if res.Code != -1 || res.PeakRead {
			t.Errorf("a plain Run of %s: Code %d, PeakRead %v, want Code -1 and no peak read", tc.what, res.Code, res.PeakRead)
		}
	}
}
