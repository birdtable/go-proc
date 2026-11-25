//go:build windows

package proc

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a process is waited for by its handle, which the calls of x/sys alone open
)

// processEnd gives nil once process pid, which need not be a child of this one, has ended, or why it
// cannot be watched: a handle opened now, which names that process and no later one with its pid,
// waited on until it is signalled. A pid no process has has ended already. Nothing is ended.
func processEnd(pid int) (<-chan error, error) {
	end := make(chan error, 1)
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		end <- nil
		return end, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening process %d to wait for it: %w", pid, err)
	}
	go func() {
		event, err := windows.WaitForSingleObject(h, windows.INFINITE)
		if err == nil && event != windows.WAIT_OBJECT_0 {
			err = fmt.Errorf("the wait for process %d gave %#x", pid, event)
		}
		end <- errors.Join(err, windows.CloseHandle(h))
	}()
	return end, nil
}

// peakKindWant is what a peak read on Windows counts, one process's committed memory, as the tests know it apart from
// the peakKind Run reads, so a kind wrong for its system is red.
const peakKindWant = PeakProcessCommitted
