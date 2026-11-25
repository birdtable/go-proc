//go:build unix

package proc

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: whether a pipe's write end is open is read by poll, whose POLLHUP is in x/sys alone, and what the pipe holds by FIONREAD, whose ioctl is in x/sys alone
)

// pipe is os.Pipe, whose ends are close-on-exec: exec hands the program copies of its own.
func pipe() (r, w *os.File, err error) { return os.Pipe() }

// kept is nothing on Unix but a test's seam: the read end of a pipe says by itself whether a write
// end is open now, so the run closes its own copies of the write ends once the program has them.
type kept struct {
	// asked, in a test of the package, runs once held has asked about pipe i, before it answers.
	asked func(i int)
}

// keep closes this process's copies of the write ends, which the program holds its own of.
func keep(w [2]*os.File) (*kept, error) {
	return &kept{}, errors.Join(w[0].Close(), w[1].Close())
}

// held is whether a process may still hold a write end of the pipe whose read end is r: poll does
// not say POLLHUP, which it says of a pipe once every write end is closed, buffered bytes or not.
// It says so of that instant alone: on Linux a process of the same user can open a write end again
// through /proc/<pid>/fd, so no read rests on it past the asking (tail). A poll that fails says
// held, so the pipe is cut. The poll asks through the file's own descriptor, as Fd would not: Fd
// puts the file in blocking mode, and the copy's pending Read would then hold a thread for as long
// as it waits.
func (k *kept) held(r *os.File, i int) bool {
	held := true
	if raw, err := r.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) { //nolint:errcheck // a Control that fails leaves held true
			p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // G115: a descriptor os gave is an int, which poll keeps as an int32
			if n, err := unix.Poll(p, 0); err == nil && n == 1 {
				held = p[0].Revents&unix.POLLHUP == 0
			}
		})
	}
	if k.asked != nil {
		k.asked(i)
	}
	return held
}

// unheldBounded is whether a pipe no process holds is cut at Drain's end too: not on Unix, where
// it is read past Drain for what it held then, and no more (tail).
const unheldBounded = false

// tailChunk is the most tail reads at once.
const tailChunk = 4 << 10

// tail writes to w what the pipe whose read end is r holds now, by FIONREAD's count, and no more,
// once its copy has ended: a read that finds less than the count ends it, so a write end opened
// after the count adds nothing tail waits for. The descriptor is read as it is, with no poller,
// which would wait: the bytes counted are there to read.
func tail(r *os.File, w io.Writer) error {
	raw, err := r.SyscallConn()
	if err != nil {
		return err
	}
	var errs []error
	err = raw.Control(func(h uintptr) {
		fd := int(h)
		n, err := unix.IoctlGetInt(fd, fionread)
		if err != nil {
			errs = append(errs, err)
			return
		}
		buf := make([]byte, min(n, tailChunk))
		for n > 0 {
			got, err := unix.Read(fd, buf[:min(n, len(buf))])
			switch {
			case errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.EAGAIN):
				return
			case err != nil:
				errs = append(errs, err)
				return
			case got == 0:
				return
			}
			n -= got
			if _, err := w.Write(buf[:got]); err != nil {
				errs = append(errs, err)
				return
			}
		}
	})
	return errors.Join(append(errs, err)...)
}

// let gives up nothing on Unix, which keeps no write end.
func (*kept) let() error { return nil }
