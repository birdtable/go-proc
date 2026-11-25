package proc

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: whether a child has exited is asked without waiting by waitid with WNOHANG, whose siginfo is in x/sys alone
)

// pPID is waitid's idtype for the one process its id names.
const pPID = 1

// exited waits until the process pid, a child of this one, has exited, and
// leaves it unreaped: waitid with WNOWAIT. Linux takes no siginfo to fill,
// and nothing here would read one.
func exited(pid int) error {
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), 0, syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		switch errno {
		case 0:
			return nil
		case syscall.EINTR:
		default:
			return fmt.Errorf("waiting for process %d to exit: %w", pid, errno)
		}
	}
}

// exitedNow is whether the process pid, a child of this one, has exited, asked at once and leaving
// it unreaped: waitid with WNOHANG and WNOWAIT, whose siginfo has a signal only for a child that
// has exited.
func exitedNow(pid int) (bool, error) {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		switch {
		case errors.Is(err, unix.EINTR):
		case err != nil:
			return false, fmt.Errorf("asking whether process %d has exited: %w", pid, err)
		default:
			return info.Signo != 0, nil
		}
	}
}
