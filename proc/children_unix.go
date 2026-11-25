//go:build unix

package proc

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the children's CPU time is read by getrusage(RUSAGE_CHILDREN), through x/sys
)

// childrenCPU is getrusage(RUSAGE_CHILDREN)'s user and system time.
func childrenCPU() (time.Duration, error) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &ru); err != nil {
		return 0, fmt.Errorf("proc: getrusage of this process's children: %w", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}
