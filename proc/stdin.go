package proc

import (
	"errors"
	"io"
	"os"
	"sync"
)

// fed is a run's Stdin that is no file. Run gives the program the read end of a pipe of its own and
// copies the Stdin into the write end itself, as exec would, but stops waiting for the copy when the
// run ends: exec's wait for its own copy would hold the run past its Timeout for as long as a Read
// of the Stdin's did not return.
type fed struct {
	src    io.Reader
	r, w   *os.File
	copied chan error
	// shut closes w once, whether the copy or stop comes first, and shutErr is what that close gave.
	shut    sync.Once
	shutErr error
}

// noFile is whether in, a run's Stdin or a Detached's Output, is no file: nil, a nil *os.File, or a
// closed one, whose handle exec would hand on as it is: InvalidHandle on Windows, the caller's own
// process, and on Unix none, so the first files the program opened would be its streams. A file the
// caller closes while Run or Start starts the program is the caller's race, as it is with exec.
func noFile(in io.Reader) bool {
	f, file := in.(*os.File)
	if !file || f == nil {
		return in == nil || file // nil, or a nil *os.File
	}
	rc, err := f.SyscallConn() // a closed file's handle can no longer be reached
	return err != nil || rc.Control(func(uintptr) {}) != nil
}

// feed gives cmd its stdin: none, the system's null device, when in is no file (noFile); in itself
// when it is a file, which the program reads with no copy; and otherwise the read end of a pipe
// that Run copies in into. A nil fed is no copy.
func feed(cmd *process, in io.Reader) (*fed, error) {
	if noFile(in) {
		cmd.Stdin = nil
		return nil, nil
	}
	if f, file := in.(*os.File); file {
		cmd.Stdin = f
		return nil, nil
	}
	r, w, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = r
	return &fed{src: in, r: r, w: w, copied: make(chan error, 1)}, nil
}

// shutW closes the write end and keeps what the close gave. It runs once, by shut: the copy or stop,
// whichever is first, and a call made while it runs waits for it.
func (f *fed) shutW() { f.shutErr = f.w.Close() }

// closeW closes the write end, once, and gives what that close gave.
func (f *fed) closeW() error {
	f.shut.Do(f.shutW)
	return f.shutErr
}

// started closes this process's read end, which the program holds its own of, and starts the copy.
// Once the Stdin ends, the copy gives what it met and then closes the write end, so the program
// reads to its end: a run whose program read to the end and exited finds what the copy met.
func (f *fed) started() error {
	if f == nil {
		return nil
	}
	err := f.r.Close()
	f.r = nil
	go func() {
		_, err := io.Copy(f.w, f.src)
		f.copied <- err
		// What this close gives, stop gives: it closes through shut too, which waits for this one.
		f.shut.Do(f.shutW)
	}()
	return err
}

// stop ends the run's part in the copy, once the tree has ended and its output is drained, and
// gives what the copy met and what closing the write end gave. A copy the program's end cut short (a
// broken pipe), or this stop (a closed file), met nothing. A copy not yet ended is not waited for:
// stop closes the write end, so the copy's next Write fails and it writes nothing more, and a Read
// of the Stdin's that has not returned is the Stdin's to end.
func (f *fed) stop() error {
	if f == nil {
		return nil
	}
	var met error
	select {
	case err := <-f.copied:
		if !brokenPipe(err) && !errors.Is(err, os.ErrClosed) {
			met = err
		}
	default:
	}
	return errors.Join(met, f.closeW())
}

// close closes both ends, for a run whose program never started.
func (f *fed) close() error {
	if f == nil {
		return nil
	}
	return errors.Join(f.r.Close(), f.closeW())
}
