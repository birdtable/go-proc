//go:build unix

package proc

import (
	"os"
	"testing"
)

// TestANilFileStdinGivesTheProgramTheNullDevice: a run whose Stdin is a nil *os.File, an io.Reader
// that is not nil, gives the program the null device on fd 0, as a nil Stdin does. The program is
// the shell, not a helper: a Go program that starts with fd 0 closed opens the null device there
// itself, before main, so a helper would read the same either way.
//
// What it costs when this fails: exec hands the nil file on as no descriptor, so the program starts
// with fd 0 closed, and the first file it opens, a key or a socket, is its stdin, which a program it
// starts reads as its own.
func TestANilFileStdinGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	var f *os.File
	c := Cmd{
		Path: "/bin/sh", Args: []string{"-c", "if [ /dev/fd/0 -ef /dev/null ]; then echo null; else echo other; fi"},
		Dir: t.TempDir(), Timeout: longEnough, MaxOutput: 1 << 10, Drain: drainWait, Stdin: f,
	}
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || string(res.Stdout) != "null\n" {
		t.Errorf("a run with a nil *os.File Stdin: %v, Code %d, wrote %q and %q, want fd 0 on the null device", err, res.Code, res.Stdout, res.Stderr)
	}
}

// TestAClosedFileStdinGivesTheProgramTheNullDevice: a run whose Stdin is an *os.File already
// closed gives the program the null device on fd 0, as a nil Stdin does.
//
// What it costs when this fails: a closed file has the descriptor of a nil one, none, which exec
// hands on, so the program starts with fd 0 closed, and the first file it opens, a key or a socket,
// is its stdin, which a program it starts reads as its own.
func TestAClosedFileStdinGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	c := Cmd{
		Path: "/bin/sh", Args: []string{"-c", "if [ /dev/fd/0 -ef /dev/null ]; then echo null; else echo other; fi"},
		Dir: t.TempDir(), Timeout: longEnough, MaxOutput: 1 << 10, Drain: drainWait, Stdin: f,
	}
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || string(res.Stdout) != "null\n" {
		t.Errorf("a run with a closed *os.File Stdin: %v, Code %d, wrote %q and %q, want fd 0 on the null device", err, res.Code, res.Stdout, res.Stderr)
	}
}
