//go:build windows && (amd64 || arm64)

package proc

import (
	"testing"
	"unsafe"
)

// The sizes winnt.h gives the two structs on a 64-bit Windows, checked as the package compiles: a
// field added, dropped or of another size fails the build of the tests before any test runs.
var (
	_ [48]byte = [unsafe.Sizeof(jobAccounting{})]byte{}
	_ [16]byte = [unsafe.Sizeof(jobPidList{})]byte{}
)

// TestTheJobStructsHaveTheLayoutWinntGivesThemOn64BitWindows: JOBOBJECT_BASIC_ACCOUNTING_INFORMATION
// is four LARGE_INTEGERs and four DWORDs, 48 bytes, and JOBOBJECT_BASIC_PROCESS_ID_LIST is two DWORDs
// and a ULONG_PTR array, its first id at byte 8, on amd64 and on arm64, field by field.
//
// What it costs when this fails: Usage reads the CPU time or the live count from the wrong bytes,
// and Pids reads ids from where Windows put none, so a caller stops the wrong processes.
func TestTheJobStructsHaveTheLayoutWinntGivesThemOn64BitWindows(t *testing.T) {
	t.Parallel()
	var a jobAccounting
	var p jobPidList
	rows := []struct {
		field     string
		got, want uintptr
	}{
		{"jobAccounting size", unsafe.Sizeof(a), 48},
		{"TotalUserTime", unsafe.Offsetof(a.TotalUserTime), 0},
		{"TotalKernelTime", unsafe.Offsetof(a.TotalKernelTime), 8},
		{"ThisPeriodTotalUserTime", unsafe.Offsetof(a.ThisPeriodTotalUserTime), 16},
		{"ThisPeriodTotalKernelTime", unsafe.Offsetof(a.ThisPeriodTotalKernelTime), 24},
		{"TotalPageFaultCount", unsafe.Offsetof(a.TotalPageFaultCount), 32},
		{"TotalProcesses", unsafe.Offsetof(a.TotalProcesses), 36},
		{"ActiveProcesses", unsafe.Offsetof(a.ActiveProcesses), 40},
		{"TotalTerminatedProcesses", unsafe.Offsetof(a.TotalTerminatedProcesses), 44},
		{"jobPidList size", unsafe.Sizeof(p), 16},
		{"NumberOfAssignedProcesses", unsafe.Offsetof(p.NumberOfAssignedProcesses), 0},
		{"NumberOfProcessIDsInList", unsafe.Offsetof(p.NumberOfProcessIDsInList), 4},
		{"ProcessIDList", unsafe.Offsetof(p.ProcessIDList), 8},
		{"a ProcessIDList entry", unsafe.Sizeof(p.ProcessIDList[0]), 8},
	}
	for _, r := range rows {
		if r.got != r.want {
			t.Errorf("%s: %d bytes in, want %d", r.field, r.got, r.want)
		}
	}
	if len(rows) != 14 {
		t.Errorf("%d rows", len(rows))
	}
}
