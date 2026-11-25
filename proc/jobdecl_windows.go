//go:build windows

package proc

// The three Job Object declarations x/sys lacks, each with its name in the Windows headers beside
// it. This file is deleted when x/sys gains them.

import (
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the Job Object calls are in x/sys alone
)

// openJobObject opens the Job Object named name with access: OpenJobObjectW of jobapi2.h. The handle
// is not inherited.
func openJobObject(access uint32, name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	// kernel32.dll is loaded from the system folder alone, as createJob loads it.
	r, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW").Call(uintptr(access), 0, uintptr(unsafe.Pointer(p))) //nolint:gosec // G103: the call takes the name Windows reads, by its address
	if r == 0 {
		return 0, callErr
	}
	return windows.Handle(r), nil
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION of winnt.h: what a job's processes have
// used, the times in 100-nanosecond units, those of processes that have ended included.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobPidList is JOBOBJECT_BASIC_PROCESS_ID_LIST of winnt.h: its header, and the first of the
// NumberOfProcessIDsInList ids that follow it in the buffer Windows fills. The fields are named as
// winnt.h names them, with ID for Id: NumberOfProcessIdsInList and ProcessIdList there.
type jobPidList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
	ProcessIDList             [1]uintptr
}
