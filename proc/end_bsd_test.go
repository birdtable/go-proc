//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proc

// processEnd gives nil once process pid, which need not be a child of this one, has ended, or why it
// cannot be watched: exited's kqueue NOTE_EXIT, which takes any process this one may see and reaps
// nothing. A pid the kqueue cannot find has ended already. Nothing is signalled.
func processEnd(pid int) (<-chan error, error) {
	end := make(chan error, 1)
	go func() { end <- exited(pid) }()
	return end, nil
}

// peakKindWant is what a peak read on the BSDs and darwin counts, one process's resident set, as the tests know it apart from
// the peakKind Run reads, so a kind wrong for its system is red.
const peakKindWant = PeakProcessResident
