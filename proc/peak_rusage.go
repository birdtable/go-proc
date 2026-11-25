//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proc

import (
	"math"
	"syscall"
)

// peakReads is whether this system reads a run's peak, and peakKind what it counts.
const (
	peakReads = true
	peakKind  = PeakProcessResident
)

// peakOf is the run's peak: the ru_maxrss its program's wait gave, the larger of the program's own
// and that of each descendant it waited for, scaled by maxrssUnit to bytes.
func peakOf(cmd *process, _ *group, _ *runCgroup, ran bool) (int64, bool) {
	if !ran || cmd.ProcessState == nil {
		return 0, false
	}
	ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0, false
	}
	return scaled(ru.Maxrss)
}

// scaled is maxrss in bytes, or none for one below 0 or past 63 bits so scaled. Rusage's field is 32
// bits wide on some systems, 64 on most.
func scaled[N int32 | int64](maxrss N) (int64, bool) {
	v := int64(maxrss)
	if v < 0 || v > math.MaxInt64/maxrssUnit {
		return 0, false
	}
	return v * maxrssUnit, true
}
