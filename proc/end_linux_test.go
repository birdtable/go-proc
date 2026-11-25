//go:build linux

package proc

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a process that is no child of this one is waited for by a pidfd, which syscall does not give
)

// processEnd gives nil once process pid, which need not be a child of this one, has ended, or why it
// cannot be watched: a pidfd opened now, which names that process and no later one with its pid,
// polled until it is readable. A pid no process has has ended already. Nothing is signalled.
//
// A process reaped while its pid is still a group's or a session's, as when a member is a zombie no
// init has reaped yet, is ESRCH from Linux 6.16 on and EINVAL before it: with a pid
// above 0 and no flags, pidfd_open's EINVAL means only that no process has that pid, so it has ended
// too. The clause is reached only where the zombie stays: in a pid namespace whose init reaps
// nothing, as `go` was pid 1 in a container. Under tini, as `task namespace` runs,
// or on a host whose init reaps, the zombie is gone before the open and the pid is ESRCH on any
// kernel, so a run there without the clause stays green (linux-amd64, Linux 6.12).
func processEnd(pid int) (<-chan error, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("process %d is no process to watch", pid)
	}
	end := make(chan error, 1)
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EINVAL) {
		end <- nil
		return end, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening a pidfd of process %d: %w", pid, err)
	}
	go func() {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			_, err := unix.Poll(fds, -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				err = fmt.Errorf("polling the pidfd of process %d: %w", pid, err)
			}
			end <- errors.Join(err, unix.Close(fd))
			return
		}
	}()
	return end, nil
}

// peakKindWant is what a peak read on Linux counts, the whole tree's cgroup charge, as the tests know it apart from
// the peakKind Run reads, so a kind wrong for its system is red.
const peakKindWant = PeakTreeCharged
