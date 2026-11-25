//go:build !linux && !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package proc

// peakReads is whether this system reads a run's peak, and peakKind what it counts: it reads none.
const (
	peakReads = false
	peakKind  = PeakNone
)

// peakOf is the run's peak: this system gives none.
func peakOf(*process, *group, *runCgroup, bool) (int64, bool) { return 0, false }
