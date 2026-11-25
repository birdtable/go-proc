package proc

import (
	"math"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a Job Object's peak is read by the call that reads its limits, in x/sys alone
)

// peakReads is whether this system reads a run's peak, and peakKind what it counts.
const (
	peakReads = true
	peakKind  = PeakProcessCommitted
)

// jobPeak is the job's PeakProcessMemoryUsed: the most memory committed to any one process of it
// while it ran. A job that cannot be asked gives none.
func jobPeak(job windows.Handle) (uint64, bool) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call reads the struct Windows defines, by its address and size
		return 0, false
	}
	return uint64(info.PeakProcessMemoryUsed), true
}

// peakOf is the run's peak: what the job read as it ended, when the program ran.
func peakOf(_ *process, g *group, _ *runCgroup, ran bool) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !ran || !g.peakRead || g.peak > math.MaxInt64 {
		return 0, false
	}
	return int64(g.peak), true
}
