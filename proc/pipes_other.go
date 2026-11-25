//go:build !unix && !windows

package proc

import (
	"errors"
	"io"
	"os"
)

// pipe is os.Pipe: no run starts here.
func pipe() (r, w *os.File, err error) { return os.Pipe() }

// kept is nothing here, where no run starts.
type kept struct{}

// keep closes this process's copies of the write ends.
func keep(w [2]*os.File) (*kept, error) {
	return &kept{}, errors.Join(w[0].Close(), w[1].Close())
}

// held says held: nothing here tells a held pipe, so the pipe is cut at Drain's end.
func (*kept) held(*os.File, int) bool { return true }

// unheldBounded is whether a pipe no process holds is cut at Drain's end too: every pipe here is
// held, and so cut.
const unheldBounded = false

// tail is never asked here, where every pipe is held.
func tail(*os.File, io.Writer) error { return nil }

// let gives up nothing.
func (*kept) let() error { return nil }
