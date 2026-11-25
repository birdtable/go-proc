//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proc

import (
	"errors"
	"fmt"
	"syscall"
)

// exited waits until the process pid, a child of this one, has exited, and
// leaves it unreaped: a kqueue's NOTE_EXIT, which reaps nothing. A child
// the kqueue cannot find has exited already, since only a reap removes it.
func exited(pid int) error {
	_, err := exitWithin(pid, nil)
	return err
}

// exitedNow is whether the process pid, a child of this one, has exited, asked at once and
// leaving it unreaped: exited's kqueue, read with a timeout of zero.
func exitedNow(pid int) (bool, error) { return exitWithin(pid, &syscall.Timespec{}) }

// exitWithin is whether the process pid has exited within timeout, nil for no bound.
func exitWithin(pid int, timeout *syscall.Timespec) (bool, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return false, fmt.Errorf("waiting for process %d to exit: %w", pid, err)
	}
	done, err := exitOf(kq, pid, timeout)
	return done, errors.Join(err, syscall.Close(kq))
}

// exitOf waits on the kqueue kq, for timeout at most, until the process pid has exited.
func exitOf(kq, pid int, timeout *syscall.Timespec) (bool, error) {
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	ev.Fflags = syscall.NOTE_EXIT
	var err error
	for {
		_, err = syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case errors.Is(err, syscall.ESRCH):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("waiting for process %d to exit: %w", pid, err)
	}
	got := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(kq, nil, got, timeout)
		switch {
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return false, fmt.Errorf("waiting for process %d to exit: %w", pid, err)
		case n == 1:
			return true, nil
		case timeout != nil:
			return false, nil
		}
	}
}
