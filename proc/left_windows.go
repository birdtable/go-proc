//go:build windows

package proc

import (
	"errors"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a job's processes, their parents and their images are read by calls in x/sys alone
)

// consoleHostWithin is how long jobLeft waits for the console hosts Windows started for a tree to
// end once the last process on their consoles has: a host ends within milliseconds of that, and this
// is that many times over, on a machine at any load. It is waited out only by a host that does not
// end, which leaves Left true.
const consoleHostWithin = 5 * time.Second

// consoleHostWithinMs is consoleHostWithin in the milliseconds WaitForMultipleObjects takes.
const consoleHostWithinMs = uint32(consoleHostWithin / time.Millisecond)

// jobLeft is whether job holds a live process once its leader, pid leader, has been waited for, as
// jobView.left reads it, by the job's own calls.
func jobLeft(job windows.Handle, leader int) bool {
	return jobView[windows.Handle]{
		live: func() (int, error) {
			u, err := accounted(job)
			return u.Live, err
		},
		listed:  func() ([]int, error) { return listed(job) },
		parents: parentsOf,
		host:    consoleHost,
		wait: func(hs []windows.Handle) error {
			_, err := windows.WaitForMultipleObjects(hs, true, consoleHostWithinMs)
			return err
		},
		release: closeAll,
	}.left(leader)
}

// closeAll closes each handle of hs.
func closeAll(hs []windows.Handle) error {
	var err error
	for _, h := range hs {
		err = errors.Join(err, windows.CloseHandle(h))
	}
	return err
}

// consoleHost opens process pid for its end when its image is the system's console host,
// conhost.exe of the system directory, by full path, compared as Windows compares paths, without
// case; any other is errNoConsoleHost, with nothing held open. A pid no process has is errGone:
// OpenProcess answers it ERROR_INVALID_PARAMETER. So is a process that has ended while another
// handle to it is held, as a console host is between its end and its last handle's close: its
// image cannot be read then (QueryFullProcessImageName answers ERROR_GEN_FAILURE), and its handle,
// signalled, says it has ended. Whether its job still counts it, the count read again decides.
//
// A process that has begun to exit, its exit code set but its handle not yet signalled, may answer
// the image query ERROR_ACCESS_DENIED for those milliseconds. It
// is opened for its end as a host is, so jobLeft waits for it and the count read again decides.
// The query's error alone decides nothing: one from a process whose exit code says it is still
// active leaves the process unnamed, and Left true.
func consoleHost(pid int) (windows.Handle, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return 0, err
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // G115: a pid Windows listed, far below 2^32
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, errors.Join(errGone, err)
	}
	if err != nil {
		return 0, err
	}
	var buf [windows.MAX_LONG_PATH]uint16
	n := uint32(len(buf))
	err = windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
	switch {
	case err != nil && hasEnded(h):
		err = errors.Join(errGone, err)
	case err != nil && exiting(h):
		return h, nil
	}
	if err == nil && !strings.EqualFold(windows.UTF16ToString(buf[:n]), sys+`\conhost.exe`) {
		err = errNoConsoleHost
	}
	if err != nil {
		return 0, errors.Join(err, windows.CloseHandle(h))
	}
	return h, nil
}

// hasEnded is whether the process h names has ended: its handle is signalled.
func hasEnded(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// stillActive is STILL_ACTIVE of minwinbase.h: the exit code GetExitCodeProcess gives a process
// that has not begun to exit.
const stillActive = 259

// exiting is whether the process h names has begun to exit, or has ended: its exit code is set. A
// code that cannot be read is no exit. A process that exits with code 259 reads as still active,
// which leaves it unnamed, and Left true.
func exiting(h windows.Handle) bool {
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code != stillActive
}

// parentsOf is the parent of each process, by its pid, as a snapshot of the system's processes
// gives it.
func parentsOf() (map[int]int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		parents[int(pe.ProcessID)] = int(pe.ParentProcessID)
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		err = nil
	}
	return parents, errors.Join(err, windows.CloseHandle(snap))
}
