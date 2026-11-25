//go:build unix

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the burn helper reads its own user and system time by getrusage(RUSAGE_SELF), as ChildrenCPU reads its children's
)

// burnFor is the CPU time, user and system, a burn helper takes before it ends: at least half of it
// in user code, and the rest in either, as the system charges it.
const burnFor = 200 * time.Millisecond

// childrenModes runs the helper modes of ChildrenCPU, and says whether mode is one:
//
//   - burn: takes CPU time, in user code by a counted loop and, while its system time is under half of
//     burnFor, in system calls, until getrusage(RUSAGE_SELF) gives it at least half of burnFor in user
//     time and burnFor in all, then writes "burnt ", its user time and its system time in
//     nanoseconds, and a newline, and exits 0. A Unix that does not charge reads of the zero device
//     as system time gives it its share in user time, where the loop then ends it.
//   - children-cpu: starts argv and waits for it, reads ChildrenCPU, starts burn and reads its line, reads ChildrenCPU while the
//     burn has not been waited for, waits for it and reads ChildrenCPU again; then writes "before ",
//     "running " and "after " and each figure in nanoseconds, or the read's error, quoted, each
//     ended by a newline, then the burn's line.
func childrenModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "burn" && len(args) == 0:
		return burn(), true
	case mode == "children-cpu" && len(args) == 0:
		return childrenCPURead(), true
	}
	return 0, false
}

// burn takes CPU time, as the burn mode says: user time by arithmetic, which it checks against its own
// CPU clock, and system time, where the system charges it, by reads of the system's zero device into a
// buffer of a MiB, which the kernel fills.
func burn() int {
	zero, err := os.Open("/dev/zero")
	if err != nil {
		return wrote(0, err)
	}
	defer func() { _ = zero.Close() }()
	buf := make([]byte, 1<<20)
	x := uint64(1)
	for {
		var ru unix.Rusage
		if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
			return wrote(0, err)
		}
		user, sys := ru.Utime.Nano(), ru.Stime.Nano()
		if user >= int64(burnFor/2) && user+sys >= int64(burnFor) {
			runtime.KeepAlive(x)
			return wrote(fmt.Fprintf(os.Stdout, "burnt %d %d\n", user, sys))
		}
		for range 1 << 16 {
			x = x*6364136223846793005 + 1442695040888963407
		}
		if sys < int64(burnFor/2) {
			if _, err := io.ReadFull(zero, buf); err != nil {
				return wrote(0, err)
			}
		}
	}
}

// childrenCPURead reads ChildrenCPU around a burn, as the children-cpu mode says.
func childrenCPURead() int {
	figure := func() string {
		d, err := ChildrenCPU()
		if err != nil {
			return strconv.Quote(err.Error())
		}
		return strconv.FormatInt(int64(d), 10)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	// On Linux the first start of a process makes package os check pidfd once, and the check clones
	// a child and waits for it, which ChildrenCPU then counts: a first child, started and waited for
	// before the first read, leaves no such child between the reads.
	first := &process{Path: exe, Args: []string{exe, helperMode + "argv"}}
	if err := startTestProcess(first); err != nil {
		return wrote(0, err)
	}
	if err := first.Wait(); err != nil {
		return wrote(0, err)
	}
	before := figure()
	child := &process{Path: exe, Args: []string{exe, helperMode + "burn"}, Stderr: os.Stderr}
	out, err := child.StdoutPipe()
	if err != nil {
		return wrote(0, err)
	}
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	line, readErr := bufio.NewReader(out).ReadString('\n')
	running := figure()
	if err := errors.Join(readErr, child.Wait()); err != nil {
		return wrote(0, err)
	}
	return wrote(fmt.Fprintf(os.Stdout, "before %s\nrunning %s\nafter %s\n%s", before, running, figure(), line))
}
