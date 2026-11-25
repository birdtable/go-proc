//go:build windows

package proc

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a pipe's ends are kept from inheritance by SetHandleInformation, a call x/sys alone has
)

// pipe is os.Pipe with neither end inheritable: exec hands the program inheritable copies of its
// own, so a process started another way during the run, every inheritable handle handed on, holds
// no end of it.
func pipe() (r, w *os.File, err error) {
	if r, w, err = os.Pipe(); err != nil {
		return nil, nil, err
	}
	for _, f := range []*os.File{r, w} {
		if err := windows.SetHandleInformation(windows.Handle(f.Fd()), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return nil, nil, errors.Join(err, r.Close(), w.Close())
		}
	}
	return r, w, nil
}

// kept is nothing on Windows: the run closes its own copies of the write ends once the program has
// them, and a pipe still being read at Drain's end is cut whoever holds it (unheldBounded).
type kept struct{}

// keep closes this process's copies of the write ends, which the program holds its own of.
func keep(w [2]*os.File) (*kept, error) {
	return &kept{}, errors.Join(w[0].Close(), w[1].Close())
}

// held says held: nothing asks here, where every pipe still being read at Drain's end is cut.
func (*kept) held(*os.File, int) bool { return true }

// unheldBounded is whether a pipe no process holds is cut at Drain's end too: on Windows it is. The
// read end is opened for synchronous reads, so PeekNamedPipe's count of what it holds waits behind
// the copy's pending ReadFile, as every query of that end does, and the copy cannot be stopped to
// read the count: os gives a pipe here no deadline. A zero Drain is DefaultDrain here too, so no
// run's read past the kill is cut at once.
const unheldBounded = true

// tail is never asked here (unheldBounded).
func tail(*os.File, io.Writer) error { return nil }

// let gives up nothing on Windows, which keeps no write end.
func (*kept) let() error { return nil }
