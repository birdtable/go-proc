package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests make a named pipe under a pty's name by the calls Windows makes one with
)

// ptyLike makes a named pipe, \\.\pipe\<name>, that this test owns, and opens it: it gives the
// server end and the client end, both closed when the test ends.
func ptyLike(t *testing.T, name string) (server, client *os.File) {
	t.Helper()
	path := `\\.\pipe\` + name
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 512, 512, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	server = os.NewFile(uintptr(s), path)
	t.Cleanup(func() { _ = server.Close() })
	c, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	client = os.NewFile(uintptr(c), path)
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

// TestARunWithoutTerminalRefusesAnMSYSOrCygwinPtysPipe: the test makes named pipes under the names
// an MSYS or a Cygwin pty gives its pipes, as mintty with pseudo consoles off gives a native
// program in place of a console. Either end of one, as Stdin without Terminal, is refused with
// errTerminalStdin, which wraps ErrRefused, and Code -1, and the helper that would make a file
// makes none. The same pipe with Terminal runs, and so does a pipe under another name without it.
//
// What it costs when this fails: a program the caller asked to run with no terminal of its
// caller's, started from Git Bash or another mintty, asks the caller's user at their terminal; or a
// run without Terminal and a pipe that is no pty is refused.
func TestARunWithoutTerminalRefusesAnMSYSOrCygwinPtysPipe(t *testing.T) {
	t.Parallel()
	id := fmt.Sprintf("goproc%d", os.Getpid())
	msysServer, msysClient := ptyLike(t, "msys-"+id+"-pty0-from-master")
	_, cygwinClient := ptyLike(t, "cygwin-"+id+"-pty1-to-master")
	_, otherClient := ptyLike(t, "go-proc-test-"+id+"-pty2-from-master")
	for _, row := range []struct {
		name       string
		stdin      *os.File
		noTerminal bool
		refused    bool
	}{
		{"an MSYS pty's pipe, its server end, without Terminal", msysServer, true, true},
		{"an MSYS pty's pipe, its client end, without Terminal", msysClient, true, true},
		{"a Cygwin pty's pipe, without Terminal", cygwinClient, true, true},
		{"an MSYS pty's pipe, with Terminal", msysClient, false, false},
		{"a pipe under another name, without Terminal", otherClient, true, false},
	} {
		path := filepath.Join(t.TempDir(), "made")
		c := helperCmd(t, "touch", path)
		c.Stdin, c.Terminal = row.stdin, !row.noTerminal
		res, err := Run(t.Context(), c)
		_, statErr := os.Stat(path)
		switch {
		case row.refused && (!errors.Is(err, errTerminalStdin) || !errors.Is(err, ErrRefused) || res.Code != -1 || statErr == nil):
			t.Errorf("%s: %v, Code %d, file %v, want errTerminalStdin, ErrRefused, Code -1 and no file", row.name, err, res.Code, statErr)
		case !row.refused && (err != nil || res.Code != 0 || statErr != nil):
			t.Errorf("%s: %v, Code %d, file %v, stderr %q, want a run that made its file", row.name, err, res.Code, statErr, res.Stderr)
		}
	}
}
